package proc

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	kernel32                 = windows.NewLazySystemDLL("kernel32.dll")
	ntdll                    = windows.NewLazySystemDLL("ntdll.dll")
	procGlobalMemoryStatusEx = kernel32.NewProc("GlobalMemoryStatusEx")
	procGetProcessMemoryInfo = kernel32.NewProc("K32GetProcessMemoryInfo")
	procNtResumeProcess      = ntdll.NewProc("NtResumeProcess")
)

const (
	createSuspended       = 0x00000004
	createNewProcessGroup = 0x00000200
	processSuspendResume  = 0x0800

	jobObjectLimitProcessMemory = 0x00000100
	jobObjectLimitJobMemory     = 0x00000200
	jobObjectLimitPriorityClass = 0x00000020
	jobObjectLimitKillOnClose   = 0x00002000
	jobObjectLimitBreakawayOK   = 0x00000800

	classBasicAccounting   = 1
	classBasicProcessIDs   = 3
	classAssociatePort     = 7
	classExtendedLimit     = 9
	classNotificationLimit = 12

	msgEndOfJobTime          = 1
	msgActiveProcessZero     = 4
	msgNewProcess            = 6
	msgExitProcess           = 7
	msgAbnormalExitProcess   = 8
	msgProcessMemoryLimit    = 9
	msgJobMemoryLimit        = 10
	msgNotificationLimit     = 11
	infinite                 = 0xFFFFFFFF
	belowNormalPriorityClass = 0x00004000
	idlePriorityClass        = 0x00000040
	normalPriorityClass      = 0x00000020
	abovePriorityClass       = 0x00008000
)

type ioCounters struct{ ReadOps, WriteOps, OtherOps, ReadBytes, WriteBytes, OtherBytes uint64 }

type basicLimit struct {
	PerProcessUserTimeLimit int64
	PerJobUserTimeLimit     int64
	LimitFlags              uint32
	MinimumWorkingSetSize   uintptr
	MaximumWorkingSetSize   uintptr
	ActiveProcessLimit      uint32
	Affinity                uintptr
	PriorityClass           uint32
	SchedulingClass         uint32
}

type extendedLimit struct {
	Basic                 basicLimit
	IO                    ioCounters
	ProcessMemoryLimit    uintptr
	JobMemoryLimit        uintptr
	PeakProcessMemoryUsed uintptr
	PeakJobMemoryUsed     uintptr
}

type notificationLimit struct {
	IoReadBytesLimit             uint64
	IoWriteBytesLimit            uint64
	PerJobUserTimeLimit          int64
	JobMemoryLimit               uint64
	RateControlTolerance         int32
	RateControlToleranceInterval int32
	LimitFlags                   uint32
}

type basicAccounting struct {
	TotalUserTime             int64
	TotalKernelTime           int64
	ThisPeriodTotalUserTime   int64
	ThisPeriodTotalKernelTime int64
	TotalPageFaultCount       uint32
	TotalProcesses            uint32
	ActiveProcesses           uint32
	TotalTerminatedProcesses  uint32
}

type associatePort struct {
	CompletionKey  uintptr
	CompletionPort windows.Handle
}

type memoryCountersEx struct {
	Cb                         uint32
	PageFaultCount             uint32
	PeakWorkingSetSize         uintptr
	WorkingSetSize             uintptr
	QuotaPeakPagedPoolUsage    uintptr
	QuotaPagedPoolUsage        uintptr
	QuotaPeakNonPagedPoolUsage uintptr
	QuotaNonPagedPoolUsage     uintptr
	PagefileUsage              uintptr
	PeakPagefileUsage          uintptr
	PrivateUsage               uintptr
}

type memoryStatusEx struct {
	Length               uint32
	MemoryLoad           uint32
	TotalPhys            uint64
	AvailPhys            uint64
	TotalPageFile        uint64
	AvailPageFile        uint64
	TotalVirtual         uint64
	AvailVirtual         uint64
	AvailExtendedVirtual uint64
}

// SystemMemory reads the machine's physical memory.
func SystemMemory() System {
	var m memoryStatusEx
	m.Length = uint32(unsafe.Sizeof(m))
	if r, _, _ := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&m))); r == 0 {
		return System{}
	}
	return System{TotalBytes: m.TotalPhys, AvailableBytes: m.AvailPhys, LoadPercent: int(m.MemoryLoad)}
}

type winGroup struct {
	app    string
	job    windows.Handle
	port   windows.Handle
	events chan Event
	mu     sync.Mutex
	keys   map[uintptr]string // completion key → service
	seq    atomic.Uint32
	closed atomic.Bool
}

// NewGroup creates the application's job: everything in it dies with polyvisor (kill on close), and
// together it may commit at most budget bytes (0: no budget).
func NewGroup(app string, budget uint64) (Group, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("application job: %w", err)
	}
	info := extendedLimit{}
	info.Basic.LimitFlags = jobObjectLimitKillOnClose | jobObjectLimitBreakawayOK
	if budget > 0 {
		info.Basic.LimitFlags |= jobObjectLimitJobMemory
		info.JobMemoryLimit = uintptr(budget)
	}
	if err := setInfo(job, classExtendedLimit, &info); err != nil {
		windows.CloseHandle(job)
		return nil, fmt.Errorf("application job limits: %w", err)
	}
	port, err := windows.CreateIoCompletionPort(windows.InvalidHandle, 0, 0, 1)
	if err != nil {
		windows.CloseHandle(job)
		return nil, err
	}
	g := &winGroup{app: app, job: job, port: port, events: make(chan Event, 256), keys: map[uintptr]string{0: ""}}
	if err := setInfo(job, classAssociatePort, &associatePort{CompletionKey: 0, CompletionPort: port}); err != nil {
		g.Close()
		return nil, err
	}
	go g.listen()
	return g, nil
}

func setInfo[T any](job windows.Handle, class uint32, v *T) error {
	_, err := windows.SetInformationJobObject(job, class, uintptr(unsafe.Pointer(v)), uint32(unsafe.Sizeof(*v)))
	return err
}

func (g *winGroup) listen() {
	for {
		var msg uint32
		var key uintptr
		var ov *windows.Overlapped
		if err := windows.GetQueuedCompletionStatus(g.port, &msg, &key, &ov, infinite); err != nil && ov == nil {
			if g.closed.Load() {
				return
			}
			continue
		}
		if msg == 0 && key == ^uintptr(0) { // Close
			return
		}
		g.mu.Lock()
		service, known := g.keys[key]
		g.mu.Unlock()
		if !known {
			continue
		}
		kind := ""
		switch msg {
		case msgNotificationLimit:
			kind = "soft-limit"
		case msgJobMemoryLimit:
			kind = "hard-limit"
		case msgProcessMemoryLimit:
			kind = "process-limit"
		case msgNewProcess:
			kind = "new-process"
		case msgExitProcess:
			kind = "exit-process"
		case msgAbnormalExitProcess:
			kind = "abnormal-exit"
		case msgActiveProcessZero:
			kind = "empty"
		case msgEndOfJobTime:
			kind = "time-limit"
		default:
			continue
		}
		e := Event{Service: service, Kind: kind, PID: int(uintptr(unsafe.Pointer(ov)))}
		select {
		case g.events <- e:
		default: // nobody keeping up: drop, the polled usage still tells
		}
	}
}

func (g *winGroup) Events() <-chan Event { return g.events }

func (g *winGroup) Start(cmd *exec.Cmd, lim Limits) (Started, error) {
	seq := g.seq.Add(1)
	name := fmt.Sprintf(`Local\polyvisor-%s-%s-%d-%d`, g.app, lim.Service, os.Getpid(), seq)
	cmd.Env = append(cmd.Env, "POLYVISOR_JOB="+name, "WIN_TELEMETRY_JOB="+name)
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= createSuspended
	if lim.OwnGroup {
		cmd.SysProcAttr.CreationFlags |= createNewProcessGroup
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	pid := cmd.Process.Pid
	p := &winProc{pid: pid, ownGroup: lim.OwnGroup}
	h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE|processSuspendResume|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		_ = cmd.Process.Kill()
		return nil, fmt.Errorf("opening the new process: %w", err)
	}
	defer windows.CloseHandle(h)

	// placed while suspended: app job first (it nests under polyvisor's own job), then the service's
	// job, which nests under the app job. A failure leaves the process unlimited, not unstarted.
	var warn []error
	if err := windows.AssignProcessToJobObject(g.job, h); err != nil {
		warn = append(warn, fmt.Errorf("application job: %w", err))
	}
	job, err := g.serviceJob(name, lim)
	if err != nil {
		warn = append(warn, err)
	} else if err := windows.AssignProcessToJobObject(job, h); err != nil {
		windows.CloseHandle(job)
		warn = append(warn, fmt.Errorf("service job: %w", err))
	} else {
		p.job = job
	}
	if r, _, e := procNtResumeProcess.Call(uintptr(h)); int32(r) < 0 {
		_ = windows.TerminateProcess(h, 1)
		p.Release()
		return nil, fmt.Errorf("resuming the new process: NTSTATUS %#x (%v)", uint32(r), e)
	}
	return p, errors.Join(warn...)
}

func (g *winGroup) serviceJob(name string, lim Limits) (windows.Handle, error) {
	wide, _ := windows.UTF16PtrFromString(name)
	job, err := windows.CreateJobObject(nil, wide)
	if err != nil {
		return 0, fmt.Errorf("service job: %w", err)
	}
	info := extendedLimit{}
	if lim.HardBytes > 0 {
		info.Basic.LimitFlags |= jobObjectLimitJobMemory
		info.JobMemoryLimit = uintptr(lim.HardBytes)
	}
	if pc := priorityClass(lim.Priority); pc != 0 {
		info.Basic.LimitFlags |= jobObjectLimitPriorityClass
		info.Basic.PriorityClass = pc
	}
	if err := setInfo(job, classExtendedLimit, &info); err != nil {
		windows.CloseHandle(job)
		return 0, fmt.Errorf("service job limits: %w", err)
	}
	if lim.SoftBytes > 0 {
		n := notificationLimit{JobMemoryLimit: lim.SoftBytes, LimitFlags: jobObjectLimitJobMemory}
		if err := setInfo(job, classNotificationLimit, &n); err != nil {
			windows.CloseHandle(job)
			return 0, fmt.Errorf("service job soft limit: %w", err)
		}
	}
	key := uintptr(job)
	g.mu.Lock()
	g.keys[key] = lim.Service
	g.mu.Unlock()
	if err := setInfo(job, classAssociatePort, &associatePort{CompletionKey: key, CompletionPort: g.port}); err != nil {
		windows.CloseHandle(job)
		return 0, err
	}
	return job, nil
}

func priorityClass(p string) uint32 {
	switch p {
	case "background":
		return belowNormalPriorityClass
	case "idle":
		return idlePriorityClass
	case "high":
		return abovePriorityClass
	}
	return 0
}

func (g *winGroup) Usage() Usage { return jobUsage(g.job) }

func (g *winGroup) Interrupt() error {
	// Ctrl+C to every process on the console that shares its process group, polyvisor included (it
	// is already shutting down when it does this)
	return windows.GenerateConsoleCtrlEvent(windows.CTRL_C_EVENT, 0)
}

func (g *winGroup) Close() {
	if g.closed.Swap(true) {
		return
	}
	_ = windows.PostQueuedCompletionStatus(g.port, 0, ^uintptr(0), nil)
	windows.CloseHandle(g.job) // kill on close: anything left goes with it
	windows.CloseHandle(g.port)
}

type winProc struct {
	pid      int
	job      windows.Handle
	ownGroup bool
	mu       sync.Mutex
}

func (p *winProc) PID() int { return p.pid }

func (p *winProc) Usage() Usage {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.job == 0 {
		return processUsage(p.pid)
	}
	return jobUsage(p.job)
}

func (p *winProc) Signal(kind string) error {
	switch kind {
	case "break":
		if p.ownGroup {
			return windows.GenerateConsoleCtrlEvent(windows.CTRL_BREAK_EVENT, uint32(p.pid))
		}
		return errors.New("not in a process group of its own")
	case "interrupt":
		return errors.New("Ctrl+C reaches a single service only through the console")
	}
	return p.KillTree()
}

func (p *winProc) KillTree() error {
	p.mu.Lock()
	job := p.job
	p.mu.Unlock()
	if job != 0 {
		return windows.TerminateJobObject(job, 1)
	}
	h, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, uint32(p.pid))
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	return windows.TerminateProcess(h, 1)
}

func (p *winProc) Release() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.job != 0 {
		windows.CloseHandle(p.job)
		p.job = 0
	}
}

func jobUsage(job windows.Handle) Usage {
	var u Usage
	var acct basicAccounting
	if windows.QueryInformationJobObject(job, classBasicAccounting, uintptr(unsafe.Pointer(&acct)), uint32(unsafe.Sizeof(acct)), nil) == nil {
		u.Processes = int(acct.ActiveProcesses)
		u.CPU = time.Duration(acct.TotalUserTime+acct.TotalKernelTime) * 100
	}
	var ext extendedLimit
	if windows.QueryInformationJobObject(job, classExtendedLimit, uintptr(unsafe.Pointer(&ext)), uint32(unsafe.Sizeof(ext)), nil) == nil {
		u.PeakBytes = uint64(ext.PeakJobMemoryUsed)
	}
	// JOBOBJECT_BASIC_PROCESS_ID_LIST: two DWORDs, then the ids (ULONG_PTR)
	buf := make([]uintptr, 2+256)
	if windows.QueryInformationJobObject(job, classBasicProcessIDs, uintptr(unsafe.Pointer(&buf[0])), uint32(len(buf))*uint32(unsafe.Sizeof(buf[0])), nil) == nil {
		listed := int(*(*uint32)(unsafe.Pointer(uintptr(unsafe.Pointer(&buf[0])) + 4)))
		for _, pid := range buf[1 : 1+min(listed, 256)] {
			u.PIDs = append(u.PIDs, int(pid))
			u.MemoryBytes += privateBytes(int(pid))
		}
	}
	return u
}

func processUsage(pid int) Usage {
	return Usage{MemoryBytes: privateBytes(pid), Processes: 1, PIDs: []int{pid}}
}

func privateBytes(pid int) uint64 {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return 0
	}
	defer windows.CloseHandle(h)
	var m memoryCountersEx
	m.Cb = uint32(unsafe.Sizeof(m))
	if r, _, _ := procGetProcessMemoryInfo.Call(uintptr(h), uintptr(unsafe.Pointer(&m)), uintptr(m.Cb)); r == 0 {
		return 0
	}
	return uint64(m.PrivateUsage)
}
