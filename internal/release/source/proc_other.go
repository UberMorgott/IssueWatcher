//go:build !windows

package source

import "os/exec"

func prepare(*exec.Cmd) {}
