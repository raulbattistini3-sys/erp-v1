package supervisor

import (
	"encoding/json"
	"os"
	"sync"
	"time"
)

// Event: one thing that happened to the application or a service. Kinds:
//
//	lifecycle  starting, started, ready, exited, backoff, deferred, failed, stopping, stopped
//	resources  soft-limit, hard-limit, process-limit, pressure (normal | elevated | critical)
//	app        boot, shutdown, warning
type Event struct {
	Seq     uint64         `json:"seq"`
	Time    time.Time      `json:"time"`
	Service string         `json:"service,omitempty"`
	Kind    string         `json:"kind"`
	PID     int            `json:"pid,omitempty"`
	Detail  string         `json:"detail,omitempty"`
	Data    map[string]any `json:"data,omitempty"`
}

// eventLog keeps the last events in memory (for status / the events command), appends them to an NDJSON
// file when there is one, and fans them out to followers.
type eventLog struct {
	mu        sync.Mutex
	seq       uint64
	ring      []Event
	next      int
	full      bool
	file      *os.File
	followers map[chan Event]struct{}
}

func newEventLog(size int, path string) (*eventLog, error) {
	l := &eventLog{ring: make([]Event, size), followers: map[chan Event]struct{}{}}
	if path != "" {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return nil, err
		}
		l.file = f
	}
	return l, nil
}

func (l *eventLog) add(e Event) Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seq++
	e.Seq = l.seq
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	l.ring[l.next] = e
	l.next = (l.next + 1) % len(l.ring)
	if l.next == 0 {
		l.full = true
	}
	if l.file != nil {
		if line, err := json.Marshal(e); err == nil {
			_, _ = l.file.Write(append(line, '\n'))
		}
	}
	for ch := range l.followers {
		select {
		case ch <- e:
		default: // a slow follower misses events rather than stalling the supervisor
		}
	}
	return e
}

// since: the kept events after seq, oldest first.
func (l *eventLog) since(seq uint64) []Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	var all []Event
	if l.full {
		all = append(all, l.ring[l.next:]...)
	}
	all = append(all, l.ring[:l.next]...)
	out := all[:0:0]
	for _, e := range all {
		if e.Seq > seq {
			out = append(out, e)
		}
	}
	return out
}

func (l *eventLog) follow() (chan Event, func()) {
	ch := make(chan Event, 64)
	l.mu.Lock()
	l.followers[ch] = struct{}{}
	l.mu.Unlock()
	return ch, func() {
		l.mu.Lock()
		delete(l.followers, ch)
		l.mu.Unlock()
	}
}

func (l *eventLog) close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file != nil {
		_ = l.file.Close()
	}
}
