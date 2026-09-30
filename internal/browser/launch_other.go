//go:build !windows

package browser

import (
	"errors"
	"fmt"
	"os/exec"
)

func startPipe(string, []string) (*process, error) {
	return nil, errors.New("browser: DevTools pipe is wired for Windows only")
}

func startPlain(exe string, args []string) (*process, error) {
	cmd := exec.Command(exe, args...) //nolint:gosec,noctx // G204: the installed browser
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("browser: start %s: %w", exe, err)
	}
	p := &process{kill: func() { _ = cmd.Process.Kill() }, exited: make(chan struct{})}
	go func() {
		_ = cmd.Wait()
		close(p.exited)
	}()
	return p, nil
}
