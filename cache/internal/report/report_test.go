package report

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/raulbattistini3-sys/erp-cache/internal/config"
	"github.com/raulbattistini3-sys/erp-cache/internal/proxy"
)

func TestSendsEnvelopesAndKeepsThemWhileDown(t *testing.T) {
	var mu sync.Mutex
	var got []Envelope
	down := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "bad token", 401)
			return
		}
		if down {
			http.Error(w, "down", 503)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var e Envelope
		if err := json.Unmarshal(body, &e); err != nil {
			t.Error(err)
		}
		got = append(got, e)
		if p := os.Getenv("REPORT_ENVELOPE_OUT"); p != "" { // for checking against @tcc/contracts
			os.WriteFile(p, body, 0o644)
		}
		w.WriteHeader(202)
	}))
	defer srv.Close()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer upstream.Close()
	cfg := config.Site{Name: "erp", Upstream: upstream.URL, MaxEntries: 10, MaxBytes: 1 << 20, MaxEntryBytes: 1 << 16}
	site, err := proxy.New(cfg, false, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	r := New(srv.URL, "tok", "erp-cache", "test", time.Second, []*proxy.Site{site}, slog.Default())
	r.tick(time.Second) // down: kept
	r.tick(time.Second)
	if len(got) != 0 || len(r.queue) != 2 {
		t.Fatalf("while down: got %d, queued %d", len(got), len(r.queue))
	}
	mu.Lock()
	down = false
	mu.Unlock()
	r.Lifecycle("ready", "")
	r.tick(time.Second)
	if len(got) != 3 || len(r.queue) != 0 {
		t.Fatalf("after recovery: got %d, queued %d", len(got), len(r.queue))
	}
	for i, e := range got {
		if e.Sequence != i || e.Version != 1 || e.BootID != got[0].BootID {
			t.Fatalf("envelope %d: %+v", i, e)
		}
	}
	types := map[string]bool{}
	for _, s := range got[0].Samples {
		types[s["type"].(string)] = true
	}
	if !types["lifecycle"] || !types["cache"] || !types["go.runtime"] {
		t.Fatalf("first envelope samples: %v", types)
	}
}
