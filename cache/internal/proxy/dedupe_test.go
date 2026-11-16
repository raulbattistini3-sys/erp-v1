package proxy

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/raulbattistini3-sys/erp-cache/internal/config"
)

func tccSite(t *testing.T, upstream string) *Site {
	t.Helper()
	cfg := config.Site{
		Name: "tcc", Upstream: upstream, MaxEntries: 100, MaxBytes: 1 << 20, MaxEntryBytes: 1 << 16,
		Vary: []string{"authorization"},
		Routes: []config.Route{
			{Path: "/telemetry/host/latest", TTL: config.Duration(time.Minute), Tables: []string{"host_samples"}},
		},
		Dedupe: []config.Dedupe{{
			Path: "/telemetry/host", Keys: []string{"source", "instanceId", "bootId", "sequence"},
			Invalidates: []string{"host_batches", "host_samples"}, MaxKeys: 2,
		}},
	}
	s, err := New(cfg, false, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func post(t *testing.T, h http.Handler, path, auth, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	r.Header.Set("Authorization", auth)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func envelope(seq int) string {
	return `{"source":"sample-erp-api","instanceId":"api-01","bootId":"b1","sequence":` + strconv.Itoa(seq) + `,"samples":[]}`
}

func TestDedupeAnswersResendsOfAcceptedRecords(t *testing.T) {
	var posts, bodies atomic.Int32
	fail := atomic.Bool{}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posts.Add(1)
			if b, _ := io.ReadAll(r.Body); len(b) > 0 {
				bodies.Add(1)
			}
			if fail.Load() {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		io.WriteString(w, `[]`)
	}))
	defer up.Close()
	s := tccSite(t, up.URL)

	// a failed POST is not remembered: its resend goes to the upstream
	fail.Store(true)
	if w := post(t, s, "/telemetry/host", "Bearer A", envelope(1)); w.Code != 503 || w.Header().Get("X-Cache") != "miss" {
		t.Fatalf("failing upstream: %d %s", w.Code, w.Header().Get("X-Cache"))
	}
	fail.Store(false)
	if w := post(t, s, "/telemetry/host", "Bearer A", envelope(1)); w.Code != 204 || w.Header().Get("X-Cache") != "miss" {
		t.Fatalf("first accepted: %d %s", w.Code, w.Header().Get("X-Cache"))
	}
	// the resend of an accepted record is answered here, with the upstream's status
	if w := post(t, s, "/telemetry/host", "Bearer A", envelope(1)); w.Code != 204 || w.Header().Get("X-Cache") != "dedupe" {
		t.Fatalf("resend: %d %s", w.Code, w.Header().Get("X-Cache"))
	}
	if posts.Load() != 2 || bodies.Load() != 2 {
		t.Fatalf("upstream saw %d posts (%d with a body), want 2", posts.Load(), bodies.Load())
	}
	// another caller, or another sequence, is another record
	post(t, s, "/telemetry/host", "Bearer B", envelope(1))
	post(t, s, "/telemetry/host", "Bearer A", envelope(2))
	if posts.Load() != 4 {
		t.Fatalf("upstream saw %d posts, want 4", posts.Load())
	}
	// a body without the key fields goes to the upstream (which answers it)
	post(t, s, "/telemetry/host", "Bearer A", `{"source":"x"}`)
	post(t, s, "/telemetry/host", "Bearer A", `{"source":"x"}`)
	if posts.Load() != 6 {
		t.Fatalf("upstream saw %d posts, want 6", posts.Load())
	}
	// MaxKeys 2: the oldest record (A/1) was forgotten
	if w := post(t, s, "/telemetry/host", "Bearer A", envelope(1)); w.Header().Get("X-Cache") != "miss" {
		t.Fatalf("forgotten record: %s", w.Header().Get("X-Cache"))
	}
	st := s.Dedupes()[0]
	if st.Answered != 1 || st.Forwarded != 7 || st.Remembered != 2 {
		t.Fatalf("stats %+v", st)
	}
	s.ForgetRecords()
	if s.Dedupes()[0].Remembered != 0 {
		t.Fatal("ForgetRecords kept records")
	}
}

func TestDedupeInvalidatesCachedReads(t *testing.T) {
	var gets atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		gets.Add(1)
		io.WriteString(w, `[]`)
	}))
	defer up.Close()
	s := tccSite(t, up.URL)

	get(t, s, "/telemetry/host/latest", "Bearer A")
	if w := get(t, s, "/telemetry/host/latest", "Bearer A"); w.Header().Get("X-Cache") != "hit" {
		t.Fatalf("second read: %s", w.Header().Get("X-Cache"))
	}
	// a new record drops what reads host_samples
	post(t, s, "/telemetry/host", "Bearer A", envelope(3))
	if w := get(t, s, "/telemetry/host/latest", "Bearer A"); w.Header().Get("X-Cache") != "miss" {
		t.Fatalf("read after a new record: %s", w.Header().Get("X-Cache"))
	}
	// a resend changes nothing upstream, so the cached read stays
	post(t, s, "/telemetry/host", "Bearer A", envelope(3))
	if w := get(t, s, "/telemetry/host/latest", "Bearer A"); w.Header().Get("X-Cache") != "hit" {
		t.Fatalf("read after a resend: %s", w.Header().Get("X-Cache"))
	}
	if gets.Load() != 2 {
		t.Fatalf("upstream saw %d reads, want 2", gets.Load())
	}
}
