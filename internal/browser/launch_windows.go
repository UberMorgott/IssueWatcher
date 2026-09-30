package browser

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

const processSuspendResume = 0x0800 // PROCESS_SUSPEND_RESUME (not in x/sys/windows)

var procNtResumeProcess = windows.NewLazySystemDLL("ntdll.dll").NewProc("NtResumeProcess")

// startPipe starts the browser with --remote-debugging-pipe over two
// inherited anonymous pipes (--remote-debugging-io-pipes=<read>,<write>: no
// listening port), suspended, inside a kill-on-close Job Object.
func startPipe(exe string, args []string) (*process, error) {
	toBrowserR, toBrowserW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	fromBrowserR, fromBrowserW, err := os.Pipe()
	if err != nil {
		_ = toBrowserR.Close()
		_ = toBrowserW.Close()
		return nil, err
	}
	closeAll := func() {
		for _, f := range []*os.File{toBrowserR, toBrowserW, fromBrowserR, fromBrowserW} {
			_ = f.Close()
		}
	}
	inherit := []syscall.Handle{syscall.Handle(toBrowserR.Fd()), syscall.Handle(fromBrowserW.Fd())}
	for _, h := range inherit {
		if err := windows.SetHandleInformation(windows.Handle(h), windows.HANDLE_FLAG_INHERIT, windows.HANDLE_FLAG_INHERIT); err != nil {
			closeAll()
			return nil, fmt.Errorf("browser: inheritable pipe: %w", err)
		}
	}
	args = append(args, "--remote-debugging-pipe",
		"--remote-debugging-io-pipes="+strconv.FormatUint(uint64(inherit[0]), 10)+","+strconv.FormatUint(uint64(inherit[1]), 10))
	cmd := exec.Command(exe, args...) //nolint:gosec,noctx // G204: the installed browser; lifetime = the browser session
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags:              windows.CREATE_NEW_PROCESS_GROUP | windows.CREATE_SUSPENDED,
		AdditionalInheritedHandles: inherit,
	}
	if err := cmd.Start(); err != nil {
		closeAll()
		return nil, fmt.Errorf("browser: start %s: %w", exe, err)
	}
	_ = toBrowserR.Close() // the browser's ends now live in the child
	_ = fromBrowserW.Close()
	tree, err := adopt(cmd)
	if err != nil {
		_ = cmd.Process.Kill()
		_ = toBrowserW.Close()
		_ = fromBrowserR.Close()
		return nil, err
	}
	p := &process{kill: tree.kill, exited: make(chan struct{})}
	p.t = NewPipeTransport(fromBrowserR, toBrowserW, MaxFrame, toBrowserW, fromBrowserR)
	go func() {
		_ = cmd.Wait()
		close(p.exited)
	}()
	return p, nil
}

// procTree is the browser and every process it spawns, killed together.
type procTree struct {
	mu  sync.Mutex
	job windows.Handle
}

func adopt(cmd *exec.Cmd) (*procTree, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("browser: job object: %w", err)
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil { //nolint:gosec // G103: the documented way to pass the limits struct
		_ = windows.CloseHandle(job)
		return nil, fmt.Errorf("browser: job object limits: %w", err)
	}
	pid := uint32(cmd.Process.Pid) //nolint:gosec // G115: a PID we just started
	h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE|processSuspendResume, false, pid)
	if err != nil {
		_ = windows.CloseHandle(job)
		return nil, fmt.Errorf("browser: open process: %w", err)
	}
	defer func() { _ = windows.CloseHandle(h) }()
	if err := windows.AssignProcessToJobObject(job, h); err != nil {
		_ = windows.CloseHandle(job)
		return nil, fmt.Errorf("browser: assign job object: %w", err)
	}
	if st, _, _ := procNtResumeProcess.Call(uintptr(h)); st != 0 {
		_ = windows.TerminateJobObject(job, 1)
		_ = windows.CloseHandle(job)
		return nil, fmt.Errorf("browser: resume: NTSTATUS 0x%08x", st)
	}
	return &procTree{job: job}, nil
}

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

// startPlain starts the browser (port mode) suspended inside a job.
func startPlain(exe string, args []string) (*process, error) {
	cmd := exec.Command(exe, args...) //nolint:gosec,noctx // G204: the installed browser
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP | windows.CREATE_SUSPENDED}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("browser: start %s: %w", exe, err)
	}
	tree, err := adopt(cmd)
	if err != nil {
		_ = cmd.Process.Kill()
		return nil, err
	}
	p := &process{kill: tree.kill, exited: make(chan struct{})}
	go func() {
		_ = cmd.Wait()
		close(p.exited)
	}()
	return p, nil
}
