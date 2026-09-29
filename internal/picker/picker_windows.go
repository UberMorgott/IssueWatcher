// Package picker shows the native Windows folder dialog (IFileOpenDialog with
// FOS_PICKFOLDERS) for the dashboard's «Обзор…» button (POST /api/dialog/folder).
package picker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
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
	procIsIconic                    = user32.NewProc("IsIconic")

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
	findDialogInterval = 10 * time.Millisecond
	closeInterval      = 20 * time.Millisecond
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

// Picker shows one folder dialog at a time on one long-lived STA thread: COM
// is initialised once and the shell's dialog DLLs stay loaded, so only the
// first use pays for them (Warm moves that cost to startup).
type Picker struct {
	mu     sync.Mutex // one dialog at a time
	log    *slog.Logger
	hwnd   atomic.Uintptr // the open dialog window, once found
	start  sync.Once
	jobs   chan func()
	thread uint32        // OS thread id of the STA thread, set before the first job
	coinit time.Duration // CoInitializeEx time, reported once by the first job
	last   phases        // timings of the last dialog (written on the STA thread)
}

// phases are the durations of one dialog, from PickFolder's call (visible, front).
type phases struct {
	coinit, create, folder, visible, front, show time.Duration
	skipped                                      string // initial folder not used (network path)
}

type result struct {
	path string
	ok   bool
	err  error
}

// New returns the desktop folder picker; log gets one debug line per dialog.
func New(log *slog.Logger) *Picker {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Picker{log: log, jobs: make(chan func())}
}

// Warm starts the STA thread and loads the dialog's COM class once, so the
// first «Обзор…» click does not pay for it. Safe from any goroutine.
func (p *Picker) Warm() {
	t0 := time.Now()
	var coinit time.Duration
	var err error
	p.run(func() {
		coinit = p.takeCoinit()
		var dlg *comObj
		if dlg, err = createDialog(); err == nil {
			dlg.release()
		}
	})
	p.log.Debug("folder picker warm", "ms", ms(time.Since(t0)), "coinit_ms", ms(coinit), "err", err)
}

// run executes f on the STA thread and waits for it.
func (p *Picker) run(f func()) {
	p.start.Do(func() {
		ready := make(chan struct{})
		go p.loop(ready)
		<-ready
	})
	done := make(chan struct{})
	p.jobs <- func() { defer close(done); f() }
	<-done
}

// loop owns the STA thread for the life of the process: the goroutine keeps
// its OS thread locked and never returns, so COM stays initialised on it.
func (p *Picker) loop(ready chan<- struct{}) {
	runtime.LockOSThread()
	t0 := time.Now()
	if err := windows.CoInitializeEx(0, coinitApartmentThreaded|coinitDisableOLE1DDE); err != nil && !errors.Is(err, sFalse) {
		p.log.Warn("folder picker: CoInitializeEx", "err", err)
	}
	p.coinit = time.Since(t0)
	p.thread = windows.GetCurrentThreadId()
	close(ready)
	for f := range p.jobs {
		f()
	}
}

// takeCoinit returns the COM init time once (to the job that follows it). STA thread only.
func (p *Picker) takeCoinit() time.Duration {
	d := p.coinit
	p.coinit = 0
	return d
}

// PickFolder shows the dialog over the foreground window (the dashboard's
// browser window, which sent the request), brought to the front, starting in
// initial ("" or a network path = the shell's default). ok is false when the
// user cancelled. A cancelled ctx closes the dialog.
func (p *Picker) PickFolder(ctx context.Context, title, initial string) (path string, ok bool, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	res := make(chan result, 1)
	t0 := time.Now()
	go p.run(func() {
		path, ok, err := p.show(t0, title, initial)
		res <- result{path, ok, err}
	})
	var r result
	select {
	case r = <-res:
	case <-ctx.Done():
		r = p.closeDialog(res)
	}
	l := p.last // res was sent after show wrote it
	p.log.Debug("folder picker", "coinit_ms", ms(l.coinit), "create_ms", ms(l.create), "folder_ms", ms(l.folder),
		"visible_ms", ms(l.visible), "front_ms", ms(l.front), "show_ms", ms(l.show), "total_ms", ms(time.Since(t0)),
		"skipped_initial", l.skipped, "picked", r.ok, "err", r.err)
	return r.path, r.ok, r.err
}

// closeDialog keeps asking the dialog to close until Show returns (the window
// may not be found yet).
func (p *Picker) closeDialog(res <-chan result) result {
	tick := time.NewTicker(closeInterval)
	defer tick.Stop()
	for {
		h := p.hwnd.Load()
		if h == 0 { // not on screen (yet): close a hidden one too
			h = findDialog(p.thread, false)
		}
		if h != 0 {
			_, _, _ = procPostMessageW.Call(h, wmClose, 0, 0)
		}
		select {
		case r := <-res:
			return r
		case <-tick.C:
		}
	}
}

func ms(d time.Duration) int64 { return d.Milliseconds() }

func createDialog() (*comObj, error) {
	var dlg *comObj
	if hr, _, _ := procCoCreateInstance.Call(uintptr(unsafe.Pointer(&clsidFileOpenDialog)), 0, clsctxInprocServer,
		uintptr(unsafe.Pointer(&iidFileOpenDialog)), uintptr(unsafe.Pointer(&dlg))); uint32(hr) != 0 || dlg == nil {
		return nil, fmt.Errorf("CoCreateInstance(FileOpenDialog): 0x%08X", uint32(hr))
	}
	return dlg, nil
}

// show runs on the STA thread; t0 is when PickFolder was called.
func (p *Picker) show(t0 time.Time, title, initial string) (string, bool, error) {
	p.last = phases{coinit: p.takeCoinit()}
	mark := time.Now()
	dlg, err := createDialog()
	if err != nil {
		return "", false, err
	}
	defer dlg.release()
	p.last.create = time.Since(mark)

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
	mark = time.Now()
	switch {
	case initial == "":
	case IsNetworkPath(initial):
		// Resolving a share can block for seconds (dead host): start in the shell default.
		p.last.skipped = initial
	default:
		setFolder(dlg, initial)
	}
	p.last.folder = time.Since(mark)

	owner := dialogOwner(windows.GetForegroundWindow())
	stop := make(chan struct{})
	done := make(chan struct{})
	var found, front time.Duration
	go p.raise(windows.GetCurrentThreadId(), t0, &found, &front, stop, done)
	mark = time.Now()
	hr := dlg.call(vShow, uintptr(owner))
	p.last.show = time.Since(mark)
	close(stop)
	<-done
	p.last.visible, p.last.front = found, front
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

// dialogOwner is the window the dialog is shown over: a hidden or minimised
// owner would hide the dialog with it, so those get none.
func dialogOwner(fg windows.HWND) windows.HWND {
	if fg == 0 || !windows.IsWindowVisible(fg) {
		return 0
	}
	if r, _, _ := procIsIconic.Call(uintptr(fg)); r != 0 {
		return 0
	}
	return fg
}

// The EnumThreadWindows callback is created once (Go never frees callbacks);
// findMu guards its result. lParam 1 = visible windows only.
var (
	findMu    sync.Mutex
	foundHwnd uintptr
	findEnum  = syscall.NewCallback(func(hwnd windows.HWND, visibleOnly uintptr) uintptr {
		if (visibleOnly == 0 || windows.IsWindowVisible(hwnd)) && className(hwnd) == dialogClass {
			foundHwnd = uintptr(hwnd)
			return 0
		}
		return 1
	})
)

// findDialog returns the dialog window of thread tid, or 0.
func findDialog(tid uint32, visibleOnly bool) uintptr {
	var flag uintptr
	if visibleOnly {
		flag = 1
	}
	findMu.Lock()
	defer findMu.Unlock()
	foundHwnd = 0
	_, _, _ = procEnumThreadWindows.Call(uintptr(tid), findEnum, flag)
	return foundHwnd
}

func className(hwnd windows.HWND) string {
	var buf [64]uint16
	n, _, _ := procGetClassNameW.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	return windows.UTF16ToString(buf[:n])
}

// raise waits for the dialog window of thread tid and brings it to the front:
// the request came from the browser, so this process holds no foreground right
// and borrows the foreground thread's input state for SetForegroundWindow.
// found and front get the times (since t0) the window appeared and got the
// foreground; they are read after done closes.
func (p *Picker) raise(tid uint32, t0 time.Time, found, front *time.Duration, stop <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	runtime.LockOSThread() // AttachThreadInput is per OS thread
	defer runtime.UnlockOSThread()
	tick := time.NewTicker(findDialogInterval)
	defer tick.Stop()
	// Keep looking until Show returns: a cancelled request can only close a
	// window it knows. Only a window found soon is pulled to the front.
	deadline := time.Now().Add(findDialogFor)
	for {
		if h := findDialog(tid, true); h != 0 {
			*found = time.Since(t0)
			p.hwnd.Store(h)
			if time.Now().Before(deadline) {
				bringToFront(h)
				*front = time.Since(t0)
			}
			return
		}
		select {
		case <-stop:
			return
		case <-tick.C:
		}
	}
}

func bringToFront(h uintptr) {
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
}

// phaseLog formats the last dialog's timings (manual timing test).
func (p *Picker) phaseLog() string {
	l := p.last
	return fmt.Sprintf("coinit=%dms create=%dms folder=%dms visible=%dms front=%dms skipped=%q",
		ms(l.coinit), ms(l.create), ms(l.folder), ms(l.visible), ms(l.front), l.skipped)
}
