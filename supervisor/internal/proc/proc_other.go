//go:build !windows && !linux

package proc

import (
	"errors"
	"os/exec"
	"syscall"
)

// Elsewhere (macOS, BSD): processes and process groups, no resource limits.

func SystemMemory() System { return System{} }

type plainGroup struct{ events chan Event }

func NewGroup(app string, budget uint64) (Group, error) { return &plainGroup{events: make(chan Event)}, nil }

func (g *plainGroup) Start(cmd *exec.Cmd, lim Limits) (Started, error) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	var warn error
	if lim.HardBytes > 0 || lim.SoftBytes > 0 {
		warn = errors.New("memory limits are not supported on this platform")
	}
	return &plainProc{pid: cmd.Process.Pid}, warn
}

func (g *plainGroup) Usage() Usage           { return Usage{} }
func (g *plainGroup) Events() <-chan Event   { return g.events }
func (g *plainGroup) Interrupt() error       { return nil }
func (g *plainGroup) Close()                 {}

type plainProc struct{ pid int }

func (p *plainProc) PID() int     { return p.pid }
func (p *plainProc) Usage() Usage { return Usage{Processes: 1, PIDs: []int{p.pid}} }
func (p *plainProc) Signal(kind string) error {
	switch kind {
	case "interrupt":
		return syscall.Kill(-p.pid, syscall.SIGINT)
	case "break", "terminate":
		return syscall.Kill(-p.pid, syscall.SIGTERM)
	}
	return p.KillTree()
}
func (p *plainProc) KillTree() error { return syscall.Kill(-p.pid, syscall.SIGKILL) }
func (p *plainProc) Release()        {}
