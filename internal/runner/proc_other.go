//go:build !windows

package runner

import (
	"os/exec"
	"syscall"
	"time"
)

// procTree kills the command's process group (non-Windows builds, tests only).
type procTree struct{ pid int }

func prepare(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }

func prepareTree(cmd *exec.Cmd) { prepare(cmd) }

func attach(cmd *exec.Cmd) (*procTree, error) { return &procTree{pid: cmd.Process.Pid}, nil }

func (p *procTree) kill() {
	if p != nil && p.pid > 0 {
		_ = syscall.Kill(-p.pid, syscall.SIGKILL)
	}
}

func (p *procTree) pids() []uint32 {
	if p == nil || p.pid <= 0 {
		return nil
	}
	return []uint32{uint32(p.pid)} //nolint:gosec // G115: a pid
}

func (p *procTree) end(time.Duration) error {
	p.kill()
	return nil
}

func (p *procTree) close() {}

func processCreated(uint32) (int64, bool) { return 0, false }

func killRecorded([]procRecord) int { return 0 }

// InAgentJob reports whether process pid runs inside a live runner job (not
// detectable without job objects: always false).
func InAgentJob(uint32) bool { return false }
