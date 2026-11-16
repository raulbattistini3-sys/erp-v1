package coldstart

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestWarmMeasuresColdThenWarmAndSendsAuth(t *testing.T) {
	var gets atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			return
		}
		if r.Header.Get("Authorization") != "Bearer x" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if gets.Add(1) == 1 {
			time.Sleep(60 * time.Millisecond) // the cold first answer
		}
	}))
	defer srv.Close()

	reqs, err := Warm(context.Background(), srv.URL, []string{"/a"}, "Bearer x", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(reqs) != 1 || reqs[0].Status != 200 || reqs[0].Err != "" {
		t.Fatalf("unexpected %+v", reqs)
	}
	if reqs[0].ColdMs < 50 || reqs[0].WarmMs >= reqs[0].ColdMs {
		t.Fatalf("cold %v should exceed warm %v", reqs[0].ColdMs, reqs[0].WarmMs)
	}
}

func TestWarmGivesUpWhenUpstreamNeverAnswers(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	if _, err := Warm(context.Background(), url, []string{"/a"}, "", 600*time.Millisecond); err == nil {
		t.Fatal("expected an error")
	}
}
