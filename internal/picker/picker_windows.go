// Package picker shows the native Windows folder dialog (IFileOpenDialog with
// FOS_PICKFOLDERS) for the dashboard's «Обзор…» button (POST /api/dialog/folder).
package picker

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	ole32   = windows.NewLazySystemDLL("ole32.dll")
	shell32 = windows.NewLazySystemDLL("shell32.dll")
	user32  = windows.NewLazySystemDLL("user32.dll")

	procCoCreateInstance            = ole32.NewProc("CoCreateInstance")
	procSHCreateItemFromParsingName = shell32.NewProc("SHCreateItemFromParsingName")
	procEnumThreadWindows           = user32.NewProc("EnumThreadWindows")
	procGetClassNameW               = user32.NewProc("GetClassNameW")
	procSetForegroundWindow         = user32.NewProc("SetForegroundWindow")
	procBringWindowToTop            = user32.NewProc("BringWindowToTop")
	procAttachThreadInput           = user32.NewProc("AttachThreadInput")
	procPostMessageW                = user32.NewProc("PostMessageW")

	clsidFileOpenDialog = mustGUID("{DC1C5A9C-E88A-4DDE-A5A1-60F82A20AEF7}")
	iidFileOpenDialog   = mustGUID("{D57C7288-D4AD-4768-BE02-9D969532D960}")
	iidShellItem        = mustGUID("{43826D1E-E718-42EE-BC55-A1E261C37BFE}")
)

const (
	coinitApartmentThreaded = 0x2
	coinitDisableOLE1DDE    = 0x4
	clsctxInprocServer      = 0x1

	fosNoChangeDir     = 0x8
	fosPickFolders     = 0x20
	fosForceFileSystem = 0x40
	fosPathMustExist   = 0x800
	sigdnFileSysPath   = 0x80058000
	hrCancelled        = 0x800704C7 // HRESULT_FROM_WIN32(ERROR_CANCELLED)
	sFalse             = syscall.Errno(1)
	wmClose            = 0x0010
	dialogClass        = "#32770"
	findDialogFor      = 5 * time.Second
	findDialogInterval = 50 * time.Millisecond
)

// Vtable slots (IUnknown 0-2, IModalWindow 3, IFileDialog 4-26; IShellItem 3-7).
const (
	vRelease        = 2
	vShow           = 3
	vSetOptions     = 9
	vGetOptions     = 10
	vSetFolder      = 12
	vSetTitle       = 17
	vGetResult      = 20
	vGetDisplayName = 5
)

func mustGUID(s string) windows.GUID {
	g, err := windows.GUIDFromString(s)
	if err != nil {
		panic(err)
	}
	return g
}

// comObj is a COM interface pointer: its first word points at the vtable.
type comObj struct{ vtbl *[32]uintptr }

func (o *comObj) call(slot int, args ...uintptr) uint32 {
	r, _, _ := syscall.SyscallN(o.vtbl[slot], append([]uintptr{uintptr(unsafe.Pointer(o))}, args...)...)
	return uint32(r) // HRESULT is 32-bit
}

func (o *comObj) release() { o.call(vRelease) }

// Picker shows one folder dialog at a time. The zero value is not used; see New.
type Picker struct {
	mu   sync.Mutex
	hwnd atomic.Uintptr // the open dialog window, once found
}

// New returns the desktop folder picker.
func New() *Picker { return &Picker{} }

// PickFolder shows the dialog over the foreground window (the dashboard's
// browser window, which sent the request), brought to the front, starting in
// initial ("" = the shell's default). ok is false when the user cancelled.
// A cancelled ctx closes the dialog.
func (p *Picker) PickFolder(ctx context.Context, title, initial string) (path string, ok bool, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	type result struct {
		path string
		ok   bool
		err  error
	}
	res := make(chan result, 1)
	go func() {
		// COM apartment and the modal loop belong to one OS thread.
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		path, ok, err := p.show(title, initial)
		res <- result{path, ok, err}
	}()
	select {
	case r := <-res:
		return r.path, r.ok, r.err
	case <-ctx.Done():
	}
	// The dialog may not be found yet: keep asking it to close until Show returns.
	tick := time.NewTicker(findDialogInterval)
	defer tick.Stop()
	for {
		if h := p.hwnd.Load(); h != 0 {
			_, _, _ = procPostMessageW.Call(h, wmClose, 0, 0)
		}
		select {
		case r := <-res:
			return r.path, r.ok, r.err
		case <-tick.C:
		}
	}
}

func (p *Picker) show(title, initial string) (string, bool, error) {
	if err := windows.CoInitializeEx(0, coinitApartmentThreaded|coinitDisableOLE1DDE); err != nil && !errors.Is(err, sFalse) {
		return "", false, fmt.Errorf("CoInitializeEx: %w", err)
	}
	defer windows.CoUninitialize()

	var dlg *comObj
	if hr, _, _ := procCoCreateInstance.Call(uintptr(unsafe.Pointer(&clsidFileOpenDialog)), 0, clsctxInprocServer,
		uintptr(unsafe.Pointer(&iidFileOpenDialog)), uintptr(unsafe.Pointer(&dlg))); uint32(hr) != 0 || dlg == nil {
		return "", false, fmt.Errorf("CoCreateInstance(FileOpenDialog): 0x%08X", uint32(hr))
	}
	defer dlg.release()

	var opts uint32
	if hr := dlg.call(vGetOptions, uintptr(unsafe.Pointer(&opts))); hr != 0 {
		return "", false, fmt.Errorf("GetOptions: 0x%08X", hr)
	}
	if hr := dlg.call(vSetOptions, uintptr(opts|fosPickFolders|fosForceFileSystem|fosPathMustExist|fosNoChangeDir)); hr != 0 {
		return "", false, fmt.Errorf("SetOptions: 0x%08X", hr)
	}
	if title != "" {
		if t, err := windows.UTF16PtrFromString(title); err == nil {
			dlg.call(vSetTitle, uintptr(unsafe.Pointer(t)))
		}
	}
	if initial != "" {
		setFolder(dlg, initial)
	}

	owner := windows.GetForegroundWindow()
	stop := make(chan struct{})
	done := make(chan struct{})
	go p.raise(windows.GetCurrentThreadId(), stop, done)
	hr := dlg.call(vShow, uintptr(owner))
	close(stop)
	<-done
	p.hwnd.Store(0)
	if hr == hrCancelled {
		return "", false, nil
	}
	if hr != 0 {
		return "", false, fmt.Errorf("IFileDialog.Show: 0x%08X", hr)
	}

	var item *comObj
	if hr := dlg.call(vGetResult, uintptr(unsafe.Pointer(&item))); hr != 0 || item == nil {
		return "", false, fmt.Errorf("GetResult: 0x%08X", hr)
	}
	defer item.release()
	var s *uint16
	if hr := item.call(vGetDisplayName, sigdnFileSysPath, uintptr(unsafe.Pointer(&s))); hr != 0 || s == nil {
		return "", false, fmt.Errorf("GetDisplayName: 0x%08X", hr)
	}
	defer windows.CoTaskMemFree(unsafe.Pointer(s))
	return windows.UTF16PtrToString(s), true, nil
}

// setFolder opens the dialog in dir (SetFolder beats the shell's last-used folder).
func setFolder(dlg *comObj, dir string) {
	d, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return
	}
	var item *comObj
	if hr, _, _ := procSHCreateItemFromParsingName.Call(uintptr(unsafe.Pointer(d)), 0,
		uintptr(unsafe.Pointer(&iidShellItem)), uintptr(unsafe.Pointer(&item))); uint32(hr) != 0 || item == nil {
		return
	}
	dlg.call(vSetFolder, uintptr(unsafe.Pointer(item)))
	item.release()
}

// The EnumThreadWindows callback is created once (Go never frees callbacks);
// findMu guards its result.
var (
	findMu    sync.Mutex
	foundHwnd uintptr
	findEnum  = syscall.NewCallback(func(hwnd windows.HWND, _ uintptr) uintptr {
		if windows.IsWindowVisible(hwnd) && className(hwnd) == dialogClass {
			foundHwnd = uintptr(hwnd)
			return 0
		}
		return 1
	})
)

func className(hwnd windows.HWND) string {
	var buf [64]uint16
	n, _, _ := procGetClassNameW.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	return windows.UTF16ToString(buf[:n])
}

// raise waits for the dialog window of thread tid and brings it to the front:
// the request came from the browser, so this process holds no foreground right
// and borrows the foreground thread's input state for SetForegroundWindow.
func (p *Picker) raise(tid uint32, stop <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	runtime.LockOSThread() // AttachThreadInput is per OS thread
	defer runtime.UnlockOSThread()
	deadline := time.Now().Add(findDialogFor)
	for time.Now().Before(deadline) {
		select {
		case <-stop:
			return
		case <-time.After(findDialogInterval):
		}
		findMu.Lock()
		foundHwnd = 0
		_, _, _ = procEnumThreadWindows.Call(uintptr(tid), findEnum, 0)
		h := foundHwnd
		findMu.Unlock()
		if h == 0 {
			continue
		}
		p.hwnd.Store(h)
		fg := windows.GetForegroundWindow()
		if uintptr(fg) == h {
			return
		}
		fgThread, _ := windows.GetWindowThreadProcessId(fg, nil)
		self := windows.GetCurrentThreadId()
		if fgThread != 0 && fgThread != self {
			_, _, _ = procAttachThreadInput.Call(uintptr(self), uintptr(fgThread), 1)
			defer func() { _, _, _ = procAttachThreadInput.Call(uintptr(self), uintptr(fgThread), 0) }()
		}
		_, _, _ = procSetForegroundWindow.Call(h)
		_, _, _ = procBringWindowToTop.Call(h)
		return
	}
}
