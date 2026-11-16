//go:build !windows

package osctl

import (
	"bufio"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

type unixUnit struct{ pid int }

func Prepare(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// Contain uses a process group on unix; hard memory caps belong to
// cgroups/systemd and are not enforced here.
func Contain(cmd *exec.Cmd, memMB int) (Unit, error) {
	return &unixUnit{pid: cmd.Process.Pid}, nil
}

func (u *unixUnit) Kill() error      { return syscall.Kill(-u.pid, syscall.SIGKILL) }
func (u *unixUnit) Interrupt() error { return syscall.Kill(-u.pid, syscall.SIGTERM) }
func (u *unixUnit) Close()           {}

func (u *unixUnit) MemBytes() uint64 {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(u.pid) + "/statm")
	if err != nil {
		return 0
	}
	f := strings.Fields(string(b))
	if len(f) < 2 {
		return 0
	}
	n, _ := strconv.ParseUint(f[1], 10, 64)
	return n * uint64(os.Getpagesize())
}

func AvailableMB() int {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return -1
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		if strings.HasPrefix(s.Text(), "MemAvailable:") {
			kb, _ := strconv.Atoi(strings.Fields(s.Text())[1])
			return kb >> 10
		}
	}
	return -1
}
