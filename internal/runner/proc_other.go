//go:build !windows

package runner

import (
	"os/exec"
	"syscall"
)

// procTree kills the command's process group (non-Windows builds, tests only).
type procTree struct{ pid int }

func prepare(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }

func attach(cmd *exec.Cmd) (*procTree, error) { return &procTree{pid: cmd.Process.Pid}, nil }

func (p *procTree) kill() {
	if p != nil && p.pid > 0 {
		_ = syscall.Kill(-p.pid, syscall.SIGKILL)
	}
}

func (p *procTree) close() {}
