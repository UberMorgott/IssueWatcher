package main

import (
	"os"

	"golang.org/x/sys/windows"
)

var procAttachConsole = windows.NewLazySystemDLL("kernel32.dll").NewProc("AttachConsole")

const attachParentProcess = ^uintptr(0) // ATTACH_PARENT_PROCESS ((DWORD)-1)

// attachConsole gives a -H windowsgui build usable stdio for CLI subcommands:
// std handles that are already a pipe, file or console (redirected by the
// caller, e.g. an MCP client) are kept; missing ones are taken from the parent
// process's console, when it has one. Best effort.
func attachConsole() {
	std := []struct {
		id   uint32
		file **os.File
		name string
	}{
		{windows.STD_INPUT_HANDLE, &os.Stdin, "CONIN$"},
		{windows.STD_OUTPUT_HANDLE, &os.Stdout, "CONOUT$"},
		{windows.STD_ERROR_HANDLE, &os.Stderr, "CONOUT$"},
	}
	var missing bool
	for _, s := range std {
		if !usable(s.id) {
			missing = true
		}
	}
	if !missing {
		return
	}
	if r, _, _ := procAttachConsole.Call(attachParentProcess); r == 0 {
		return // no parent console (Explorer, a scheduler): nothing to attach
	}
	for _, s := range std {
		if usable(s.id) {
			continue
		}
		access := uint32(windows.GENERIC_READ | windows.GENERIC_WRITE)
		name, _ := windows.UTF16PtrFromString(s.name)
		h, err := windows.CreateFile(name, access, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, 0, 0)
		if err != nil {
			continue
		}
		_ = windows.SetStdHandle(s.id, h)
		*s.file = os.NewFile(uintptr(h), s.name)
	}
}

// usable reports a std handle that is set and of a known type.
func usable(id uint32) bool {
	h, err := windows.GetStdHandle(id)
	if err != nil || h == 0 || h == windows.InvalidHandle {
		return false
	}
	t, err := windows.GetFileType(h)
	return err == nil && t != windows.FILE_TYPE_UNKNOWN
}
