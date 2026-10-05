package source

import (
	"os/exec"
	"syscall"
)

const createNoWindow = 0x08000000

// prepare hides the console window of a short git call.
func prepare(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
}
