package runner

import (
	"fmt"
	"os/exec"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

const createNoWindow = 0x08000000

// procTree puts a started command and everything it spawns into a Windows Job
// Object, so kill ends the whole tree (the CLI, its shells, test runners).
type procTree struct {
	job windows.Handle
}

func prepare(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow | windows.CREATE_NEW_PROCESS_GROUP}
}

// attach creates a kill-on-close job object and assigns the started process.
func attach(cmd *exec.Cmd) (*procTree, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("runner: job object: %w", err)
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil { //nolint:gosec // G103: the documented way to pass the limits struct
		_ = windows.CloseHandle(job)
		return nil, fmt.Errorf("runner: job object limits: %w", err)
	}
	h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid)) //nolint:gosec // G115: a PID we just started
	if err != nil {
		_ = windows.CloseHandle(job)
		return nil, fmt.Errorf("runner: open process: %w", err)
	}
	defer func() { _ = windows.CloseHandle(h) }()
	if err := windows.AssignProcessToJobObject(job, h); err != nil {
		_ = windows.CloseHandle(job)
		return nil, fmt.Errorf("runner: assign job object: %w", err)
	}
	return &procTree{job: job}, nil
}

// kill terminates every process in the tree.
func (p *procTree) kill() {
	if p != nil && p.job != 0 {
		_ = windows.TerminateJobObject(p.job, 1)
	}
}

// close releases the job object (kill-on-close ends stragglers).
func (p *procTree) close() {
	if p != nil && p.job != 0 {
		_ = windows.CloseHandle(p.job)
		p.job = 0
	}
}
