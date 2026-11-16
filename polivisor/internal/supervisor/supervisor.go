// Package supervisor runs a dependency-ordered set of services: it gates each
// start on its dependencies' readiness and on free memory, contains every
// service in its own OS unit (Job Object on Windows), restarts crashes with
// backoff inside a budget, and stops everything in reverse order.
package supervisor

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"sync"
	"time"

	"github.com/raulbattistini3-sys/erp-v1/polivisor/internal/osctl"
	"github.com/raulbattistini3-sys/erp-v1/polivisor/internal/spec"
)

type State string

const (
	Waiting  State = "waiting"
	Starting State = "starting"
	Ready    State = "ready"
	Backoff  State = "backoff"
	Failed   State = "failed"
	Skipped  State = "skipped"
	Stopped  State = "stopped"
)

type Status struct {
	Name     string `json:"name"`
	State    State  `json:"state"`
	PID      int    `json:"pid,omitempty"`
	Restarts int    `json:"restarts"`
	MemMB    int    `json:"mem_mb"`
	LimitMB  int    `json:"limit_mb,omitempty"`
	Tier     int    `json:"tier"`
}

type svc struct {
	spec     *spec.Service
	tier     int
	ready    chan struct{} // closed once the service first becomes ready
	done     chan struct{} // closed when the service reaches ready, failed or skipped
	mu       sync.Mutex
	st       Status
	unit     osctl.Unit
	exited   chan struct{}
	readyOne sync.Once
	doneOne  sync.Once
}

func (s *svc) set(f func(*Status)) { s.mu.Lock(); f(&s.st); s.mu.Unlock() }
func (s *svc) get() Status         { s.mu.Lock(); defer s.mu.Unlock(); return s.st }
func (s *svc) markReady()          { s.readyOne.Do(func() { close(s.ready) }); s.markDone() }
func (s *svc) markDone()           { s.doneOne.Do(func() { close(s.done) }) }

type Supervisor struct {
	cfg   *spec.Config
	svcs  map[string]*svc
	order [][]string
	out   *logger
	abort context.CancelCauseFunc
}

func New(cfg *spec.Config) (*Supervisor, error) {
	tiers, err := cfg.Tiers()
	if err != nil {
		return nil, err
	}
	sv := &Supervisor{cfg: cfg, svcs: map[string]*svc{}, order: tiers, out: newLogger(os.Stdout)}
	for t, names := range tiers {
		for _, n := range names {
			s := cfg.Find(n)
			sv.svcs[n] = &svc{spec: s, tier: t, ready: make(chan struct{}), done: make(chan struct{}),
				st: Status{Name: n, State: Waiting, Tier: t, LimitMB: s.MemoryMB}}
		}
	}
	return sv, nil
}

// Run blocks until ctx is cancelled (graceful stop) or a critical service gives up.
func (sv *Supervisor) Run(ctx context.Context) error {
	runCtx, abort := context.WithCancelCause(ctx)
	sv.abort = abort
	defer abort(nil)

	for i, names := range sv.order {
		sv.out.line("polivisor", fmt.Sprintf("tier %d: %v", i, names))
	}
	if sv.cfg.Control != "" {
		go sv.serveControl(runCtx)
	}
	go sv.monitor(runCtx)

	var wg sync.WaitGroup
	for _, s := range sv.svcs {
		wg.Add(1)
		go func(s *svc) { defer wg.Done(); sv.lifecycle(runCtx, s) }(s)
	}
	<-runCtx.Done()
	cause := context.Cause(runCtx)
	sv.stopAll()
	wg.Wait()
	if cause == context.Canceled || cause == ctx.Err() {
		return nil
	}
	return cause
}

func (sv *Supervisor) lifecycle(ctx context.Context, s *svc) {
	sp := s.spec
	defer s.markDone()

	// Gate on dependencies: depends_on must be ready, after only needs to settle.
	for _, d := range sp.DependsOn {
		dep := sv.svcs[d]
		select {
		case <-dep.done:
		case <-ctx.Done():
			return
		}
		select {
		case <-dep.ready:
		default:
			sv.out.line(sp.Name, fmt.Sprintf("not starting: dependency %s is %s", d, dep.get().State))
			s.set(func(st *Status) { st.State = Skipped })
			return
		}
	}
	for _, d := range sp.After {
		select {
		case <-sv.svcs[d].done:
		case <-ctx.Done():
			return
		}
	}

	if _, err := exec.LookPath(sp.Program); err != nil {
		if sp.Optional {
			sv.out.line(sp.Name, "skipped: "+sp.Program+" not found")
			s.set(func(st *Status) { st.State = Skipped })
			return
		}
		sv.giveUp(s, fmt.Errorf("%s: %w", sp.Program, err))
		return
	}

	var history []time.Time
	backoff := sp.Restart.BackoffMin.Duration
	for {
		sv.admit(ctx, s)
		if ctx.Err() != nil {
			return
		}
		started := time.Now()
		err := sv.runOnce(ctx, s)
		if ctx.Err() != nil {
			return
		}
		now := time.Now()
		if now.Sub(started) > sp.Restart.Window.Duration {
			backoff = sp.Restart.BackoffMin.Duration // ran healthy for a while
		}
		history = append(history, now)
		for len(history) > 0 && now.Sub(history[0]) > sp.Restart.Window.Duration {
			history = history[1:]
		}
		if len(history) > sp.Restart.Max {
			sv.giveUp(s, fmt.Errorf("%d crashes within %s (last: %v)", len(history), sp.Restart.Window.Duration, err))
			return
		}
		sv.out.line(sp.Name, fmt.Sprintf("exited (%v); restarting in %s", err, backoff))
		s.set(func(st *Status) { st.State = Backoff; st.PID = 0; st.Restarts++; st.MemMB = 0 })
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return
		}
		backoff = min(backoff*2, sp.Restart.BackoffMax.Duration)
	}
}

// admit waits (bounded by the ready timeout) for enough free RAM, so a heavy
// service is not started into a machine that is already paging.
func (sv *Supervisor) admit(ctx context.Context, s *svc) {
	need := s.spec.MinFreeMB
	if need <= 0 {
		return
	}
	deadline := time.Now().Add(s.spec.Ready.Timeout.Duration)
	warned := false
	for {
		free := osctl.AvailableMB()
		if free < 0 || free >= need || time.Now().After(deadline) {
			if free >= 0 && free < need {
				sv.out.line(s.spec.Name, fmt.Sprintf("starting under memory pressure (%d MB free < %d MB)", free, need))
			}
			return
		}
		if !warned {
			sv.out.line(s.spec.Name, fmt.Sprintf("waiting for memory: %d MB free, need %d MB", free, need))
			warned = true
		}
		select {
		case <-time.After(2 * time.Second):
		case <-ctx.Done():
			return
		}
	}
}

func (sv *Supervisor) runOnce(ctx context.Context, s *svc) error {
	sp := s.spec
	cmd := exec.Command(sp.Program, sp.CommandLine()...)
	cmd.Dir = sp.Cwd
	cmd.Env = os.Environ()
	for k, v := range sp.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	osctl.Prepare(cmd)
	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()
	s.set(func(st *Status) { st.State = Starting })
	if err := cmd.Start(); err != nil {
		return err
	}
	unit, err := osctl.Contain(cmd, sp.MemoryMB)
	if err != nil {
		sv.out.line(sp.Name, "warning: not contained: "+err.Error())
		unit = nil
	}
	exited := make(chan struct{})
	s.mu.Lock()
	s.unit, s.exited = unit, exited
	s.st.PID = cmd.Process.Pid
	s.mu.Unlock()

	var pipes sync.WaitGroup
	pipes.Add(2)
	go func() { defer pipes.Done(); sv.out.pipe(sp.Name, stdout) }()
	go func() { defer pipes.Done(); sv.out.pipe(sp.Name, stderr) }()

	probeCtx, stopProbe := context.WithCancel(ctx)
	go sv.probe(probeCtx, s)

	pipes.Wait()
	err = cmd.Wait()
	stopProbe()
	close(exited)
	s.mu.Lock()
	if s.unit != nil {
		s.unit.Close()
	}
	s.unit = nil
	s.mu.Unlock()
	if err == nil {
		err = fmt.Errorf("exit 0")
	}
	return err
}

func (sv *Supervisor) probe(ctx context.Context, s *svc) {
	addr := s.spec.Ready.TCP
	if addr == "" {
		// No probe: consider it ready once it survived a short warm-up.
		select {
		case <-time.After(500 * time.Millisecond):
			sv.setReady(s)
		case <-ctx.Done():
		}
		return
	}
	deadline := time.Now().Add(s.spec.Ready.Timeout.Duration)
	for ctx.Err() == nil {
		c, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			c.Close()
			sv.setReady(s)
			return
		}
		if time.Now().After(deadline) {
			sv.out.line(s.spec.Name, fmt.Sprintf("not ready on %s after %s; killing", addr, s.spec.Ready.Timeout.Duration))
			s.mu.Lock()
			u := s.unit
			s.mu.Unlock()
			if u != nil {
				u.Kill()
			}
			return
		}
		select {
		case <-time.After(300 * time.Millisecond):
		case <-ctx.Done():
		}
	}
}

func (sv *Supervisor) setReady(s *svc) {
	first := false
	s.readyOne.Do(func() { first = true; close(s.ready) })
	s.markDone()
	s.set(func(st *Status) { st.State = Ready })
	if first {
		sv.out.line(s.spec.Name, "ready")
	}
}

func (sv *Supervisor) giveUp(s *svc, err error) {
	s.set(func(st *Status) { st.State = Failed; st.PID = 0 })
	sv.out.line(s.spec.Name, "giving up: "+err.Error())
	if s.spec.Critical {
		sv.abort(fmt.Errorf("critical service %s failed: %w", s.spec.Name, err))
	}
}

// stopAll interrupts tiers from the top down, waiting up to grace for each tier
// before forcibly terminating its jobs.
func (sv *Supervisor) stopAll() {
	sv.out.line("polivisor", "stopping")
	for t := len(sv.order) - 1; t >= 0; t-- {
		var wg sync.WaitGroup
		for _, n := range sv.order[t] {
			s := sv.svcs[n]
			s.mu.Lock()
			u, exited := s.unit, s.exited
			s.mu.Unlock()
			if u == nil {
				continue
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				u.Interrupt()
				select {
				case <-exited:
				case <-time.After(sv.cfg.Grace.Duration):
					sv.out.line(s.spec.Name, "did not stop in time; terminating")
					u.Kill()
					<-exited
				}
			}()
		}
		wg.Wait()
	}
	for _, s := range sv.svcs {
		s.set(func(st *Status) {
			if st.State != Failed && st.State != Skipped {
				st.State = Stopped
			}
			st.PID = 0
		})
	}
}

// monitor samples memory per unit and warns when a service nears its cap.
func (sv *Supervisor) monitor(ctx context.Context) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	warned := map[string]bool{}
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		for n, s := range sv.svcs {
			s.mu.Lock()
			u := s.unit
			s.mu.Unlock()
			if u == nil {
				continue
			}
			mb := int(u.MemBytes() >> 20)
			s.set(func(st *Status) { st.MemMB = mb })
			if lim := s.spec.MemoryMB; lim > 0 {
				high := mb*10 >= lim*9
				if high && !warned[n] {
					sv.out.line(n, fmt.Sprintf("memory %d MB is above 90%% of its %d MB cap", mb, lim))
				}
				warned[n] = high
			}
		}
	}
}

func (sv *Supervisor) Snapshot() []Status {
	out := make([]Status, 0, len(sv.svcs))
	for _, s := range sv.svcs {
		out = append(out, s.get())
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Tier != out[j].Tier {
			return out[i].Tier < out[j].Tier
		}
		return out[i].Name < out[j].Name
	})
	return out
}

func (sv *Supervisor) serveControl(ctx context.Context) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"available_mb": osctl.AvailableMB(),
			"services":     sv.Snapshot(),
		})
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		for _, st := range sv.Snapshot() {
			if st.State == Failed {
				w.WriteHeader(http.StatusServiceUnavailable)
				fmt.Fprintf(w, "%s failed\n", st.Name)
				return
			}
		}
		io.WriteString(w, "ok\n")
	})
	srv := &http.Server{Addr: sv.cfg.Control, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { <-ctx.Done(); srv.Close() }()
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		sv.out.line("polivisor", "control endpoint: "+err.Error())
	}
}

type logger struct {
	mu sync.Mutex
	w  io.Writer
}

func newLogger(w io.Writer) *logger { return &logger{w: w} }

func (l *logger) line(src, msg string) {
	l.mu.Lock()
	fmt.Fprintf(l.w, "%9s | %s\n", src, msg)
	l.mu.Unlock()
}

func (l *logger) pipe(src string, r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		l.line(src, sc.Text())
	}
}
