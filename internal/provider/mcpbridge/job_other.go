//go:build !windows

package mcpbridge

import (
	"os/exec"
	"syscall"
)

// procTree kills the server's process group (non-Windows builds, tests only).
type procTree struct{ pid int }

func prepareChild(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }

func adopt(cmd *exec.Cmd) (*procTree, error) { return &procTree{pid: cmd.Process.Pid}, nil }

func (p *procTree) kill() {
	if p != nil && p.pid > 0 {
		_ = syscall.Kill(-p.pid, syscall.SIGKILL)
	}
}
