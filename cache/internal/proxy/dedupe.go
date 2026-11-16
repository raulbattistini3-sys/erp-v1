package proxy

import (
	"bytes"
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/raulbattistini3-sys/erp-cache/internal/config"
)

// a body this large is never parsed: it goes to the upstream as it came
const maxDedupeBody = 1 << 20

// dedupe answers resends of records the upstream already accepted (config.Dedupe).
//
// A telemetry sender keeps an envelope queued until its POST gets a 2xx and resends it, in order,
// on its next tick. When the upstream stored it but the answer was lost (a timeout, the sender
// restarting), the resend costs a request, a JSON parse and an INSERT ... ON CONFLICT DO UPDATE
// that only bumps a counter. Here it costs a map lookup.
//
// Only accepted records are remembered: a record whose POST failed, or that is still in flight,
// goes to the upstream again. The key includes the Authorization header (hashed), so one caller's
// accepted record never answers another caller.
type dedupe struct {
	path        string
	keys        []string
	invalidates []string
	max         int

	mu     sync.Mutex
	seen   map[string]*list.Element // → element whose Value is a seenRecord
	order  *list.List               // most recently seen first
	hits   atomic.Uint64
	misses atomic.Uint64
}

type seenRecord struct {
	key    string
	status int
}

func newDedupe(c config.Dedupe) *dedupe {
	return &dedupe{
		path:        strings.TrimRight(c.Path, "/"),
		keys:        c.Keys,
		invalidates: c.Invalidates,
		max:         c.MaxKeys,
		seen:        map[string]*list.Element{},
		order:       list.New(),
	}
}

// key of a body: the path, the raw JSON of each key field and the caller. false when the body is not
// a JSON object or lacks a key field (the upstream answers that one, with its own error).
func (d *dedupe) key(body []byte, auth string) (string, bool) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return "", false
	}
	var b strings.Builder
	b.WriteString(d.path)
	for _, k := range d.keys {
		v, ok := fields[k]
		if !ok || len(v) == 0 || string(v) == "null" {
			return "", false
		}
		b.WriteByte(0)
		b.Write(bytes.TrimSpace(v))
	}
	h := sha256.Sum256([]byte(auth))
	b.WriteByte(0)
	b.WriteString(hex.EncodeToString(h[:12]))
	return b.String(), true
}

func (d *dedupe) lookup(key string) (int, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	el, ok := d.seen[key]
	if !ok {
		return 0, false
	}
	d.order.MoveToFront(el)
	return el.Value.(seenRecord).status, true
}

func (d *dedupe) remember(key string, status int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if el, ok := d.seen[key]; ok {
		d.order.MoveToFront(el)
		return
	}
	d.seen[key] = d.order.PushFront(seenRecord{key, status})
	for d.order.Len() > d.max {
		oldest := d.order.Back()
		d.order.Remove(oldest)
		delete(d.seen, oldest.Value.(seenRecord).key)
	}
}

func (d *dedupe) forget() {
	d.mu.Lock()
	d.seen = map[string]*list.Element{}
	d.order.Init()
	d.mu.Unlock()
}

// DedupeStats are the numbers of one dedupe path, for /stats and the cache's own telemetry.
type DedupeStats struct {
	Path       string `json:"path"`
	Remembered int    `json:"remembered"`
	Answered   uint64 `json:"answered"`  // resends answered by the cache
	Forwarded  uint64 `json:"forwarded"` // records sent to the upstream
}

// Dedupes returns the numbers of every dedupe path of the site.
func (s *Site) Dedupes() []DedupeStats {
	out := make([]DedupeStats, 0, len(s.dedupes))
	for _, d := range s.dedupes {
		d.mu.Lock()
		n := d.order.Len()
		d.mu.Unlock()
		out = append(out, DedupeStats{Path: d.path, Remembered: n, Answered: d.hits.Load(), Forwarded: d.misses.Load()})
	}
	return out
}

// ForgetRecords drops every remembered record (POST /flush): their resends reach the upstream again.
func (s *Site) ForgetRecords() {
	for _, d := range s.dedupes {
		d.forget()
	}
}

func (s *Site) dedupeFor(r *http.Request) *dedupe {
	if r.Method != http.MethodPost {
		return nil
	}
	p := strings.TrimRight(r.URL.Path, "/")
	for _, d := range s.dedupes {
		if d.path == p {
			return d
		}
	}
	return nil
}

func (s *Site) serveDedupe(w http.ResponseWriter, r *http.Request, d *dedupe) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxDedupeBody+1))
	if err != nil {
		http.Error(w, "request read failed", http.StatusBadRequest)
		return
	}
	key, ok := "", false
	if len(body) <= maxDedupeBody {
		key, ok = d.key(body, r.Header.Get("Authorization"))
	}
	if ok {
		if status, seen := d.lookup(key); seen {
			d.hits.Add(1)
			w.Header().Set("X-Cache", "dedupe")
			w.WriteHeader(status)
			return
		}
	}
	d.misses.Add(1)
	// what was read goes first, then whatever a body too large to parse still has
	r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(body), r.Body))
	rec := &statusWriter{ResponseWriter: w}
	w.Header().Set("X-Cache", "miss")
	s.bypass.ServeHTTP(rec, r)
	if ok && rec.status >= 200 && rec.status < 300 {
		d.remember(key, rec.status)
		for _, t := range d.invalidates {
			s.Store.Invalidate(t)
		}
	}
}

// statusWriter records the status the upstream answered with.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
