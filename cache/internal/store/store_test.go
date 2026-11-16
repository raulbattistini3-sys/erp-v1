package store

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestLRUBoundsAndTTL(t *testing.T) {
	s := New(2, 1<<20, 0)
	now := time.Unix(0, 0)
	s.now = func() time.Time { return now }
	put := func(k string) { s.Put(k, 200, http.Header{}, []byte(k), []string{"t"}, time.Minute, s.Gen([]string{"t"})) }

	put("a")
	put("b")
	s.Get("a") // a is now the most recent
	put("c")   // evicts b
	if _, ok := s.Get("b"); ok {
		t.Fatal("b should have been evicted")
	}
	if _, ok := s.Get("a"); !ok {
		t.Fatal("a should still be there")
	}
	now = now.Add(2 * time.Minute)
	if _, ok := s.Get("a"); ok {
		t.Fatal("a should have expired")
	}
	st := s.Stats()
	if st.Evictions != 1 || st.Expirations != 1 || st.Entries != 1 {
		t.Fatalf("stats %+v", st)
	}
}

func TestByteBound(t *testing.T) {
	s := New(100, 1000, 600)
	big := []byte(strings.Repeat("x", 400))
	for _, k := range []string{"a", "b", "c"} {
		s.Put(k, 200, nil, big, nil, time.Minute, s.Gen(nil))
	}
	if st := s.Stats(); st.Bytes > 1000 || st.Entries != 2 {
		t.Fatalf("over the byte bound: %+v", st)
	}
	if s.Put("huge", 200, nil, make([]byte, 700), nil, time.Minute, s.Gen(nil)) {
		t.Fatal("an entry over maxEntryBytes was stored")
	}
}

func TestInvalidateByTableAndInFlightRefusal(t *testing.T) {
	s := New(10, 1<<20, 0)
	s.Put("products", 200, nil, []byte("p"), []string{"produtos"}, time.Minute, s.Gen([]string{"produtos"}))
	s.Put("clients", 200, nil, []byte("c"), []string{"clientes"}, time.Minute, s.Gen([]string{"clientes"}))

	gen := s.Gen([]string{"produtos", "categorias"}) // a fetch starts
	if n := s.Invalidate("produtos"); n != 1 {
		t.Fatalf("invalidated %d, want 1", n)
	}
	if _, ok := s.Get("products"); ok {
		t.Fatal("products survived its table's invalidation")
	}
	if _, ok := s.Get("clients"); !ok {
		t.Fatal("clients went with an unrelated table")
	}
	// the fetch that started before the change comes back: it may be stale, so it is refused
	if s.Put("products", 200, nil, []byte("old"), []string{"produtos", "categorias"}, time.Minute, gen) {
		t.Fatal("a response read before the invalidation was stored")
	}
	s.ForgetAll()
	if st := s.Stats(); st.Entries != 0 || st.Bytes != 0 || st.StaleRejected != 1 {
		t.Fatalf("stats %+v", st)
	}
}
