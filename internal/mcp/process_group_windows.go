//go:build windows

package mcp

import (
	"fmt"
	"syscall"
	"unsafe"
)

const (
	jobObjectBasicAccountingInformation = 1
	jobObjectExtendedLimitInformation   = 9
	jobObjectLimitActiveProcess         = 0x00000008
	jobObjectLimitKillOnJobClose        = 0x00002000
	processSetQuota                     = 0x0100
	processTerminate                    = 0x0001
)

var (
	jobKernel32                   = syscall.NewLazyDLL("kernel32.dll")
	procCreateJobObjectW          = jobKernel32.NewProc("CreateJobObjectW")
	procSetInformationJobObject   = jobKernel32.NewProc("SetInformationJobObject")
	procAssignProcessToJobObject  = jobKernel32.NewProc("AssignProcessToJobObject")
	procTerminateJobObject        = jobKernel32.NewProc("TerminateJobObject")
	procQueryInformationJobObject = jobKernel32.NewProc("QueryInformationJobObject")
	procOpenProcess               = jobKernel32.NewProc("OpenProcess")
	procCloseHandle               = jobKernel32.NewProc("CloseHandle")
)

type jobBasicLimitInformation struct {
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

type jobIOCounters struct {
	ReadOperationCount  uint64
	WriteOperationCount uint64
	OtherOperationCount uint64
	ReadTransferCount   uint64
	WriteTransferCount  uint64
	OtherTransferCount  uint64
}

type jobExtendedLimitInformation struct {
	BasicLimitInformation jobBasicLimitInformation
	IOInfo                jobIOCounters
	ProcessMemoryLimit    uintptr
	JobMemoryLimit        uintptr
	PeakProcessMemoryUsed uintptr
	PeakJobMemoryUsed     uintptr
}

type jobBasicAccountingInformation struct {
	TotalUserTime             int64
	TotalKernelTime           int64
	ThisPeriodTotalUserTime   int64
	ThisPeriodTotalKernelTime int64
	TotalPageFaultCount       uint32
	TotalProcesses            uint32
	ActiveProcesses           uint32
	TotalTerminatedProcesses  uint32
}

func winCallError(name string, err error) error {
	if errno, ok := err.(syscall.Errno); ok && errno == 0 {
		return fmt.Errorf("%s failed", name)
	}
	if err == nil {
		return fmt.Errorf("%s failed", name)
	}
	return fmt.Errorf("%s: %w", name, err)
}

func attachProcessGroup(pid int, activeProcessLimit uint32) (uintptr, error) {
	job, _, err := procCreateJobObjectW.Call(0, 0)
	if job == 0 {
		return 0, winCallError("CreateJobObjectW", err)
	}
	closeJob := true
	defer func() {
		if closeJob {
			_, _, _ = procCloseHandle.Call(job)
		}
	}()

	limits := jobExtendedLimitInformation{}
	limits.BasicLimitInformation.LimitFlags = jobObjectLimitKillOnJobClose | jobObjectLimitActiveProcess
	limits.BasicLimitInformation.ActiveProcessLimit = activeProcessLimit
	ok, _, setErr := procSetInformationJobObject.Call(
		job,
		jobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&limits)),
		unsafe.Sizeof(limits),
	)
	if ok == 0 {
		return 0, winCallError("SetInformationJobObject", setErr)
	}

	process, _, openErr := procOpenProcess.Call(processSetQuota|processTerminate, 0, uintptr(uint32(pid)))
	if process == 0 {
		return 0, winCallError("OpenProcess", openErr)
	}
	defer procCloseHandle.Call(process)

	ok, _, assignErr := procAssignProcessToJobObject.Call(job, process)
	if ok == 0 {
		return 0, winCallError("AssignProcessToJobObject", assignErr)
	}
	closeJob = false
	return job, nil
}

func processGroupProcessCount(handle uintptr) (uint32, error) {
	if handle == 0 {
		return 0, nil
	}
	info := jobBasicAccountingInformation{}
	ok, _, err := procQueryInformationJobObject.Call(
		handle,
		jobObjectBasicAccountingInformation,
		uintptr(unsafe.Pointer(&info)),
		unsafe.Sizeof(info),
		0,
	)
	if ok == 0 {
		return 0, winCallError("QueryInformationJobObject", err)
	}
	return info.ActiveProcesses, nil
}

func terminateProcessGroup(handle uintptr) error {
	if handle == 0 {
		return nil
	}
	ok, _, err := procTerminateJobObject.Call(handle, 1)
	if ok == 0 {
		return winCallError("TerminateJobObject", err)
	}
	return nil
}

func closeProcessGroup(handle uintptr) {
	if handle != 0 {
		_, _, _ = procCloseHandle.Call(handle)
	}
}
