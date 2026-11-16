//go:build windows

package osctl

import (
	"fmt"
	"os/exec"
	"syscall"
	"unsafe"
)

var (
	k32                  = syscall.NewLazyDLL("kernel32.dll")
	pCreateJobObject     = k32.NewProc("CreateJobObjectW")
	pSetInformationJob   = k32.NewProc("SetInformationJobObject")
	pQueryInformationJob = k32.NewProc("QueryInformationJobObject")
	pAssignProcessToJob  = k32.NewProc("AssignProcessToJobObject")
	pTerminateJobObject  = k32.NewProc("TerminateJobObject")
	pGlobalMemoryStatus  = k32.NewProc("GlobalMemoryStatusEx")
	pGenerateCtrlEvent   = k32.NewProc("GenerateConsoleCtrlEvent")
	pEnumProcessesInJob  = k32.NewProc("QueryInformationJobObject")
	pK32GetProcessMemory = k32.NewProc("K32GetProcessMemoryInfo")
)

const (
	limitJobMemory      = 0x00000200
	limitBreakawayOK    = 0x00000800
	limitKillOnJobClose = 0x00002000
	classBasicPidList   = 3
	classExtendedLimit  = 9
	createNewProcGroup  = 0x00000200
	processTerminate    = 0x0001
	processSetQuota     = 0x0100
	processQueryInfo    = 0x0400
	processVMRead       = 0x0010
)

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
	IO                    [6]uint64
	ProcessMemoryLimit    uintptr
	JobMemoryLimit        uintptr
	PeakProcessMemoryUsed uintptr
	PeakJobMemoryUsed     uintptr
}

type memCounters struct {
	CB                         uint32
	PageFaultCount             uint32
	PeakWorkingSetSize         uintptr
	WorkingSetSize             uintptr
	QuotaPeakPagedPoolUsage    uintptr
	QuotaPagedPoolUsage        uintptr
	QuotaPeakNonPagedPoolUsage uintptr
	QuotaNonPagedPoolUsage     uintptr
	PagefileUsage              uintptr
	PeakPagefileUsage          uintptr
}

type winUnit struct {
	job syscall.Handle
	pid int
}

// Prepare gives the child its own console process group so Ctrl+C at the
// terminal reaches only the polivisor, which then stops children in order.
func Prepare(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNewProcGroup}
}

// Contain places a started process into a fresh job capped at memMB.
// Processes the child spawns later (forked workers) inherit the job.
func Contain(cmd *exec.Cmd, memMB int) (Unit, error) {
	h, _, err := pCreateJobObject.Call(0, 0)
	if h == 0 {
		return nil, fmt.Errorf("CreateJobObject: %w", err)
	}
	job := syscall.Handle(h)
	var info extendedLimit
	info.Basic.LimitFlags = limitKillOnJobClose | limitBreakawayOK
	if memMB > 0 {
		info.Basic.LimitFlags |= limitJobMemory
		info.JobMemoryLimit = uintptr(memMB) << 20
	}
	if r, _, err := pSetInformationJob.Call(uintptr(job), classExtendedLimit,
		uintptr(unsafe.Pointer(&info)), unsafe.Sizeof(info)); r == 0 {
		syscall.CloseHandle(job)
		return nil, fmt.Errorf("SetInformationJobObject: %w", err)
	}
	ph, err := syscall.OpenProcess(processTerminate|processSetQuota|processQueryInfo,
		false, uint32(cmd.Process.Pid))
	if err != nil {
		syscall.CloseHandle(job)
		return nil, fmt.Errorf("OpenProcess: %w", err)
	}
	defer syscall.CloseHandle(ph)
	if r, _, err := pAssignProcessToJob.Call(uintptr(job), uintptr(ph)); r == 0 {
		syscall.CloseHandle(job)
		return nil, fmt.Errorf("AssignProcessToJobObject: %w", err)
	}
	return &winUnit{job: job, pid: cmd.Process.Pid}, nil
}

func (u *winUnit) Kill() error {
	if r, _, err := pTerminateJobObject.Call(uintptr(u.job), 1); r == 0 {
		return err
	}
	return nil
}

// Interrupt sends CTRL_BREAK to the child's process group (CTRL_C cannot
// target a group). Bun, Go, Node and the JVM all treat it as shutdown.
func (u *winUnit) Interrupt() error {
	if r, _, err := pGenerateCtrlEvent.Call(1, uintptr(u.pid)); r == 0 {
		return err
	}
	return nil
}

// MemBytes sums the commit charge of every process in the job, which is the
// figure JOB_OBJECT_LIMIT_JOB_MEMORY enforces.
func (u *winUnit) MemBytes() uint64 {
	var buf struct {
		Assigned, InList uint32
		Pids             [256]uintptr
	}
	if r, _, _ := pEnumProcessesInJob.Call(uintptr(u.job), classBasicPidList,
		uintptr(unsafe.Pointer(&buf)), unsafe.Sizeof(buf), 0); r == 0 {
		return 0
	}
	var total uint64
	for _, pid := range buf.Pids[:buf.InList] {
		ph, err := syscall.OpenProcess(processQueryInfo|processVMRead, false, uint32(pid))
		if err != nil {
			continue
		}
		mc := memCounters{CB: uint32(unsafe.Sizeof(memCounters{}))}
		if r, _, _ := pK32GetProcessMemory.Call(uintptr(ph),
			uintptr(unsafe.Pointer(&mc)), uintptr(mc.CB)); r != 0 {
			total += uint64(mc.PagefileUsage)
		}
		syscall.CloseHandle(ph)
	}
	return total
}

func (u *winUnit) Close() { syscall.CloseHandle(u.job) }

type memStatus struct {
	Length               uint32
	MemoryLoad           uint32
	TotalPhys, AvailPhys uint64
	TotalPage, AvailPage uint64
	TotalVirt, AvailVirt uint64
	AvailExtendedVirtual uint64
}

// AvailableMB returns free physical memory, or -1 if unknown.
func AvailableMB() int {
	m := memStatus{Length: uint32(unsafe.Sizeof(memStatus{}))}
	if r, _, _ := pGlobalMemoryStatus.Call(uintptr(unsafe.Pointer(&m))); r == 0 {
		return -1
	}
	return int(m.AvailPhys >> 20)
}
