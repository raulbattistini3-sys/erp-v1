package proc

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// SystemMemory reads /proc/meminfo.
func SystemMemory() System {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return System{}
	}
	defer f.Close()
	var s System
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 {
			continue
		}
		kb, _ := strconv.ParseUint(fields[1], 10, 64)
		switch fields[0] {
		case "MemTotal:":
			s.TotalBytes = kb << 10
		case "MemAvailable:":
			s.AvailableBytes = kb << 10
		}
	}
	if s.TotalBytes > 0 {
		s.LoadPercent = int(100 - s.AvailableBytes*100/s.TotalBytes)
	}
	return s
}

// The cgroup v2 tree polyvisor manages: <own cgroup>/polyvisor-<app>/<service>-<n>. Delegation is
// needed (systemd-run --user --scope -p Delegate=yes, a container, or root); without it services run
// without limits and Start says so once.
type linuxGroup struct {
	app    string
	dir    string // "" when cgroups aren't available
	why    error
	events chan Event
	seq    int
	mu     sync.Mutex
	procs  map[string]*linuxProc
	stop   chan struct{}
}

func NewGroup(app string, budget uint64) (Group, error) {
	g := &linuxGroup{app: app, events: make(chan Event, 256), procs: map[string]*linuxProc{}, stop: make(chan struct{})}
	g.dir, g.why = setupCgroup(app, budget)
	if g.dir != "" {
		go g.watch()
	}
	return g, nil
}

func setupCgroup(app string, budget uint64) (string, error) {
	data, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return "", err
	}
	own := ""
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.HasPrefix(line, "0::") {
			own = strings.TrimPrefix(line, "0::")
		}
	}
	if own == "" {
		return "", errors.New("no cgroup v2 hierarchy")
	}
	base := filepath.Join("/sys/fs/cgroup", own)
	// no processes in a cgroup with children (the "no internal processes" rule): polyvisor moves itself
	// into a leaf of its own first
	self := filepath.Join(base, "polyvisor-self")
	if err := os.MkdirAll(self, 0o755); err != nil {
		return "", fmt.Errorf("cgroup %s not delegated: %w", base, err)
	}
	if err := os.WriteFile(filepath.Join(self, "cgroup.procs"), []byte(strconv.Itoa(os.Getpid())), 0); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(base, "cgroup.subtree_control"), []byte("+memory +cpu"), 0); err != nil {
		return "", fmt.Errorf("enabling the memory controller: %w", err)
	}
	dir := filepath.Join(base, "polyvisor-"+app)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, "cgroup.subtree_control"), []byte("+memory +cpu"), 0); err != nil {
		return "", err
	}
	if budget > 0 {
		if err := os.WriteFile(filepath.Join(dir, "memory.max"), []byte(strconv.FormatUint(budget, 10)), 0); err != nil {
			return "", err
		}
	}
	return dir, nil
}

func (g *linuxGroup) Start(cmd *exec.Cmd, lim Limits) (Started, error) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	cmd.SysProcAttr.Pdeathsig = syscall.SIGKILL // polyvisor gone: so is the service
	p := &linuxProc{}
	var warn error
	if g.dir != "" {
		g.mu.Lock()
		g.seq++
		dir := filepath.Join(g.dir, fmt.Sprintf("%s-%d", lim.Service, g.seq))
		g.mu.Unlock()
		if err := os.Mkdir(dir, 0o755); err != nil {
			warn = err
		} else {
			if lim.HardBytes > 0 {
				warn = errors.Join(warn, os.WriteFile(filepath.Join(dir, "memory.max"), []byte(strconv.FormatUint(lim.HardBytes, 10)), 0))
			}
			if lim.SoftBytes > 0 {
				warn = errors.Join(warn, os.WriteFile(filepath.Join(dir, "memory.high"), []byte(strconv.FormatUint(lim.SoftBytes, 10)), 0))
			}
			if w := cpuWeight(lim.Priority); w > 0 {
				_ = os.WriteFile(filepath.Join(dir, "cpu.weight"), []byte(strconv.Itoa(w)), 0)
			}
			fd, err := syscall.Open(dir, syscall.O_DIRECTORY|syscall.O_RDONLY, 0)
			if err != nil {
				warn = errors.Join(warn, err)
			} else {
				defer syscall.Close(fd)
				// clone3 straight into the cgroup: limited from the first instruction
				cmd.SysProcAttr.UseCgroupFD = true
				cmd.SysProcAttr.CgroupFD = fd
				p.cgroup = dir
				cmd.Env = append(cmd.Env, "POLYVISOR_CGROUP="+dir)
			}
		}
	} else if g.why != nil {
		warn = fmt.Errorf("no limits (cgroups: %v)", g.why)
		g.why = nil // said once
	}
	if err := cmd.Start(); err != nil {
		if p.cgroup != "" {
			_ = os.Remove(p.cgroup)
		}
		return nil, err
	}
	p.pid = cmd.Process.Pid
	p.service = lim.Service
	if n := nice(lim.Priority); n != 0 {
		_ = syscall.Setpriority(syscall.PRIO_PGRP, p.pid, n)
	}
	if p.cgroup != "" {
		g.mu.Lock()
		g.procs[p.cgroup] = p
		g.mu.Unlock()
	}
	return p, warn
}

func cpuWeight(priority string) int {
	switch priority {
	case "background":
		return 50
	case "idle":
		return 10
	case "high":
		return 200
	}
	return 0
}

func nice(priority string) int {
	switch priority {
	case "background":
		return 10
	case "idle":
		return 19
	}
	return 0
}

// watch turns memory.events counters (high, max, oom_kill) into events: cgroups have no completion port,
// the counters are polled.
func (g *linuxGroup) watch() {
	seen := map[string]map[string]uint64{}
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-g.stop:
			return
		case <-t.C:
		}
		g.mu.Lock()
		procs := make([]*linuxProc, 0, len(g.procs))
		for _, p := range g.procs {
			procs = append(procs, p)
		}
		g.mu.Unlock()
		for _, p := range procs {
			counts := readKV(filepath.Join(p.cgroup, "memory.events"))
			prev := seen[p.cgroup]
			for key, kind := range map[string]string{"high": "soft-limit", "max": "hard-limit", "oom_kill": "abnormal-exit"} {
				if counts[key] > prev[key] {
					select {
					case g.events <- Event{Service: p.service, Kind: kind}:
					default:
					}
				}
			}
			seen[p.cgroup] = counts
		}
	}
}

func readKV(path string) map[string]uint64 {
	out := map[string]uint64{}
	data, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	for _, line := range bytes.Split(data, []byte("\n")) {
		f := strings.Fields(string(line))
		if len(f) == 2 {
			out[f[0]], _ = strconv.ParseUint(f[1], 10, 64)
		}
	}
	return out
}

func readUint(path string) uint64 {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	v, _ := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
	return v
}

func (g *linuxGroup) Events() <-chan Event { return g.events }

func (g *linuxGroup) Usage() Usage {
	if g.dir == "" {
		return Usage{}
	}
	return cgroupUsage(g.dir)
}

func (g *linuxGroup) Interrupt() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, p := range g.procs {
		_ = syscall.Kill(-p.pid, syscall.SIGINT)
	}
	return nil
}

func (g *linuxGroup) Close() {
	select {
	case <-g.stop:
	default:
		close(g.stop)
	}
}

type linuxProc struct {
	pid     int
	service string
	cgroup  string
}

func (p *linuxProc) PID() int { return p.pid }

func (p *linuxProc) Usage() Usage {
	if p.cgroup == "" {
		return Usage{Processes: 1, PIDs: []int{p.pid}, MemoryBytes: rss(p.pid)}
	}
	return cgroupUsage(p.cgroup)
}

func cgroupUsage(dir string) Usage {
	u := Usage{MemoryBytes: readUint(filepath.Join(dir, "memory.current")), PeakBytes: readUint(filepath.Join(dir, "memory.peak"))}
	cpu := readKV(filepath.Join(dir, "cpu.stat"))
	u.CPU = time.Duration(cpu["usage_usec"]) * time.Microsecond
	if data, err := os.ReadFile(filepath.Join(dir, "cgroup.procs")); err == nil {
		for _, f := range strings.Fields(string(data)) {
			if pid, err := strconv.Atoi(f); err == nil {
				u.PIDs = append(u.PIDs, pid)
			}
		}
	}
	u.Processes = len(u.PIDs)
	return u
}

func rss(pid int) uint64 {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/statm", pid))
	if err != nil {
		return 0
	}
	f := strings.Fields(string(data))
	if len(f) < 2 {
		return 0
	}
	pages, _ := strconv.ParseUint(f[1], 10, 64)
	return pages * uint64(os.Getpagesize())
}

func (p *linuxProc) Signal(kind string) error {
	switch kind {
	case "interrupt":
		return syscall.Kill(-p.pid, syscall.SIGINT)
	case "break", "terminate":
		return syscall.Kill(-p.pid, syscall.SIGTERM)
	}
	return p.KillTree()
}

func (p *linuxProc) KillTree() error {
	if p.cgroup != "" {
		if err := os.WriteFile(filepath.Join(p.cgroup, "cgroup.kill"), []byte("1"), 0); err == nil {
			return nil
		}
	}
	return syscall.Kill(-p.pid, syscall.SIGKILL)
}

func (p *linuxProc) Release() {
	if p.cgroup != "" {
		// the cgroup empties a moment after the last process is reaped
		for i := 0; i < 20 && os.Remove(p.cgroup) != nil; i++ {
			time.Sleep(50 * time.Millisecond)
		}
	}
}
