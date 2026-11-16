package supervisor

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/raulbattistini3-sys/erp-v1/supervisor/internal/config"
	"github.com/raulbattistini3-sys/erp-v1/supervisor/internal/proc"
)

// Service states.
const (
	Pending  = "pending"  // not started yet
	Waiting  = "waiting"  // for its dependencies to be ready
	Deferred = "deferred" // admission: the machine or the budget has no room for it now
	Starting = "starting" // running, not ready yet
	Ready    = "ready"
	Backoff  = "backoff" // exited, restarting after a delay
	Stopping = "stopping"
	Stopped  = "stopped" // stopped on request, or exited for good by its policy
	Failed   = "failed"  // gave up after maxRestarts
)

// ServiceStatus: what status shows for a service.
type ServiceStatus struct {
	Name      string     `json:"name"`
	State     string     `json:"state"`
	Detail    string     `json:"detail,omitempty"`
	PID       int        `json:"pid,omitempty"`
	Required  bool       `json:"required"`
	DependsOn []string   `json:"dependsOn,omitempty"`
	Restarts  int        `json:"restarts"`
	StartedAt *time.Time `json:"startedAt,omitempty"`
	ReadyAt   *time.Time `json:"readyAt,omitempty"`
	LastExit  *Exit      `json:"lastExit,omitempty"`
	Usage     proc.Usage `json:"usage"`
	SoftBytes uint64     `json:"softBytes,omitempty"`
	HardBytes uint64     `json:"hardBytes,omitempty"`
	Pressure  string     `json:"pressure"`
	Priority  string     `json:"priority,omitempty"`
	Limits    struct {
		Soft int `json:"soft"`
		Hard int `json:"hard"`
	} `json:"limitEvents"`
}

type Exit struct {
	Code   int       `json:"code"`
	At     time.Time `json:"at"`
	Reason string    `json:"reason,omitempty"` // "hard-limit" when the memory limit preceded it
	Uptime string    `json:"uptime"`
}

type unit struct {
	s          *Supervisor
	svc        *config.Service
	dependents []*unit
	cmds       chan string
	done       chan struct{} // closed when the unit has stopped for good (shutdown)

	mu        sync.Mutex
	state     string
	detail    string
	running   proc.Started
	cmd       *exec.Cmd
	exited    chan struct{}
	startedAt time.Time
	readyAt   time.Time
	restarts  []time.Time
	total     int
	lastExit  *Exit
	pressure  string
	last      proc.Usage
	softHits  int
	hardHits  int
	lastHard  time.Time
}

func newUnit(s *Supervisor, svc *config.Service) *unit {
	return &unit{s: s, svc: svc, cmds: make(chan string), done: make(chan struct{}), state: Pending, pressure: "normal"}
}

func (u *unit) set(state, detail string) {
	u.mu.Lock()
	changed := u.state != state || u.detail != detail
	u.state, u.detail = state, detail
	u.mu.Unlock()
	if changed {
		u.s.bump()
	}
}

func (u *unit) get() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.state
}

func (u *unit) emit(kind string, pid int, detail string, data map[string]any) {
	u.s.emit(Event{Kind: kind, Service: u.svc.Name, PID: pid, Detail: detail, Data: data})
}

// run owns the service's process: it starts it, restarts it, and stops it, until the shutdown.
func (u *unit) run() {
	defer close(u.done)
	want := true
	backoff := u.svc.Restart.Backoff.Or(time.Second)
	for {
		if u.s.ctx.Err() != nil {
			u.waitDependents()
			u.set(Stopped, "")
			return
		}
		if !want {
			u.set(Stopped, "on request")
			select {
			case <-u.s.ctx.Done():
				continue
			case c := <-u.cmds:
				want = c != "stop"
				continue
			}
		}
		if ok, c := u.waitDependencies(); !ok {
			if c == "stop" {
				want = false
			}
			continue
		}
		if ok, c := u.admit(); !ok {
			if c == "stop" {
				want = false
			}
			continue
		}

		out := u.once()
		switch out.kind {
		case "shutdown":
			continue
		case "stopped":
			want = false
			continue
		case "restart":
			backoff = u.svc.Restart.Backoff.Or(time.Second)
			continue
		}
		// it exited by itself
		policy := u.svc.Restart.Policy
		if policy == "never" || (policy == "on-failure" && out.code == 0) {
			u.set(Stopped, fmt.Sprintf("exited %d (restart: %s)", out.code, orDefault(policy, "always")))
			if u.svc.Required && out.code != 0 {
				u.s.fail(u, fmt.Sprintf("exited %d and restart is %s", out.code, policy))
			} else if u.svc.Required {
				// the application can't run without it, and it finished: the application is done (exit 0)
				u.emit("completed", 0, "required service finished; stopping the application", nil)
				u.s.Shutdown(false)
			}
			want = false
			continue
		}
		if u.gaveUp() {
			why := fmt.Sprintf("%d restarts within %v", u.svc.Restart.MaxRestarts, u.svc.Restart.Window.Or(5*time.Minute))
			u.set(Failed, why)
			u.emit("failed", 0, why, nil)
			if u.svc.Required {
				u.s.fail(u, why)
			}
			want = false
			continue
		}
		if out.uptime > time.Minute {
			backoff = u.svc.Restart.Backoff.Or(time.Second) // it ran for a while: start over
		}
		u.set(Backoff, fmt.Sprintf("restarting in %v", backoff))
		u.emit("backoff", 0, fmt.Sprintf("exit %d, restarting in %v", out.code, backoff), nil)
		select {
		case <-u.s.ctx.Done():
		case <-time.After(backoff):
		case c := <-u.cmds:
			if c == "stop" {
				want = false
			}
		}
		backoff = min(backoff*2, u.svc.Restart.MaxBackoff.Or(30*time.Second))
	}
}

func (u *unit) gaveUp() bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	now := time.Now()
	u.restarts = append(u.restarts, now)
	u.total++
	window := u.svc.Restart.Window.Or(5 * time.Minute)
	kept := u.restarts[:0]
	for _, t := range u.restarts {
		if now.Sub(t) <= window {
			kept = append(kept, t)
		}
	}
	u.restarts = kept
	return u.svc.Restart.MaxRestarts > 0 && len(kept) > u.svc.Restart.MaxRestarts
}

// waitDependencies until each dependency is ready. false: interrupted (shutdown, or a command).
func (u *unit) waitDependencies() (bool, string) {
	for {
		var missing []string
		for _, d := range u.svc.DependsOn {
			if u.s.units[d].get() != Ready {
				missing = append(missing, d)
			}
		}
		if len(missing) == 0 {
			return true, ""
		}
		u.set(Waiting, "for "+strings.Join(missing, ", "))
		select {
		case <-u.s.ctx.Done():
			return false, ""
		case c := <-u.cmds:
			return false, c
		case <-u.s.changed():
		}
	}
}

// admit: required services always start. The others wait while the machine has less free memory than
// reserveForSystem, or while the application's budget has less room than the service's soft limit.
func (u *unit) admit() (bool, string) {
	if u.svc.Required {
		return true, ""
	}
	said := ""
	for {
		why := u.s.admission(u.svc)
		if why == "" {
			if said != "" {
				u.emit("admitted", 0, "room again", nil)
			}
			return true, ""
		}
		if why != said {
			u.set(Deferred, why)
			u.emit("deferred", 0, why, nil)
			said = why
		}
		select {
		case <-u.s.ctx.Done():
			return false, ""
		case c := <-u.cmds:
			if c == "start" || c == "restart" { // an explicit start skips admission
				return true, ""
			}
			return false, c
		case <-time.After(2 * time.Second):
		}
	}
}

func (s *Supervisor) admission(svc *config.Service) string {
	if reserve := uint64(s.app.Resources.ReserveForSystem); reserve > 0 {
		if sys := proc.SystemMemory(); sys.TotalBytes > 0 && sys.AvailableBytes < reserve {
			return fmt.Sprintf("%s free on the machine, reserveForSystem is %v", mb(sys.AvailableBytes), s.app.Resources.ReserveForSystem)
		}
	}
	if budget := uint64(s.app.Resources.MemoryBudget); budget > 0 {
		need := uint64(svc.Memory.SoftLimit)
		if need == 0 {
			need = uint64(svc.Memory.HardLimit)
		}
		if used := s.group.Usage().MemoryBytes; used+need > budget {
			return fmt.Sprintf("the services use %s of the %v budget, %s needs %s", mb(used), s.app.Resources.MemoryBudget, svc.Name, mb(need))
		}
	}
	return ""
}

type outcome struct {
	kind   string // exited, stopped, restart, shutdown
	code   int
	uptime time.Duration
}

// once: one start of the service, until it exits, is stopped, or the application shuts down.
func (u *unit) once() outcome {
	cmd, err := u.command()
	if err != nil {
		u.emit("failed", 0, err.Error(), nil)
		return outcome{kind: "exited", code: -1}
	}
	// os pipes, not exec's: Wait must not wait for the readers, since a process the service started may
	// hold the pipe open after the service itself exited
	r, w, err := os.Pipe()
	if err != nil {
		u.emit("failed", 0, err.Error(), nil)
		return outcome{kind: "exited", code: -1}
	}
	cmd.Stdout, cmd.Stderr = w, w
	lim := proc.Limits{
		Service: u.svc.Name, SoftBytes: uint64(u.svc.Memory.SoftLimit), HardBytes: uint64(u.svc.Memory.HardLimit),
		Priority: u.svc.Scheduling.Priority, OwnGroup: u.svc.Stop.Signal == "break",
	}
	u.set(Starting, "")
	started, err := u.s.group.Start(cmd, lim)
	w.Close() // the service has its own copy
	if started == nil {
		r.Close()
		u.emit("failed", 0, fmt.Sprintf("could not start %s: %v", cmd.Path, err), nil)
		return outcome{kind: "exited", code: -1}
	}
	if err != nil {
		u.emit("warning", started.PID(), "started without its limits: "+err.Error(), nil)
	}
	go func() { defer r.Close(); u.forward(r) }()
	exited := make(chan struct{})
	code := 0
	go func() {
		werr := cmd.Wait()
		if cmd.ProcessState != nil {
			code = cmd.ProcessState.ExitCode()
		} else if werr != nil {
			code = -1
		}
		close(exited)
	}()

	now := time.Now()
	u.mu.Lock()
	u.running, u.cmd, u.exited, u.startedAt, u.readyAt = started, cmd, exited, now, time.Time{}
	u.mu.Unlock()
	u.emit("started", started.PID(), fmt.Sprintf("%s %s", filepath.Base(cmd.Path), strings.Join(u.svc.Args, " ")), map[string]any{
		"softBytes": lim.SoftBytes, "hardBytes": lim.HardBytes, "priority": lim.Priority,
	})

	readyCh := make(chan error, 1)
	go func() { readyCh <- u.probe(exited) }()

	result := func(kind string) outcome {
		<-exited
		started.Release()
		uptime := time.Since(now)
		reason := ""
		u.mu.Lock()
		if !u.lastHard.IsZero() && time.Since(u.lastHard) < 5*time.Second {
			reason = "hard-limit"
		}
		u.lastExit = &Exit{Code: code, At: time.Now(), Reason: reason, Uptime: uptime.Round(time.Millisecond).String()}
		u.running, u.cmd = nil, nil
		u.pressure = "normal"
		u.mu.Unlock()
		detail := fmt.Sprintf("code %d after %v", code, uptime.Round(time.Millisecond))
		if reason != "" {
			detail += " (" + reason + ")"
		}
		u.emit("exited", started.PID(), detail, map[string]any{"code": code, "reason": reason})
		return outcome{kind: kind, code: code, uptime: uptime}
	}

	for {
		select {
		case err := <-readyCh:
			readyCh = nil
			if err != nil {
				u.emit("failed", started.PID(), "not ready: "+err.Error(), nil)
				_ = started.KillTree()
				return result("exited")
			}
			u.mu.Lock()
			u.readyAt = time.Now()
			u.mu.Unlock()
			u.set(Ready, "")
			u.emit("ready", started.PID(), fmt.Sprintf("after %v", time.Since(now).Round(time.Millisecond)), nil)
		case <-exited:
			return result("exited")
		case c := <-u.cmds:
			switch c {
			case "stop", "restart":
				u.stop(started, exited, false)
				if c == "stop" {
					return result("stopped")
				}
				return result("restart")
			}
		case <-u.s.ctx.Done():
			// dependents first: they may still be finishing work against this one
			u.waitDependents()
			u.stop(started, exited, u.s.console.Load())
			return result("shutdown")
		}
	}
}

func (u *unit) waitDependents() {
	for _, d := range u.dependents {
		<-d.doneOrStopped()
	}
}

// doneOrStopped: closed once the unit has no process left during the shutdown.
func (u *unit) doneOrStopped() <-chan struct{} { return u.done }

// stop asks the service to stop the way its config says and kills the whole tree after the grace period.
func (u *unit) stop(p proc.Started, exited chan struct{}, fromConsole bool) {
	signal := orDefault(u.svc.Stop.Signal, "interrupt")
	grace := u.svc.Stop.Grace.Or(20 * time.Second)
	u.set(Stopping, signal)
	u.emit("stopping", p.PID(), fmt.Sprintf("%s, %v grace", signal, grace), nil)
	var err error
	switch {
	case signal == "kill":
		err = p.KillTree()
	case signal == "interrupt" && runtime.GOOS == "windows":
		if !fromConsole {
			// a single process can't be sent Ctrl+C on Windows without the whole console getting it
			err = p.KillTree()
		}
	default:
		err = p.Signal(signal)
	}
	if err != nil {
		_ = p.KillTree()
	}
	select {
	case <-exited:
	case <-time.After(grace):
		u.emit("warning", p.PID(), fmt.Sprintf("still running after %v, killing its process tree", grace), nil)
		_ = p.KillTree()
		<-exited
	}
}

func (u *unit) kill() {
	u.mu.Lock()
	p := u.running
	u.mu.Unlock()
	if p != nil {
		_ = p.KillTree()
	}
}

// probe: nil once the service is ready by its config (at once without one), an error past the timeout.
func (u *unit) probe(exited chan struct{}) error {
	r := u.svc.Ready
	if r.Delay > 0 {
		select {
		case <-exited:
			return errors.New("exited")
		case <-time.After(time.Duration(r.Delay)):
		}
	}
	if r.TCP == "" && r.HTTP == "" {
		return nil
	}
	timeout := r.Timeout.Or(120 * time.Second)
	deadline := time.Now().Add(timeout)
	addr := r.TCP
	if addr != "" && !strings.Contains(addr, ":") {
		addr = "127.0.0.1:" + addr
	}
	client := &http.Client{Timeout: 2 * time.Second}
	for time.Now().Before(deadline) {
		ok := true
		if addr != "" {
			c, err := net.DialTimeout("tcp", addr, time.Second)
			if err != nil {
				ok = false
			} else {
				c.Close()
			}
		}
		if ok && r.HTTP != "" {
			resp, err := client.Get(r.HTTP)
			if err != nil {
				ok = false
			} else {
				resp.Body.Close()
				ok = resp.StatusCode < 400
			}
		}
		if ok {
			return nil
		}
		select {
		case <-exited:
			return errors.New("exited")
		case <-time.After(250 * time.Millisecond):
		}
	}
	return fmt.Errorf("no answer from %s within %v", strings.TrimSpace(addr+" "+r.HTTP), timeout)
}

// command builds the process: the command from the config (relative paths against the application's
// folder, bare names on PATH), polyvisor's environment, the application's env, the service's env, and
// the POLYVISOR_* variables that tell the service its limits.
func (u *unit) command() (*exec.Cmd, error) {
	app := u.s.app
	path := u.svc.Command
	if strings.ContainsAny(path, `/\`) {
		path = app.Path(path)
	} else if found, err := exec.LookPath(path); err == nil {
		path = found
	} else if local := app.Path(path); fileExists(local) || fileExists(local+".exe") {
		path = local
	} else {
		return nil, fmt.Errorf("%s: not found on PATH or in %s", u.svc.Command, app.Dir)
	}
	cmd := exec.Command(path, u.svc.Args...)
	cmd.Dir = app.Dir
	if u.svc.Cwd != "" {
		cmd.Dir = app.Path(u.svc.Cwd)
	}
	env := os.Environ()
	for k, v := range app.Env {
		env = append(env, k+"="+v)
	}
	for k, v := range u.svc.Env {
		env = append(env, k+"="+v)
	}
	env = append(env,
		"POLYVISOR=1",
		"POLYVISOR_APP="+app.Name,
		"POLYVISOR_SERVICE="+u.svc.Name,
		"POLYVISOR_PID="+strconv.Itoa(os.Getpid()),
		"POLYVISOR_MEMORY_SOFT_BYTES="+strconv.FormatUint(uint64(u.svc.Memory.SoftLimit), 10),
		"POLYVISOR_MEMORY_HARD_BYTES="+strconv.FormatUint(uint64(u.svc.Memory.HardLimit), 10),
		"POLYVISOR_MEMORY_BUDGET_BYTES="+strconv.FormatUint(uint64(app.Resources.MemoryBudget), 10),
		"POLYVISOR_PRIORITY="+orDefault(u.svc.Scheduling.Priority, "normal"),
		"POLYVISOR_MAX_CONCURRENCY="+strconv.Itoa(u.svc.Scheduling.MaxConcurrency),
	)
	if u.s.opts.ControlURL != "" {
		env = append(env, "POLYVISOR_CONTROL_URL="+u.s.opts.ControlURL, "POLYVISOR_CONTROL_TOKEN="+u.s.opts.Token)
	}
	if u.s.runtime != "" {
		env = append(env, "POLYVISOR_RUNTIME="+u.s.runtime)
	}
	cmd.Env = env
	return cmd, nil
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

func (u *unit) forward(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		u.s.out.line(u.svc.Name, sc.Text())
	}
	if err := sc.Err(); err != nil {
		u.s.out.line(u.svc.Name, fmt.Sprintf("[polyvisor: output dropped: %v]", err))
	}
	// a line over 1 MB ends the scan: drain the rest so the service never blocks on a full pipe
	_, _ = io.Copy(io.Discard, r)
}

func (u *unit) usage() proc.Usage {
	u.mu.Lock()
	p := u.running
	u.mu.Unlock()
	if p == nil {
		return proc.Usage{}
	}
	return p.Usage()
}

func (u *unit) noteLimit(kind string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	switch kind {
	case "soft-limit":
		u.softHits++
	case "hard-limit", "process-limit":
		u.hardHits++
		u.lastHard = time.Now()
	}
}

func (u *unit) sample() {
	usage := u.usage()
	soft, hard := uint64(u.svc.Memory.SoftLimit), uint64(u.svc.Memory.HardLimit)
	level := "normal"
	switch {
	case hard > 0 && usage.MemoryBytes >= hard*95/100:
		level = "critical"
	case soft > 0 && usage.MemoryBytes >= soft:
		level = "elevated"
	}
	u.mu.Lock()
	u.last = usage
	prev := u.pressure
	running := u.running != nil
	u.pressure = level
	u.mu.Unlock()
	if running && level != prev {
		u.emit("pressure", 0, fmt.Sprintf("%s → %s (%s committed)", prev, level, mb(usage.MemoryBytes)), map[string]any{
			"level": level, "memoryBytes": usage.MemoryBytes, "softBytes": soft, "hardBytes": hard,
		})
	}
}

func (u *unit) status() ServiceStatus {
	u.mu.Lock()
	defer u.mu.Unlock()
	st := ServiceStatus{
		Name: u.svc.Name, State: u.state, Detail: u.detail, Required: u.svc.Required, DependsOn: u.svc.DependsOn,
		Restarts: u.total, LastExit: u.lastExit, Usage: u.last, Pressure: u.pressure,
		SoftBytes: uint64(u.svc.Memory.SoftLimit), HardBytes: uint64(u.svc.Memory.HardLimit),
		Priority: orDefault(u.svc.Scheduling.Priority, "normal"),
	}
	st.Limits.Soft, st.Limits.Hard = u.softHits, u.hardHits
	if u.running != nil {
		st.PID = u.running.PID()
		t := u.startedAt
		st.StartedAt = &t
		if !u.readyAt.IsZero() {
			r := u.readyAt
			st.ReadyAt = &r
		}
	}
	return st
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
