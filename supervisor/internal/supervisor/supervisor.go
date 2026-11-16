// Package supervisor runs an application's services: in dependency order, each started once what it
// depends on is ready, inside its memory limits, restarted by its policy, admitted only while the machine
// and the application's budget have room, and stopped in reverse order.
package supervisor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/raulbattistini3-sys/erp-v1/supervisor/internal/config"
	"github.com/raulbattistini3-sys/erp-v1/supervisor/internal/proc"
)

// Options for a run.
type Options struct {
	Out        io.Writer // service output, prefixed with the service's name; default os.Stdout
	ControlURL string    // handed to services as POLYVISOR_CONTROL_URL (with the token)
	Token      string
	Sample     time.Duration // usage / pressure sampling; default 2s
}

type Supervisor struct {
	app    *config.Application
	opts   Options
	group  proc.Group
	events *eventLog
	out    *prefixWriter

	units map[string]*unit
	order []*unit

	ctx      context.Context
	cancel   context.CancelFunc
	console  atomic.Bool // the shutdown came from the console (Ctrl+C reached every service already)
	exitCode atomic.Int32
	started  time.Time
	runtime  string

	mu     sync.Mutex
	change chan struct{}
}

// New prepares the application's container; Run starts the services.
func New(app *config.Application, opts Options) (*Supervisor, error) {
	if opts.Out == nil {
		opts.Out = os.Stdout
	}
	if opts.Sample <= 0 {
		opts.Sample = 2 * time.Second
	}
	order, err := app.Order()
	if err != nil {
		return nil, err
	}
	events, err := newEventLog(2048, app.Path(app.Events))
	if err != nil {
		return nil, fmt.Errorf("events file: %w", err)
	}
	group, err := proc.NewGroup(app.Name, uint64(app.Resources.MemoryBudget))
	if err != nil {
		events.close()
		return nil, err
	}
	s := &Supervisor{
		app: app, opts: opts, group: group, events: events,
		units: map[string]*unit{}, change: make(chan struct{}), started: time.Now(),
	}
	width := len("polyvisor")
	for _, svc := range order {
		width = max(width, len(svc.Name))
	}
	s.out = &prefixWriter{w: opts.Out, width: width}
	s.ctx, s.cancel = context.WithCancel(context.Background())
	for _, svc := range order {
		u := newUnit(s, svc)
		s.units[svc.Name] = u
		s.order = append(s.order, u)
	}
	for _, u := range s.order {
		for _, d := range u.svc.DependsOn {
			s.units[d].dependents = append(s.units[d].dependents, u)
		}
	}
	s.runtime = runtimeDir(app)
	return s, nil
}

// runtimeDir: the JS runtime services may load (POLYVISOR_RUNTIME): the configured one, or runtime/ next to
// polyvisor's executable (where the npm package and the launcher put it).
func runtimeDir(app *config.Application) string {
	if app.Runtime != "" {
		return app.Path(app.Runtime)
	}
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	for _, dir := range []string{filepath.Join(filepath.Dir(exe), "runtime"), filepath.Join(filepath.Dir(exe), "..", "..", "src", "runtime")} {
		if st, err := os.Stat(dir); err == nil && st.IsDir() {
			abs, _ := filepath.Abs(dir)
			return abs
		}
	}
	return ""
}

// Run starts every enabled service and returns once all have stopped (after Shutdown, or when a required
// service gave up). The int is the exit code: 0, or 1 when a required service failed.
func (s *Supervisor) Run() int {
	sys := proc.SystemMemory()
	s.emit(Event{Kind: "boot", PID: os.Getpid(), Detail: fmt.Sprintf("%s: %d services, budget %v, reserve %v, %s/%s",
		s.app.Name, len(s.order), s.app.Resources.MemoryBudget, s.app.Resources.ReserveForSystem, runtime.GOOS, runtime.GOARCH),
		Data: map[string]any{"system": sys}})
	var wg sync.WaitGroup
	for _, u := range s.order {
		wg.Add(1)
		go func() { defer wg.Done(); u.run() }()
	}
	go s.watchGroup()
	go s.sample()
	wg.Wait()
	s.emit(Event{Kind: "shutdown", Detail: fmt.Sprintf("exit %d", s.exitCode.Load())})
	s.group.Close()
	s.events.close()
	return int(s.exitCode.Load())
}

// Shutdown stops every service, dependents before what they depend on. fromConsole: Ctrl+C on the
// console, which the services sharing it received themselves.
func (s *Supervisor) Shutdown(fromConsole bool) {
	if s.ctx.Err() != nil {
		return
	}
	s.console.Store(fromConsole)
	s.cancel()
}

// Kill ends every service now (a second Ctrl+C).
func (s *Supervisor) Kill() {
	s.Shutdown(false)
	for _, u := range s.order {
		u.kill()
	}
}

func (s *Supervisor) fail(u *unit, why string) {
	s.exitCode.Store(1)
	s.emit(Event{Kind: "failed", Service: u.svc.Name, Detail: why + "; required, stopping the application"})
	s.Shutdown(false)
}

func (s *Supervisor) emit(e Event) Event {
	e = s.events.add(e)
	line := e.Kind
	if e.PID != 0 {
		line += fmt.Sprintf(" (pid %d)", e.PID)
	}
	if e.Detail != "" {
		line += ": " + e.Detail
	}
	name := e.Service
	if name == "" {
		name = "polyvisor"
	}
	s.out.line(name, "· "+line)
	return e
}

func (s *Supervisor) changed() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.change
}

func (s *Supervisor) bump() {
	s.mu.Lock()
	close(s.change)
	s.change = make(chan struct{})
	s.mu.Unlock()
}

// watchGroup turns the operating system's container notifications into events.
func (s *Supervisor) watchGroup() {
	for {
		select {
		case <-s.ctx.Done():
			// keep reading during the shutdown, until Run closes the group
		default:
		}
		e, ok := <-s.group.Events()
		if !ok {
			return
		}
		switch e.Kind {
		case "soft-limit", "hard-limit", "process-limit":
		default:
			continue
		}
		u := s.units[e.Service]
		ev := Event{Kind: e.Kind, Service: e.Service, PID: e.PID}
		if u != nil {
			usage := u.usage()
			u.noteLimit(e.Kind)
			ev.Data = map[string]any{"memoryBytes": usage.MemoryBytes, "softBytes": uint64(u.svc.Memory.SoftLimit), "hardBytes": uint64(u.svc.Memory.HardLimit)}
			ev.Detail = fmt.Sprintf("%s committed", mb(usage.MemoryBytes))
		} else if e.Service == "" {
			ev.Detail = "the application's memoryBudget " + s.app.Resources.MemoryBudget.String()
		}
		s.emit(ev)
		if u != nil && e.Kind != "soft-limit" {
			go s.reapStuck(u)
		}
	}
}

// reapStuck: the job refuses allocations past the hard limit, but a runtime may not exit on that: it
// collects again and again at the cap (a GC'd heap that cannot grow), serving nothing. One that freed
// memory keeps running; one still at the limit after hardLimitGrace is killed, and its restart policy
// applies, as for an out-of-memory exit.
func (s *Supervisor) reapStuck(u *unit) {
	select {
	case <-s.ctx.Done():
		return
	case <-time.After(hardLimitGrace):
	}
	hard := uint64(u.svc.Memory.HardLimit)
	if hard == 0 || u.usage().MemoryBytes < hard/100*98 {
		return
	}
	u.emit("killed", 0, "still at the hard limit after "+hardLimitGrace.String(), nil)
	u.kill()
}

const hardLimitGrace = 3 * time.Second

// sample reads each running service's usage and reports pressure changes: elevated from the soft limit,
// critical from 95% of the hard limit.
func (s *Supervisor) sample() {
	t := time.NewTicker(s.opts.Sample)
	defer t.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-t.C:
		}
		for _, u := range s.order {
			u.sample()
		}
	}
}

// Status of the application and its services.
type Status struct {
	App       string          `json:"app"`
	PID       int             `json:"pid"`
	StartedAt time.Time       `json:"startedAt"`
	Stopping  bool            `json:"stopping"`
	Budget    uint64          `json:"memoryBudgetBytes"`
	Reserve   uint64          `json:"reserveForSystemBytes"`
	System    proc.System     `json:"system"`
	Usage     proc.Usage      `json:"usage"`
	Runtime   string          `json:"runtime,omitempty"`
	Services  []ServiceStatus `json:"services"`
}

func (s *Supervisor) Status() Status {
	st := Status{
		App: s.app.Name, PID: os.Getpid(), StartedAt: s.started, Stopping: s.ctx.Err() != nil,
		Budget: uint64(s.app.Resources.MemoryBudget), Reserve: uint64(s.app.Resources.ReserveForSystem),
		System: proc.SystemMemory(), Usage: s.group.Usage(), Runtime: s.runtime,
	}
	for _, u := range s.order {
		st.Services = append(st.Services, u.status())
	}
	return st
}

func (s *Supervisor) Events(since uint64) []Event      { return s.events.since(since) }
func (s *Supervisor) Follow() (chan Event, func())     { return s.events.follow() }
func (s *Supervisor) App() *config.Application         { return s.app }

var errUnknown = errors.New("no such service")

// Control: "start", "stop" or "restart" a service.
func (s *Supervisor) Control(name, action string) error {
	u := s.units[name]
	if u == nil {
		return fmt.Errorf("%w: %s", errUnknown, name)
	}
	if s.ctx.Err() != nil {
		return errors.New("the application is stopping")
	}
	switch action {
	case "start", "stop", "restart":
	default:
		return fmt.Errorf("unknown action %q", action)
	}
	select {
	case u.cmds <- action:
		return nil
	case <-time.After(5 * time.Second):
		return errors.New("the service is busy, try again")
	}
}

// prefixWriter writes service output line by line, as "name | line".
type prefixWriter struct {
	mu    sync.Mutex
	w     io.Writer
	width int
}

func (p *prefixWriter) line(name, text string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	fmt.Fprintf(p.w, "%-*s | %s\n", p.width, name, strings.TrimRight(text, "\r\n"))
}

func mb(b uint64) string { return fmt.Sprintf("%.1f MB", float64(b)/(1<<20)) }
