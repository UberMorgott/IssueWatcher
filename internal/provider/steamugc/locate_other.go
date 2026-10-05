//go:build !windows

package steamugc

import "os/exec"

func hideWindow(*exec.Cmd) {}

func findDLL() (string, error) { return "", ErrNoSteamAPI }
