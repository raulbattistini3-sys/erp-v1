// Package proxy answers configured GET routes from the store and forwards everything else, untouched,
// to the upstream (which stays the only owner of queries and of the rules about who may see what).
//
// A request is answered from the cache only when all of these hold:
//   - it is a GET on a configured route, without "Cache-Control: no-cache";
//   - the site's feed is up (or the site has none): while the cache can't hear about changes, it
//     can't prove an entry is current, so every request goes to the upstream ("x-cache: bypass");
//   - an entry for the same path, query and Vary headers (Authorization for the ERP: per user) is live.
//
// A POST on a configured dedupe path (telemetry envelopes) that resends a record the upstream already
// accepted is answered here ("x-cache: dedupe"), see dedupe.go.
//
// A miss goes to the upstream once per key, however many identical requests arrive meanwhile
// (singleflight). The response is stored if it is a 200 without Set-Cookie or "no-store", fits the
// size limits and none of its tables changed while it was fetched.
package proxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/raulbattistini3-sys/erp-cache/internal/config"
	"github.com/raulbattistini3-sys/erp-cache/internal/store"
)

// hop-by-hop and per-response headers that are never stored
var dropHeaders = []string{"Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade", "Set-Cookie", "Date", "Content-Length"}

type route struct {
	segs   []string
	rest   bool // ends in "*"
	ttl    time.Duration
	tables []string
}

func (r *route) match(path string) bool {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if r.rest {
		if len(parts) < len(r.segs) {
			return false
		}
	} else if len(parts) != len(r.segs) {
		return false
	}
	for i, s := range r.segs {
		if !strings.HasPrefix(s, ":") && s != parts[i] {
			return false
		}
		if strings.HasPrefix(s, ":") && parts[i] == "" {
			return false
		}
	}
	return true
}

type flight struct {
	done  chan struct{}
	entry *store.Entry // nil: not shareable, every waiter goes to the upstream itself
}

// Site is the handler of one upstream.
type Site struct {
	Name    string
	Store   *store.Store
	routes  []route
	dedupes []*dedupe
	vary    []string
	flushOn []string
	up      *url.URL
	client  *http.Client
	bypass  *httputil.ReverseProxy
	log     *slog.Logger

	feedUp   atomic.Bool
	bypassed atomic.Uint64
	upstream latency

	mu      sync.Mutex
	flights map[string]*flight
}

// New builds a site. Its feed is considered down until SetFeed(true) unless hasFeed is false.
func New(cfg config.Site, hasFeed bool, log *slog.Logger) (*Site, error) {
	up, err := url.Parse(cfg.Upstream)
	if err != nil {
		return nil, err
	}
	var tlsConfig *tls.Config
	if cfg.UpstreamCA != "" {
		pem, err := os.ReadFile(cfg.UpstreamCA)
		if err != nil {
			return nil, fmt.Errorf("site %s: upstream CA: %w", cfg.Name, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("site %s: no PEM certificate in %s", cfg.Name, cfg.UpstreamCA)
		}
		tlsConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	transport := &http.Transport{
		Proxy:               nil,
		TLSClientConfig:     tlsConfig,
		DialContext:         (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		MaxIdleConns:        256,
		MaxIdleConnsPerHost: 64,
		IdleConnTimeout:     90 * time.Second,
	}
	s := &Site{
		Name:    cfg.Name,
		Store:   store.New(cfg.MaxEntries, cfg.MaxBytes, cfg.MaxEntryBytes),
		flushOn: cfg.FlushOn,
		up:      up,
		client:  &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		log:     log.With("site", cfg.Name),
		flights: map[string]*flight{},
	}
	for _, h := range cfg.Vary {
		s.vary = append(s.vary, http.CanonicalHeaderKey(h))
	}
	for _, r := range cfg.Routes {
		p := strings.Trim(r.Path, "/")
		rt := route{ttl: r.TTL.D(), tables: append(append([]string{}, r.Tables...), cfg.FlushOn...)}
		if strings.HasSuffix(p, "*") {
			rt.rest = true
			p = strings.TrimSuffix(strings.TrimSuffix(p, "*"), "/")
		}
		if p != "" {
			rt.segs = strings.Split(p, "/")
		}
		s.routes = append(s.routes, rt)
	}
	for _, d := range cfg.Dedupe {
		s.dedupes = append(s.dedupes, newDedupe(d))
	}
	// everything that isn't cached, websockets and streams included, goes through as it came
	s.bypass = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(up)
			pr.SetXForwarded()
			pr.Out.Host = pr.In.Host
		},
		Transport:     transport,
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			s.log.Warn("upstream", "path", r.URL.Path, "err", err)
			http.Error(w, "upstream unavailable", http.StatusBadGateway)
		},
	}
	s.feedUp.Store(!hasFeed)
	return s, nil
}

// SetFeed is called by the feed: down forgets everything and bypasses until it is up again.
func (s *Site) SetFeed(up bool) {
	if !up {
		s.Store.ForgetAll()
	}
	s.feedUp.Store(up)
}

// FeedUp reports whether the site is answering from the cache.
func (s *Site) FeedUp() bool { return s.feedUp.Load() }

// Changed is called with the table named by a notification.
func (s *Site) Changed(table string) {
	for _, t := range s.flushOn {
		if t == table {
			s.Store.ForgetAll()
			s.log.Info("flushed", "table", table)
			return
		}
	}
	if n := s.Store.Invalidate(table); n > 0 {
		s.log.Debug("invalidated", "table", table, "entries", n)
	}
}

// Bypassed counts requests sent through without looking at the cache.
func (s *Site) Bypassed() uint64 { return s.bypassed.Load() }

// UpstreamLatency returns and resets the latency of upstream fetches made for misses.
func (s *Site) UpstreamLatency() LatencySummary { return s.upstream.take() }

func (s *Site) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if d := s.dedupeFor(r); d != nil {
		s.serveDedupe(w, r, d)
		return
	}
	rt := s.routeFor(r)
	if rt == nil || !s.feedUp.Load() || strings.Contains(strings.ToLower(r.Header.Get("Cache-Control")), "no-cache") {
		s.bypassed.Add(1)
		w.Header().Set("X-Cache", "bypass")
		s.bypass.ServeHTTP(w, r)
		return
	}
	t0 := time.Now()
	key := s.key(r)
	if e, ok := s.Store.Get(key); ok {
		writeEntry(w, e, "hit", time.Since(t0))
		return
	}

	s.mu.Lock()
	if f, ok := s.flights[key]; ok {
		s.mu.Unlock()
		select {
		case <-f.done:
		case <-r.Context().Done():
			return
		}
		if f.entry != nil {
			writeEntry(w, f.entry, "shared", time.Since(t0))
			return
		}
		s.fetch(w, r, rt, key, nil)
		return
	}
	f := &flight{done: make(chan struct{})}
	s.flights[key] = f
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		delete(s.flights, key)
		s.mu.Unlock()
		close(f.done)
	}()
	s.fetch(w, r, rt, key, f)
}

func (s *Site) routeFor(r *http.Request) *route {
	if r.Method != http.MethodGet || r.Header.Get("Upgrade") != "" {
		return nil
	}
	for i := range s.routes {
		if s.routes[i].match(r.URL.Path) {
			return &s.routes[i]
		}
	}
	return nil
}

// key: path, the query with its parameters sorted, Origin (CORS answers depend on it),
// Accept-Encoding, and the configured Vary headers (hashed: the key never holds a token).
func (s *Site) key(r *http.Request) string {
	q := r.URL.Query()
	names := make([]string, 0, len(q))
	for k := range q {
		names = append(names, k)
	}
	sort.Strings(names)
	var b strings.Builder
	b.WriteString(r.URL.Path)
	for _, k := range names {
		vs := q[k]
		sort.Strings(vs)
		for _, v := range vs {
			fmt.Fprintf(&b, "&%s=%s", url.QueryEscape(k), url.QueryEscape(v))
		}
	}
	h := sha256.New()
	for _, name := range append([]string{"Origin", "Accept-Encoding"}, s.vary...) {
		fmt.Fprintf(h, "%s:%s\n", name, strings.Join(r.Header.Values(name), ","))
	}
	b.WriteString("#")
	b.WriteString(hex.EncodeToString(h.Sum(nil)[:16]))
	return b.String()
}

// fetch goes to the upstream for a miss. With f (the leader of a flight), a cacheable response is
// stored and shared with the waiters.
func (s *Site) fetch(w http.ResponseWriter, r *http.Request, rt *route, key string, f *flight) {
	gen := s.Store.Gen(rt.tables)
	// the leader's client going away must not fail the waiters
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 60*time.Second)
	defer cancel()
	out, err := http.NewRequestWithContext(ctx, http.MethodGet, s.up.JoinPath(r.URL.Path).String(), nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	out.URL.RawQuery = r.URL.RawQuery
	out.Header = r.Header.Clone()
	for _, h := range dropHeaders {
		out.Header.Del(h)
	}
	out.Host = r.Host
	if ip, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		out.Header.Set("X-Forwarded-For", strings.TrimPrefix(r.Header.Get("X-Forwarded-For")+", "+ip, ", "))
	}
	out.Header.Set("X-Forwarded-Host", r.Host)

	t0 := time.Now()
	res, err := s.client.Do(out)
	if err != nil {
		s.log.Warn("upstream", "path", r.URL.Path, "err", err)
		http.Error(w, "upstream unavailable", http.StatusBadGateway)
		return
	}
	defer res.Body.Close()
	limit := s.Store.MaxEntryBytes()
	body, err := io.ReadAll(io.LimitReader(res.Body, int64(limit)+1))
	if err != nil {
		http.Error(w, "upstream read failed", http.StatusBadGateway)
		return
	}
	s.upstream.add(time.Since(t0))

	header := res.Header.Clone()
	for _, h := range dropHeaders { // Set-Cookie still goes to this client, just never into the store
		if h != "Set-Cookie" {
			header.Del(h)
		}
	}
	cacheable := res.StatusCode == http.StatusOK && len(body) <= limit && res.Header.Get("Set-Cookie") == "" &&
		!strings.Contains(strings.ToLower(res.Header.Get("Cache-Control")), "no-store")

	if !cacheable {
		copyHeader(w.Header(), header)
		w.Header().Set("X-Cache", "miss")
		w.WriteHeader(res.StatusCode)
		w.Write(body)
		io.Copy(w, res.Body) // the rest of a body too large to keep
		return
	}
	stored := header.Clone()
	stored.Del("Set-Cookie")
	s.Store.Put(key, res.StatusCode, stored, body, rt.tables, rt.ttl, gen)
	e := &store.Entry{Status: res.StatusCode, Header: stored, Body: body}
	if f != nil {
		f.entry = e
	}
	writeEntry(w, e, "miss", time.Since(t0))
}

func writeEntry(w http.ResponseWriter, e *store.Entry, how string, took time.Duration) {
	h := w.Header()
	copyHeader(h, e.Header)
	h.Set("X-Cache", how)
	h.Add("Server-Timing", fmt.Sprintf("cache;desc=%s;dur=%.3f", how, float64(took.Microseconds())/1000))
	if how != "miss" && !e.Stored.IsZero() {
		h.Set("Age", fmt.Sprint(int(time.Since(e.Stored).Seconds())))
	}
	h.Set("Content-Length", fmt.Sprint(len(e.Body)))
	w.WriteHeader(e.Status)
	io.Copy(w, bytes.NewReader(e.Body))
}

func copyHeader(dst, src http.Header) {
	for k, vs := range src {
		dst[k] = append([]string(nil), vs...)
	}
}

// ─── latency of the upstream fetches ──────────────────────────────────────────────────────────────

type latency struct {
	mu    sync.Mutex
	count int
	sum   time.Duration
	max   time.Duration
}

// LatencySummary is the upstream time of misses over a window.
type LatencySummary struct {
	Count  int
	MeanMs float64
	MaxMs  float64
}

func (l *latency) add(d time.Duration) {
	l.mu.Lock()
	l.count++
	l.sum += d
	if d > l.max {
		l.max = d
	}
	l.mu.Unlock()
}

func (l *latency) take() LatencySummary {
	l.mu.Lock()
	defer l.mu.Unlock()
	s := LatencySummary{Count: l.count, MaxMs: ms(l.max)}
	if l.count > 0 {
		s.MeanMs = ms(l.sum / time.Duration(l.count))
	}
	l.count, l.sum, l.max = 0, 0, 0
	return s
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }
