//go:build !windows

package runner

import (
	"errors"
	"syscall"
)

// alive reports whether process pid still runs (signal 0 probes it).
func alive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
