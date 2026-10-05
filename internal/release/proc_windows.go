package release

import (
	"os/exec"
	"syscall"
)

const createNoWindow = 0x08000000

// hide keeps git's console window from flashing.
func hide(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
}
