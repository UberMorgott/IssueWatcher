package selfupdate

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
)

const createNoWindow = 0x08000000

// Start runs exe with args as a process that outlives this one: its own
// process group and, when allowed, outside a job object this process runs in.
// The environment is inherited (IW_DATA_DIR, IW_HEADLESS, …).
func Start(exe string, args ...string) (*os.Process, error) {
	start := func(breakaway bool) (*os.Process, error) {
		flags := uint32(createNoWindow | windows.CREATE_NEW_PROCESS_GROUP)
		if breakaway {
			flags |= windows.CREATE_BREAKAWAY_FROM_JOB
		}
		// The new app outlives this one, so no context may end it.
		cmd := exec.CommandContext(context.Background(), exe, args...) //nolint:gosec // G204: this program's own path and flags
		cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: flags}
		if err := cmd.Start(); err != nil {
			return nil, err
		}
		return cmd.Process, nil
	}
	p, err := start(true)
	if errors.Is(err, windows.ERROR_ACCESS_DENIED) { // the job forbids breakaway
		p, err = start(false)
	}
	return p, err
}

// WaitPID blocks until process pid has exited (nil when it is already gone)
// or timeout passes.
func WaitPID(pid int, timeout time.Duration) error {
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid)) //nolint:gosec // G115: a PID from our own flag
	if err != nil {
		if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
			return nil // no such process: already exited
		}
		return fmt.Errorf("open process %d: %w", pid, err)
	}
	defer func() { _ = windows.CloseHandle(h) }()
	ev, err := windows.WaitForSingleObject(h, uint32(timeout/time.Millisecond)) //nolint:gosec // G115: bounded timeout
	switch {
	case err != nil:
		return fmt.Errorf("wait for process %d: %w", pid, err)
	case ev == uint32(windows.WAIT_TIMEOUT):
		return fmt.Errorf("process %d still running after %s", pid, timeout)
	}
	return nil
}
