//go:build !windows

package release

import "os/exec"

func hide(*exec.Cmd) {}
