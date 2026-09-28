//go:build windows

package notify

import (
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/UberMorgott/issuewatcher/internal/store"
)

// Own minimal Win32 tray: icon with a badge, menu, click. Notifications are the
// app's own popup windows (popup_windows.go), not tray balloons: Windows
// swallows balloons under Do Not Disturb / Focus assist and never reports
// clicks made in the notification center.

var (
	user32   = windows.NewLazySystemDLL("user32.dll")
	shell32  = windows.NewLazySystemDLL("shell32.dll")
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")

	procRegisterClassExW         = user32.NewProc("RegisterClassExW")
	procCreateWindowExW          = user32.NewProc("CreateWindowExW")
	procDefWindowProcW           = user32.NewProc("DefWindowProcW")
	procDestroyWindow            = user32.NewProc("DestroyWindow")
	procGetMessageW              = user32.NewProc("GetMessageW")
	procTranslateMessage         = user32.NewProc("TranslateMessage")
	procDispatchMessageW         = user32.NewProc("DispatchMessageW")
	procPostMessageW             = user32.NewProc("PostMessageW")
	procPostQuitMessage          = user32.NewProc("PostQuitMessage")
	procCreatePopupMenu          = user32.NewProc("CreatePopupMenu")
	procAppendMenuW              = user32.NewProc("AppendMenuW")
	procTrackPopupMenu           = user32.NewProc("TrackPopupMenu")
	procDestroyMenu              = user32.NewProc("DestroyMenu")
	procSetForegroundWindow      = user32.NewProc("SetForegroundWindow")
	procGetCursorPos             = user32.NewProc("GetCursorPos")
	procCreateIconFromResourceEx = user32.NewProc("CreateIconFromResourceEx")
	procDestroyIcon              = user32.NewProc("DestroyIcon")
	procGetSystemMetrics         = user32.NewProc("GetSystemMetrics")
	procRegisterWindowMessageW   = user32.NewProc("RegisterWindowMessageW")
	procShellNotifyIconW         = shell32.NewProc("Shell_NotifyIconW")
	procGetModuleHandleW         = kernel32.NewProc("GetModuleHandleW")
)

const (
	wmNull        = 0x0000
	wmDestroy     = 0x0002
	wmClose       = 0x0010
	wmContextMenu = 0x007B
	wmUser        = 0x0400
	wmApp         = 0x8000

	wmTrayCallback = wmApp + 1

	wmMouseMove     = 0x0200
	wmLButtonDown   = 0x0201
	wmLButtonUp     = 0x0202
	wmLButtonDblClk = 0x0203
	wmRButtonDown   = 0x0204
	wmRButtonUp     = 0x0205

	ninSelect     = wmUser + 0
	ninKeySelect  = wmUser + 1
	ninPopupOpen  = wmUser + 6 // hover tooltip shown / hidden
	ninPopupClose = wmUser + 7

	nimAdd        = 0
	nimModify     = 1
	nimDelete     = 2
	nimSetVersion = 4

	nifMessage  = 0x01
	nifIcon     = 0x02
	nifTip      = 0x04
	nifShowTip  = 0x80
	notifyIconV = 4 // NOTIFYICON_VERSION_4

	mfString    = 0x0000
	mfSeparator = 0x0800
	tpmRightBtn = 0x0002
	tpmNoNotify = 0x0080
	tpmRetCmd   = 0x0100

	smCxSmIcon = 49
	smCySmIcon = 50
)

type notifyIconData struct {
	Size            uint32
	Wnd             windows.HWND
	ID              uint32
	Flags           uint32
	CallbackMessage uint32
	Icon            windows.Handle
	Tip             [128]uint16
	State           uint32
	StateMask       uint32
	Info            [256]uint16
	Version         uint32 // union with uTimeout
	InfoTitle       [64]uint16
	InfoFlags       uint32
	GUIDItem        windows.GUID
	BalloonIcon     windows.Handle
}

type wndClassEx struct {
	Size       uint32
	Style      uint32
	WndProc    uintptr
	ClsExtra   int32
	WndExtra   int32
	Instance   windows.Handle
	Icon       windows.Handle
	Cursor     windows.Handle
	Background windows.Handle
	MenuName   *uint16
	ClassName  *uint16
	IconSm     windows.Handle
}

type point struct{ X, Y int32 }

type msg struct {
	Hwnd     windows.HWND
	Message  uint32
	WParam   uintptr
	LParam   uintptr
	Time     uint32
	Pt       point
	LPrivate uint32
}

// MenuItem is one tray menu entry; an empty Title is a separator.
type MenuItem struct {
	Title   string
	OnClick func()
}

// TrayOptions configures RunTray. Handlers run on their own goroutine.
type TrayOptions struct {
	Tooltip     string
	Icon        []byte // PNG
	Menu        []MenuItem
	OnClick     func()            // left click / keyboard select on the icon
	OnCardClick func(path string) // click on a popup card: dashboard route to open
	Theme       Theme             // popup palette; zero = DarkTheme
	Prefs       *Prefs            // notification settings; nil = DefaultPrefs
	Log         *slog.Logger      // required; tray callbacks at debug level (not mouse moves)
}

// selectDebounce folds the two NIN_SELECT messages of a double-click into one open.
const selectDebounce = 400 * time.Millisecond

// Tray is a running tray icon. Methods are safe from any goroutine.
type Tray struct {
	opts           TrayOptions
	hwnd           windows.HWND
	taskbarCreated uint32

	mu    sync.Mutex
	icon  windows.Handle
	prefs Prefs

	popups     *popupManager
	lastSelect time.Time // message-loop thread only
}

var (
	activeTray *Tray // one tray per process: the window procedure is global
	wndProcPtr = windows.NewCallback(wndProc)
)

// RunTray shows the icon and runs the message loop on the calling goroutine
// (locked to its OS thread) until Quit. ready runs once the icon exists.
func RunTray(opts TrayOptions, ready func(*Tray)) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	if activeTray != nil {
		return errors.New("tray: already running")
	}
	t := &Tray{opts: opts, prefs: DefaultPrefs()}
	if opts.Prefs != nil {
		t.prefs = *opts.Prefs
	}
	activeTray = t
	defer func() { activeTray = nil }()

	if err := t.createWindow(); err != nil {
		return err
	}
	theme := opts.Theme
	if theme == (Theme{}) {
		theme = DarkTheme()
	}
	p, err := newPopupManager(theme, t.prefs.AutoHide, opts.OnCardClick, opts.Log)
	if err != nil {
		return err
	}
	t.popups = p
	if err := t.SetIcon(opts.Icon); err != nil {
		return err
	}
	if err := t.add(); err != nil {
		return err
	}
	if ready != nil {
		ready(t)
	}

	var m msg
	for {
		r, _, err := procGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		switch int32(r) {
		case -1:
			return fmt.Errorf("tray: GetMessage: %w", err)
		case 0:
			return nil // WM_QUIT
		}
		_, _, _ = procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		_, _, _ = procDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
	}
}

func (t *Tray) createWindow() error {
	className, _ := windows.UTF16PtrFromString("IssueWatcherTray")
	instance, _, _ := procGetModuleHandleW.Call(0)
	wc := wndClassEx{
		WndProc:   wndProcPtr,
		Instance:  windows.Handle(instance),
		ClassName: className,
	}
	wc.Size = uint32(unsafe.Sizeof(wc))
	if r, _, err := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc))); r == 0 {
		return fmt.Errorf("tray: RegisterClassEx: %w", err)
	}

	// Hidden top-level window (not message-only: TrackPopupMenu needs a window
	// that can take the foreground so the menu closes on outside clicks).
	hwnd, _, err := procCreateWindowExW.Call(0,
		uintptr(unsafe.Pointer(className)), uintptr(unsafe.Pointer(className)),
		0, 0, 0, 0, 0, 0, 0, instance, 0)
	if hwnd == 0 {
		return fmt.Errorf("tray: CreateWindowEx: %w", err)
	}
	t.hwnd = windows.HWND(hwnd)

	name, _ := windows.UTF16PtrFromString("TaskbarCreated")
	r, _, _ := procRegisterWindowMessageW.Call(uintptr(unsafe.Pointer(name)))
	t.taskbarCreated = uint32(r)
	return nil
}

func (t *Tray) baseData() notifyIconData {
	nid := notifyIconData{Wnd: t.hwnd, ID: 1}
	nid.Size = uint32(unsafe.Sizeof(nid))
	return nid
}

func (t *Tray) add() error {
	nid := t.baseData()
	nid.Flags = nifMessage | nifIcon | nifTip | nifShowTip
	nid.CallbackMessage = wmTrayCallback
	t.mu.Lock()
	nid.Icon = t.icon
	t.mu.Unlock()
	copyUTF16(nid.Tip[:], t.opts.Tooltip)
	if err := shellNotify(nimAdd, &nid); err != nil {
		return err
	}
	nid.Version = notifyIconV
	return shellNotify(nimSetVersion, &nid)
}

// SetIcon replaces the tray icon with a PNG image.
func (t *Tray) SetIcon(png []byte) error {
	cx, _, _ := procGetSystemMetrics.Call(smCxSmIcon)
	cy, _, _ := procGetSystemMetrics.Call(smCySmIcon)
	if len(png) == 0 {
		return errors.New("tray: empty icon")
	}
	// CreateIconFromResourceEx accepts raw PNG data (Vista+).
	h, _, err := procCreateIconFromResourceEx.Call(
		uintptr(unsafe.Pointer(&png[0])), uintptr(len(png)), 1, 0x00030000, cx, cy, 0)
	if h == 0 {
		return fmt.Errorf("tray: CreateIconFromResourceEx: %w", err)
	}

	t.mu.Lock()
	old := t.icon
	t.icon = windows.Handle(h)
	t.mu.Unlock()

	var nerr error
	if old != 0 { // icon already shown: update it
		nid := t.baseData()
		nid.Flags = nifIcon
		nid.Icon = windows.Handle(h)
		nerr = shellNotify(nimModify, &nid)
		_, _, _ = procDestroyIcon.Call(uintptr(old))
	}
	return nerr
}

// ShowCards pops up notification cards (newest last) in the screen corner.
func (t *Tray) ShowCards(cards ...Card) {
	if len(cards) > 0 {
		t.popups.show(cards)
	}
}

// SetTheme recolours the popups (shown cards re-render).
func (t *Tray) SetTheme(th Theme) { t.popups.setTheme(th) }

// SetPrefs replaces the notification settings (safe at runtime; AutoHide
// applies to cards shown from now on).
func (t *Tray) SetPrefs(p Prefs) {
	t.mu.Lock()
	t.prefs = p
	t.mu.Unlock()
	t.popups.setAutoHide(p.AutoHide)
}

// NotifyEvents pops up cards for sync events allowed by the current Prefs and
// returns how many it showed.
func (t *Tray) NotifyEvents(events []store.Event) int {
	t.mu.Lock()
	p := t.prefs
	t.mu.Unlock()
	cards := p.Pick(events, time.Now(), WindowsBusy)
	t.ShowCards(cards...)
	if len(events) > 0 {
		t.opts.Log.Info("notifications", "events", len(events), "shown", len(cards))
	}
	return len(cards)
}

// NotifyCard shows one app card (agent job finished) unless notifications are
// off, quiet hours run or Windows is busy (full screen) and that is respected.
func (t *Tray) NotifyCard(c Card) bool {
	t.mu.Lock()
	p := t.prefs
	t.mu.Unlock()
	if !p.Enabled || p.Quiet(time.Now()) || (p.RespectWindowsDnd && WindowsBusy()) {
		return false
	}
	t.ShowCards(c)
	return true
}

// SimulateCardClick clicks the newest shown card (dev/demo verification
// without a mouse).
func (t *Tray) SimulateCardClick() { t.popups.simulateClick() }

// Quit removes the icon and ends RunTray.
func (t *Tray) Quit() {
	_, _, _ = procPostMessageW.Call(uintptr(t.hwnd), wmClose, 0, 0)
}

func (t *Tray) showMenu() {
	menu, _, _ := procCreatePopupMenu.Call()
	if menu == 0 {
		return
	}
	defer func() { _, _, _ = procDestroyMenu.Call(menu) }()

	for i, item := range t.opts.Menu {
		if item.Title == "" {
			_, _, _ = procAppendMenuW.Call(menu, mfSeparator, 0, 0)
			continue
		}
		title, _ := windows.UTF16PtrFromString(item.Title)
		_, _, _ = procAppendMenuW.Call(menu, mfString, uintptr(i+1), uintptr(unsafe.Pointer(title)))
	}

	var pt point
	_, _, _ = procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	_, _, _ = procSetForegroundWindow.Call(uintptr(t.hwnd))
	cmd, _, _ := procTrackPopupMenu.Call(menu, tpmRightBtn|tpmNoNotify|tpmRetCmd,
		uintptr(pt.X), uintptr(pt.Y), 0, uintptr(t.hwnd), 0)
	_, _, _ = procPostMessageW.Call(uintptr(t.hwnd), wmNull, 0, 0) // documented menu-dismiss quirk

	if i := int(cmd) - 1; i >= 0 && i < len(t.opts.Menu) {
		dispatch(t.opts.Menu[i].OnClick)
	}
}

func (t *Tray) handleCallback(event uint16) {
	switch event {
	case wmMouseMove, ninPopupOpen, ninPopupClose: // every hover: too chatty even for debug
	default:
		t.opts.Log.Debug("tray callback", "msg", fmt.Sprintf("0x%04X", event), "name", callbackName(event))
	}
	switch event {
	case ninSelect, ninKeySelect:
		now := time.Now()
		if now.Sub(t.lastSelect) < selectDebounce {
			return // second click of a double-click
		}
		t.lastSelect = now
		dispatch(t.opts.OnClick)
	case wmContextMenu:
		t.showMenu()
	}
}

// callbackName names NOTIFYICON_VERSION_4 callback events for the debug log.
func callbackName(event uint16) string {
	switch event {
	case ninSelect:
		return "NIN_SELECT"
	case ninKeySelect:
		return "NIN_KEYSELECT"
	case wmContextMenu:
		return "WM_CONTEXTMENU"
	case wmLButtonDown:
		return "WM_LBUTTONDOWN"
	case wmLButtonUp:
		return "WM_LBUTTONUP"
	case wmLButtonDblClk:
		return "WM_LBUTTONDBLCLK"
	case wmRButtonDown:
		return "WM_RBUTTONDOWN"
	case wmRButtonUp:
		return "WM_RBUTTONUP"
	}
	return ""
}

func wndProc(hwnd, message, wParam, lParam uintptr) uintptr {
	t := activeTray
	if t != nil && windows.HWND(hwnd) == t.hwnd {
		switch uint32(message) {
		case wmTrayCallback:
			t.handleCallback(uint16(lParam & 0xFFFF)) // v4: LOWORD = event
			return 0
		case wmClose:
			_, _, _ = procDestroyWindow.Call(hwnd)
			return 0
		case wmDestroy:
			if t.popups != nil {
				t.popups.shutdown()
			}
			nid := t.baseData()
			_ = shellNotify(nimDelete, &nid)
			t.mu.Lock()
			if t.icon != 0 {
				_, _, _ = procDestroyIcon.Call(uintptr(t.icon))
				t.icon = 0
			}
			t.mu.Unlock()
			_, _, _ = procPostQuitMessage.Call(0)
			return 0
		case t.taskbarCreated: // Explorer restarted: the icon is gone, add it back
			_ = t.add()
			return 0
		}
	}
	r, _, _ := procDefWindowProcW.Call(hwnd, message, wParam, lParam)
	return r
}

func dispatch(h func()) {
	if h != nil {
		go h()
	}
}

func shellNotify(cmd uintptr, nid *notifyIconData) error {
	if r, _, err := procShellNotifyIconW.Call(cmd, uintptr(unsafe.Pointer(nid))); r == 0 {
		return fmt.Errorf("tray: Shell_NotifyIcon(%d): %w", cmd, err)
	}
	return nil
}

// copyUTF16 writes s into a fixed NUL-terminated buffer, truncating if needed.
func copyUTF16(dst []uint16, s string) {
	u, err := windows.UTF16FromString(s)
	if err != nil {
		return // s contains NUL: leave the field empty
	}
	if len(u) > len(dst) {
		u = u[:len(dst)]
		u[len(u)-1] = 0
	}
	copy(dst, u)
}
