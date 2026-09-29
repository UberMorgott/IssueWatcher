package mcpbridge

import (
	"fmt"
	"os/exec"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	createNoWindow       = 0x08000000
	processSuspendResume = 0x0800 // PROCESS_SUSPEND_RESUME (not in x/sys/windows)
)

var procNtResumeProcess = windows.NewLazySystemDLL("ntdll.dll").NewProc("NtResumeProcess")

// procTree is the server and everything it spawns (node, headless Chrome) in a
// kill-on-close Job Object: killed on stop and when the app exits.
type procTree struct {
	mu  sync.Mutex
	job windows.Handle
}

// prepareChild hides the console and starts the child suspended, so nothing it
// spawns escapes before adopt puts it in the job.
func prepareChild(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true,
		CreationFlags: createNoWindow | windows.CREATE_NEW_PROCESS_GROUP | windows.CREATE_SUSPENDED}
}

// adopt assigns the started (suspended) child to a new job and resumes it.
func adopt(cmd *exec.Cmd) (*procTree, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("mcpbridge: job object: %w", err)
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil { //nolint:gosec // G103: the documented way to pass the limits struct
		_ = windows.CloseHandle(job)
		return nil, fmt.Errorf("mcpbridge: job object limits: %w", err)
	}
	pid := uint32(cmd.Process.Pid) //nolint:gosec // G115: a PID we just started
	h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE|processSuspendResume, false, pid)
	if err != nil {
		_ = windows.CloseHandle(job)
		return nil, fmt.Errorf("mcpbridge: open process: %w", err)
	}
	defer func() { _ = windows.CloseHandle(h) }()
	if err := windows.AssignProcessToJobObject(job, h); err != nil {
		_ = windows.CloseHandle(job)
		return nil, fmt.Errorf("mcpbridge: assign job object: %w", err)
	}
	if st, _, _ := procNtResumeProcess.Call(uintptr(h)); st != 0 {
		_ = windows.TerminateJobObject(job, 1)
		_ = windows.CloseHandle(job)
		return nil, fmt.Errorf("mcpbridge: resume the server: NTSTATUS 0x%08x", st)
	}
	return &procTree{job: job}, nil
}

// kill terminates the tree and releases the job.
func (p *procTree) kill() {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.job != 0 {
		_ = windows.TerminateJobObject(p.job, 1)
		_ = windows.CloseHandle(p.job)
		p.job = 0
	}
}
