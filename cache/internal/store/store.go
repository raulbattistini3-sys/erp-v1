// Package store is the cache's memory: a bounded LRU of HTTP responses with a TTL, tagged by the
// database tables each response was read from.
//
// Bounds: entries, total bytes and bytes per entry. Nothing in here grows without limit: the tag
// index only holds keys that are in the LRU, and goes away with them.
//
// Freshness: a response may only be stored if none of its tables changed while it was being fetched.
// Every table has a generation that Invalidate bumps; the proxy reads the generation before going to
// the upstream (Gen) and hands it back to Put, which refuses the response if it moved. ForgetAll bumps
// an epoch that is part of every generation, so it also refuses everything that was in flight.
package store

import (
	"container/list"
	"net/http"
	"sync"
	"time"
)

// Entry is one stored response.
type Entry struct {
	Key     string
	Status  int
	Header  http.Header
	Body    []byte
	Stored  time.Time
	Expires time.Time
	Tags    []string

	size int
	elem *list.Element
}

// Stats are counters since the store was made (except Entries, Bytes: current values).
type Stats struct {
	Hits          uint64 `json:"hits"`
	Misses        uint64 `json:"misses"`
	Stores        uint64 `json:"stores"`
	Evictions     uint64 `json:"evictions"`
	Expirations   uint64 `json:"expirations"`
	Invalidations uint64 `json:"invalidations"` // entries removed because a table changed
	StaleRejected uint64 `json:"staleRejected"` // responses not stored: a table changed while they were fetched
	TooLarge      uint64 `json:"tooLarge"`
	Flushes       uint64 `json:"flushes"`
	Entries       int    `json:"entries"`
	Bytes         int64  `json:"bytes"`
	MaxEntries    int    `json:"maxEntries"`
	MaxBytes      int64  `json:"maxBytes"`
}

// Store is safe for concurrent use.
type Store struct {
	mu            sync.Mutex
	maxEntries    int
	maxBytes      int64
	maxEntryBytes int
	ll            *list.List // front = most recently used
	items         map[string]*Entry
	byTag         map[string]map[string]struct{}
	gen           map[string]uint64
	epoch         uint64
	bytes         int64
	stats         Stats
	now           func() time.Time
}

// New makes a store holding at most maxEntries responses and maxBytes bytes; a single response over
// maxEntryBytes is never stored.
func New(maxEntries int, maxBytes int64, maxEntryBytes int) *Store {
	if maxEntries < 1 {
		maxEntries = 1
	}
	if maxEntryBytes <= 0 || int64(maxEntryBytes) > maxBytes {
		maxEntryBytes = int(maxBytes)
	}
	return &Store{
		maxEntries:    maxEntries,
		maxBytes:      maxBytes,
		maxEntryBytes: maxEntryBytes,
		ll:            list.New(),
		items:         map[string]*Entry{},
		byTag:         map[string]map[string]struct{}{},
		gen:           map[string]uint64{},
		now:           time.Now,
	}
}

// MaxEntryBytes is the largest body Put accepts.
func (s *Store) MaxEntryBytes() int { return s.maxEntryBytes }

// Get returns a live entry and marks it used. An expired one is removed and counts as a miss.
func (s *Store) Get(key string) (*Entry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.items[key]
	if ok && !s.now().Before(e.Expires) {
		s.remove(e)
		s.stats.Expirations++
		ok = false
	}
	if !ok {
		s.stats.Misses++
		return nil, false
	}
	s.ll.MoveToFront(e.elem)
	s.stats.Hits++
	return e, true
}

// Gen is the generation of a set of tables: it changes whenever any of them is invalidated, or
// everything is forgotten.
func (s *Store) Gen(tags []string) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.genLocked(tags)
}

func (s *Store) genLocked(tags []string) uint64 {
	// a sum of counters that only go up: equal before and after means none of them moved
	g := s.epoch
	for _, t := range tags {
		g += s.gen[t]
	}
	return g
}

// Put stores a response read while the tables' generation was `gen`, for ttl. It returns false (and
// stores nothing) when the body is too large or a table changed in the meantime.
func (s *Store) Put(key string, status int, header http.Header, body []byte, tags []string, ttl time.Duration, gen uint64) bool {
	size := len(key) + len(body) + headerSize(header) + 64
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(body) > s.maxEntryBytes || int64(size) > s.maxBytes {
		s.stats.TooLarge++
		return false
	}
	if s.genLocked(tags) != gen {
		s.stats.StaleRejected++
		return false
	}
	if old, ok := s.items[key]; ok {
		s.remove(old)
	}
	now := s.now()
	e := &Entry{Key: key, Status: status, Header: header, Body: body, Stored: now, Expires: now.Add(ttl), Tags: tags, size: size}
	e.elem = s.ll.PushFront(e)
	s.items[key] = e
	for _, t := range tags {
		m := s.byTag[t]
		if m == nil {
			m = map[string]struct{}{}
			s.byTag[t] = m
		}
		m[key] = struct{}{}
	}
	s.bytes += int64(size)
	s.stats.Stores++
	for len(s.items) > s.maxEntries || s.bytes > s.maxBytes {
		oldest := s.ll.Back().Value.(*Entry)
		s.remove(oldest)
		s.stats.Evictions++
	}
	return true
}

// Invalidate drops every response read from `table` and refuses the ones being fetched. It returns
// how many entries went.
func (s *Store) Invalidate(table string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gen[table]++
	n := 0
	for key := range s.byTag[table] {
		if e, ok := s.items[key]; ok {
			s.remove(e)
			n++
		}
	}
	delete(s.byTag, table)
	s.stats.Invalidations += uint64(n)
	return n
}

// ForgetAll drops everything, and refuses everything being fetched.
func (s *Store) ForgetAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.epoch++
	s.ll.Init()
	s.items = map[string]*Entry{}
	s.byTag = map[string]map[string]struct{}{}
	s.bytes = 0
	s.stats.Flushes++
}

// Stats returns a copy of the counters.
func (s *Store) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.stats
	st.Entries = len(s.items)
	st.Bytes = s.bytes
	st.MaxEntries = s.maxEntries
	st.MaxBytes = s.maxBytes
	return st
}

func (s *Store) remove(e *Entry) {
	s.ll.Remove(e.elem)
	delete(s.items, e.Key)
	for _, t := range e.Tags {
		if m := s.byTag[t]; m != nil {
			delete(m, e.Key)
			if len(m) == 0 {
				delete(s.byTag, t)
			}
		}
	}
	s.bytes -= int64(e.size)
}

func headerSize(h http.Header) int {
	n := 0
	for k, vs := range h {
		for _, v := range vs {
			n += len(k) + len(v) + 4
		}
	}
	return n
}
