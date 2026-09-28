package notify

import (
	"runtime"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	procGetWindowTextW    = user32.NewProc("GetWindowTextW")
	procIsIconic          = user32.NewProc("IsIconic")
	procShowWindow        = user32.NewProc("ShowWindow")
	procBringWindowToTop  = user32.NewProc("BringWindowToTop")
	procAttachThreadInput = user32.NewProc("AttachThreadInput")
)

const swRestore = 9

// The EnumWindows callback is created once: Go never frees callbacks and caps
// how many a process may create. focusMu guards the search result.
var (
	focusMu    sync.Mutex
	focusHwnd  windows.HWND
	focusTitle string
	focusEnum  = syscall.NewCallback(func(hwnd windows.HWND, _ uintptr) uintptr {
		if !windows.IsWindowVisible(hwnd) {
			return 1
		}
		if t := windowText(hwnd); IsDashboardTitle(t) {
			focusHwnd, focusTitle = hwnd, t
			return 0 // stop
		}
		return 1
	})
)

// FocusDashboard brings the browser window whose active tab is the dashboard to
// the front (restoring it when minimised) and returns its title. It only works
// while this process may set the foreground window, i.e. right after the user
// clicked the tray icon, its menu or a notification. ok is false when no such
// window exists (dashboard not the active tab, or no tab open).
func FocusDashboard() (title string, ok bool) {
	runtime.LockOSThread() // AttachThreadInput below is per OS thread
	defer runtime.UnlockOSThread()
	focusMu.Lock()
	focusHwnd, focusTitle = 0, ""
	_ = windows.EnumWindows(focusEnum, nil) // reports an error when the callback stops early
	found, title := focusHwnd, focusTitle
	focusMu.Unlock()
	if found == 0 {
		return "", false
	}
	if r, _, _ := procIsIconic.Call(uintptr(found)); r != 0 {
		_, _, _ = procShowWindow.Call(uintptr(found), swRestore)
	}
	if r, _, _ := procSetForegroundWindow.Call(uintptr(found)); r != 0 {
		return title, true
	}
	// Foreground lock: borrow the input queue of the current foreground thread.
	fgThread, _ := windows.GetWindowThreadProcessId(windows.GetForegroundWindow(), nil)
	self := windows.GetCurrentThreadId()
	if fgThread != 0 && fgThread != self {
		_, _, _ = procAttachThreadInput.Call(uintptr(self), uintptr(fgThread), 1)
		defer func() { _, _, _ = procAttachThreadInput.Call(uintptr(self), uintptr(fgThread), 0) }()
	}
	_, _, _ = procBringWindowToTop.Call(uintptr(found))
	r, _, _ := procSetForegroundWindow.Call(uintptr(found))
	return title, r != 0
}

func windowText(hwnd windows.HWND) string {
	buf := make([]uint16, 512)
	n, _, _ := procGetWindowTextW.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	return windows.UTF16ToString(buf[:n])
}
