package proxy

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/raulbattistini3-sys/erp-cache/internal/config"
)

func site(t *testing.T, upstream string, hasFeed bool) *Site {
	t.Helper()
	cfg := config.Site{
		Name: "erp", Upstream: upstream, MaxEntries: 100, MaxBytes: 1 << 20, MaxEntryBytes: 1 << 16,
		Vary: []string{"authorization"}, FlushOn: []string{"usuarios"},
		Routes: []config.Route{
			{Path: "/products/filter", TTL: config.Duration(time.Minute), Tables: []string{"produtos"}},
			{Path: "/clientes/empresas/:id", TTL: config.Duration(time.Minute), Tables: []string{"empresas"}},
		},
	}
	s, err := New(cfg, hasFeed, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func get(t *testing.T, h http.Handler, path, auth string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, path, nil)
	if auth != "" {
		r.Header.Set("Authorization", auth)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestHitMissVaryAndInvalidation(t *testing.T) {
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"user":"`+r.Header.Get("Authorization")+`","q":"`+r.URL.RawQuery+`"}`)
	}))
	defer up.Close()
	s := site(t, up.URL, false)

	if w := get(t, s, "/products/filter?b=2&a=1", "Bearer A"); w.Header().Get("X-Cache") != "miss" {
		t.Fatalf("first request: %s", w.Header().Get("X-Cache"))
	}
	// same query, parameters in another order: the same entry
	w := get(t, s, "/products/filter?a=1&b=2", "Bearer A")
	if w.Header().Get("X-Cache") != "hit" || w.Body.String() != `{"user":"Bearer A","q":"b=2&a=1"}` {
		t.Fatalf("second request: %s %s", w.Header().Get("X-Cache"), w.Body.String())
	}
	// another user never sees A's answer
	if w := get(t, s, "/products/filter?a=1&b=2", "Bearer B"); w.Header().Get("X-Cache") != "miss" {
		t.Fatal("a different Authorization was served from another user's entry")
	}
	if calls.Load() != 2 {
		t.Fatalf("upstream calls %d, want 2", calls.Load())
	}
	s.Changed("produtos")
	if w := get(t, s, "/products/filter?a=1&b=2", "Bearer A"); w.Header().Get("X-Cache") != "miss" {
		t.Fatal("served after its table changed")
	}
	// not a configured route: straight through
	if w := get(t, s, "/products/scroll", "Bearer A"); w.Header().Get("X-Cache") != "bypass" {
		t.Fatal("an unconfigured route was cached")
	}
	get(t, s, "/clientes/empresas/7", "Bearer A")
	if w := get(t, s, "/clientes/empresas/7", "Bearer A"); w.Header().Get("X-Cache") != "hit" {
		t.Fatal(":id route not cached")
	}
	s.Changed("usuarios") // flushOn: everything goes
	if st := s.Store.Stats(); st.Entries != 0 {
		t.Fatalf("flushOn left %d entries", st.Entries)
	}
}

func TestFeedDownBypassesAndForgets(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") }))
	defer up.Close()
	s := site(t, up.URL, true)
	if w := get(t, s, "/products/filter", ""); w.Header().Get("X-Cache") != "bypass" {
		t.Fatal("served from the cache before the feed was up")
	}
	s.SetFeed(true)
	get(t, s, "/products/filter", "")
	if w := get(t, s, "/products/filter", ""); w.Header().Get("X-Cache") != "hit" {
		t.Fatal("not cached with the feed up")
	}
	s.SetFeed(false)
	if w := get(t, s, "/products/filter", ""); w.Header().Get("X-Cache") != "bypass" {
		t.Fatal("served from the cache with the feed down")
	}
	if s.Store.Stats().Entries != 0 {
		t.Fatal("entries kept across a feed drop")
	}
}

func TestSingleflightAndUncacheable(t *testing.T) {
	var calls atomic.Int32
	release := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path == "/clientes/empresas/404" {
			http.NotFound(w, r)
			return
		}
		<-release
		io.WriteString(w, "slow")
	}))
	defer up.Close()
	s := site(t, up.URL, false)

	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if w := get(t, s, "/products/filter?x=1", ""); w.Body.String() != "slow" {
				t.Errorf("body %q", w.Body.String())
			}
		}()
	}
	time.Sleep(100 * time.Millisecond)
	close(release)
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("10 identical misses made %d upstream calls, want 1", calls.Load())
	}
	get(t, s, "/clientes/empresas/404", "")
	if w := get(t, s, "/clientes/empresas/404", ""); w.Code != 404 || w.Header().Get("X-Cache") != "miss" {
		t.Fatal("a 404 was cached")
	}
}
