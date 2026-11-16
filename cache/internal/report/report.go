// Package report sends the cache's own numbers to the TCC telemetry server as HostTelemetryEnvelopes
// (POST /telemetry/host, see @tcc/contracts): one "cache" sample per site, one "go.runtime" sample and
// the lifecycle events, every interval. Telemetry never blocks the cache: envelopes that can't be
// sent wait in a bounded queue (the oldest go first, leaving a gap in `sequence`).
package report

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"runtime"
	"sync"
	"time"

	"github.com/raulbattistini3-sys/erp-cache/internal/proxy"
)

const maxQueue = 360

type Envelope struct {
	Version    int              `json:"version"`
	Source     string           `json:"source"`
	InstanceID string           `json:"instanceId"`
	BootID     string           `json:"bootId"`
	Sequence   int              `json:"sequence"`
	Timestamp  string           `json:"timestamp"`
	Samples    []map[string]any `json:"samples"`
}

type Reporter struct {
	URL, Token, Source, InstanceID string
	Interval                       time.Duration
	Sites                          []*proxy.Site
	Log                            *slog.Logger

	bootID   string
	started  time.Time
	mu       sync.Mutex
	pending  []map[string]any
	queue    []Envelope
	sequence int
	last     map[string]counters
	client   http.Client
}

type counters struct{ hits, misses, evictions, invalidations, bypassed uint64 }

func New(url, token, source, instance string, interval time.Duration, sites []*proxy.Site, log *slog.Logger) *Reporter {
	b := make([]byte, 16)
	rand.Read(b)
	r := &Reporter{URL: url, Token: token, Source: source, InstanceID: instance, Interval: interval, Sites: sites, Log: log,
		bootID: hex.EncodeToString(b), started: time.Now(), last: map[string]counters{}, client: http.Client{Timeout: 5 * time.Second}}
	r.Lifecycle("started", "")
	return r
}

// Lifecycle records started / ready / degraded / recovered / stopping for the next envelope.
func (r *Reporter) Lifecycle(state, reason string) {
	s := map[string]any{"type": "lifecycle", "state": state, "uptimeMs": float64(time.Since(r.started).Microseconds()) / 1000}
	if reason != "" {
		if len(reason) > 500 {
			reason = reason[:500]
		}
		s["reason"] = reason
	}
	r.mu.Lock()
	r.pending = append(r.pending, s)
	r.mu.Unlock()
}

// Sample queues a one-off sample (e.g. the cold-start numbers) for the next envelope.
func (r *Reporter) Sample(s map[string]any) {
	r.mu.Lock()
	r.pending = append(r.pending, s)
	r.mu.Unlock()
}

func (r *Reporter) collect(window time.Duration) []map[string]any {
	var out []map[string]any
	ms := float64(window.Milliseconds())
	for _, s := range r.Sites {
		st := s.Store.Stats()
		now := counters{st.Hits, st.Misses, st.Evictions, st.Invalidations, s.Bypassed()}
		prev := r.last[s.Name]
		r.last[s.Name] = now
		up := s.UpstreamLatency()
		out = append(out, map[string]any{
			"type": "cache", "intervalMs": ms, "name": s.Name,
			"hits": now.hits - prev.hits, "misses": now.misses - prev.misses,
			"evictions": now.evictions - prev.evictions, "invalidations": now.invalidations - prev.invalidations,
			"entries": st.Entries, "bytes": st.Bytes, "maxBytes": st.MaxBytes,
			// not in the contract's cache schema: passes through as extra fields
			"bypassed": now.bypassed - prev.bypassed, "feedUp": s.FeedUp(),
			"upstreamCount": up.Count, "upstreamMeanMs": up.MeanMs, "upstreamMaxMs": up.MaxMs,
		})
	}
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	out = append(out, map[string]any{
		"type": "go.runtime", "category": "runtime",
		"heapAllocBytes": m.HeapAlloc, "heapSysBytes": m.HeapSys, "sysBytes": m.Sys,
		"goroutines": runtime.NumGoroutine(), "numGC": m.NumGC, "gcPauseTotalMs": float64(m.PauseTotalNs) / 1e6,
	})
	return out
}

// Run takes and sends a batch every interval until ctx ends, then sends a last one.
func (r *Reporter) Run(ctx context.Context) {
	t := time.NewTicker(r.Interval)
	defer t.Stop()
	last := time.Now()
	for {
		select {
		case <-ctx.Done():
			r.Lifecycle("stopping", "shutdown")
			r.tick(time.Since(last))
			return
		case now := <-t.C:
			r.tick(now.Sub(last))
			last = now
		}
	}
}

func (r *Reporter) tick(window time.Duration) {
	r.mu.Lock()
	samples := append(r.pending, r.collect(window)...)
	r.pending = nil
	r.queue = append(r.queue, Envelope{1, r.Source, r.InstanceID, r.bootID, r.sequence, time.Now().UTC().Format(time.RFC3339Nano), samples})
	r.sequence++
	if len(r.queue) > maxQueue {
		r.queue = r.queue[len(r.queue)-maxQueue:]
	}
	r.mu.Unlock()
	if r.URL == "" {
		r.mu.Lock()
		r.queue = nil
		r.mu.Unlock()
		return
	}
	for {
		r.mu.Lock()
		if len(r.queue) == 0 {
			r.mu.Unlock()
			return
		}
		e := r.queue[0]
		r.mu.Unlock()
		if err := r.send(e); err != nil {
			r.Log.Debug("telemetry not sent, kept for the next interval", "err", err)
			return
		}
		r.mu.Lock()
		r.queue = r.queue[1:]
		r.mu.Unlock()
	}
}

func (r *Reporter) send(e Envelope) error {
	body, _ := json.Marshal(e)
	req, err := http.NewRequest(http.MethodPost, r.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if r.Token != "" {
		req.Header.Set("Authorization", "Bearer "+r.Token)
	}
	res, err := r.client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		return fmt.Errorf("POST %s: HTTP %d %s", r.URL, res.StatusCode, msg)
	}
	return nil
}
