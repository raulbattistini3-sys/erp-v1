// Package proc starts a service's process inside the operating system's resource container for it and
// reads it back: on Windows a job object per service, nested in one job for the whole application (the
// memory budget); on Linux a cgroup v2 per service under one for the application. A process is placed
// before it runs its first instruction (Windows: created suspended; Linux: clone into the cgroup), so
// whatever it starts inherits the limits too.
package proc

import (
	"os/exec"
	"time"
)

// Limits for one start of a service.
type Limits struct {
	Service   string
	SoftBytes uint64 // pressure from here on (an event / memory.high)
	HardBytes uint64 // allocations past it fail (job memory limit / memory.max)
	Priority  string // normal, background, idle, high
	OwnGroup  bool   // its own process group, for a CTRL_BREAK / group signal of its own
}

// Usage of a service's whole process tree (or of the application, for the group).
type Usage struct {
	MemoryBytes uint64        `json:"memoryBytes"` // committed (private) memory, what the limits count
	PeakBytes   uint64        `json:"peakBytes"`
	Processes   int           `json:"processes"`
	CPU         time.Duration `json:"cpuNanos"`
	PIDs        []int         `json:"pids,omitempty"`
}

// Event: something the operating system reported about a container, as it happened.
type Event struct {
	Service string // "" for the application's own container
	Kind    string // soft-limit, hard-limit, process-limit, new-process, exit-process, abnormal-exit, empty
	PID     int
}

// System memory, for admission (reserveForSystem).
type System struct {
	TotalBytes     uint64 `json:"totalBytes"`
	AvailableBytes uint64 `json:"availableBytes"`
	LoadPercent    int    `json:"loadPercent"`
}

// Handle names the container a service lives in, for the service itself (its env).
type Handle struct {
	Env []string // POLYVISOR_JOB / WIN_TELEMETRY_JOB (Windows), POLYVISOR_CGROUP (Linux)
}

// Started is a running service process in its container.
type Started interface {
	PID() int
	Usage() Usage
	// Signal asks the service to stop: "interrupt", "break", "terminate"; "kill" ends the whole tree.
	Signal(kind string) error
	// KillTree ends the process and everything it started.
	KillTree() error
	// Release frees the container once the process is gone.
	Release()
}

// Group is the application's container; services are started into it.
type Group interface {
	Start(cmd *exec.Cmd, lim Limits) (Started, error)
	Usage() Usage
	Events() <-chan Event
	// Interrupt delivers Ctrl+C / SIGINT to every service sharing the console's process group.
	Interrupt() error
	Close()
}
