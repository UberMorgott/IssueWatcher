//go:build windows

package notify

import (
	"errors"
	"fmt"
	"image"
	"log/slog"
	"math"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Own popup notifications: one borderless, topmost, no-activate tool window
// per card (no taskbar button, never steals focus), drawn by UpdateLayeredWindow
// from a Go-rendered premultiplied image (per-pixel alpha: rounded corners,
// shadow). Everything runs on the tray thread: other goroutines queue work
// and post wmPopupWake to a message-only host window, whose timer drives the
// slide/fade animation and the auto-hide countdown.

var (
	gdi32  = windows.NewLazySystemDLL("gdi32.dll")
	shcore = windows.NewLazySystemDLL("shcore.dll")

	procCreateCompatibleDC           = gdi32.NewProc("CreateCompatibleDC")
	procCreateDIBSection             = gdi32.NewProc("CreateDIBSection")
	procSelectObject                 = gdi32.NewProc("SelectObject")
	procDeleteObject                 = gdi32.NewProc("DeleteObject")
	procDeleteDC                     = gdi32.NewProc("DeleteDC")
	procUpdateLayeredWindow          = user32.NewProc("UpdateLayeredWindow")
	procSetTimer                     = user32.NewProc("SetTimer")
	procKillTimer                    = user32.NewProc("KillTimer")
	procTrackMouseEvent              = user32.NewProc("TrackMouseEvent")
	procSetCursor                    = user32.NewProc("SetCursor")
	procLoadCursorW                  = user32.NewProc("LoadCursorW")
	procMonitorFromPoint             = user32.NewProc("MonitorFromPoint")
	procGetMonitorInfoW              = user32.NewProc("GetMonitorInfoW")
	procSetThreadDpiAwarenessContext = user32.NewProc("SetThreadDpiAwarenessContext")
	procGetDpiForMonitor             = shcore.NewProc("GetDpiForMonitor")
	procSHQueryUserNotificationState = shell32.NewProc("SHQueryUserNotificationState")
)

const (
	wsPopup          = 0x80000000
	wsExTopmost      = 0x00000008
	wsExToolWindow   = 0x00000080
	wsExLayered      = 0x00080000
	wsExNoActivate   = 0x08000000
	swShowNoActivate = 4
	ulwAlpha         = 2
	acSrcAlpha       = 1
	wmSetCursor      = 0x0020
	wmMouseActivate  = 0x0021
	wmTimer          = 0x0113
	wmMouseLeave     = 0x02A3
	maNoActivate     = 3
	trackLeave       = 0x2
	idcHand          = 32649
	monitorPrimary   = 1 // MONITOR_DEFAULTTOPRIMARY
	errClassExists   = 1410

	wmPopupWake  = wmApp + 2
	popupTimerID = 1
	frameMs      = 15  // animation frame
	idleMs       = 200 // countdown only
	slideIn      = 56.0
	moveTau      = 0.07 // s, slide easing time constant
	fadeTime     = 0.18 // s
)

var (
	dpiPerMonitorV2 = ^uintptr(3) // DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2 = -4
	hwndMessage     = ^uintptr(2) // HWND_MESSAGE = -3

	activePopups    *popupManager // tray thread only
	popupHostProc   = windows.NewCallback(popupHostWndProc)
	popupWindowProc = windows.NewCallback(popupWndProc)
)

type (
	winRect     struct{ Left, Top, Right, Bottom int32 }
	winSize     struct{ CX, CY int32 }
	monitorInfo struct {
		Size          uint32
		Monitor, Work winRect
		Flags         uint32
	}
	blendFunction struct{ BlendOp, BlendFlags, SourceConstantAlpha, AlphaFormat byte }
	bitmapInfo    struct {
		Size                         uint32
		Width, Height                int32
		Planes, BitCount             uint16
		Compression, SizeImage       uint32
		XPelsPerMeter, YPelsPerMeter int32
		ClrUsed, ClrImportant        uint32
		Colors                       [1]uint32
	}
	trackMouse struct {
		Size, Flags uint32
		Track       windows.HWND
		HoverTime   uint32
	}
)

// popupWin is one card window and its bitmap.
type popupWin struct {
	id      uint64
	hwnd    windows.HWND
	dc, bmp uintptr
	bits    []byte
	size    image.Point
	card    Card
	scale   float64
	bodyH   float64
	hot     int
	x, y    float64 // current position
	tx, ty  int     // target position
	alpha   float64
	closing bool
	shown   bool
}

type popupManager struct {
	host    windows.HWND
	log     *slog.Logger
	onClick func(path string)

	mu        sync.Mutex // guards the queued fields below
	pending   []Card
	nextTheme *Theme
	nextHide  time.Duration
	simulate  bool

	// tray thread only
	st      *stack
	theme   Theme
	wins    map[uint64]*popupWin
	hover   uint64
	timerMs int
	last    time.Time
}

// enterPerMonitorDPI makes the calling thread per-monitor DPI aware (v2) so
// windows it creates and the metrics it reads are in physical pixels.
func enterPerMonitorDPI() func() {
	if procSetThreadDpiAwarenessContext.Find() != nil {
		return func() {}
	}
	old, _, _ := procSetThreadDpiAwarenessContext.Call(dpiPerMonitorV2)
	return func() {
		if old != 0 {
			_, _, _ = procSetThreadDpiAwarenessContext.Call(old)
		}
	}
}

func registerClass(name string, proc uintptr) (*uint16, error) {
	cls, _ := windows.UTF16PtrFromString(name)
	instance, _, _ := procGetModuleHandleW.Call(0)
	wc := wndClassEx{WndProc: proc, Instance: windows.Handle(instance), ClassName: cls}
	wc.Size = uint32(unsafe.Sizeof(wc))
	if r, _, err := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc))); r == 0 && !errors.Is(err, windows.Errno(errClassExists)) {
		return nil, fmt.Errorf("popup: RegisterClassEx %s: %w", name, err)
	}
	return cls, nil
}

// newPopupManager runs on the tray thread.
func newPopupManager(theme Theme, autoHide time.Duration, onClick func(string), log *slog.Logger) (*popupManager, error) {
	if err := loadFonts(); err != nil {
		return nil, err
	}
	if _, err := registerClass("IssueWatcherPopup", popupWindowProc); err != nil {
		return nil, err
	}
	cls, err := registerClass("IssueWatcherPopupHost", popupHostProc)
	if err != nil {
		return nil, err
	}
	restore := enterPerMonitorDPI()
	defer restore()
	instance, _, _ := procGetModuleHandleW.Call(0)
	h, _, err := procCreateWindowExW.Call(0, uintptr(unsafe.Pointer(cls)), uintptr(unsafe.Pointer(cls)),
		0, 0, 0, 0, 0, hwndMessage, 0, instance, 0)
	if h == 0 {
		return nil, fmt.Errorf("popup: host window: %w", err)
	}
	m := &popupManager{
		host: windows.HWND(h), log: log, onClick: onClick,
		st: newStack(autoHide), theme: theme, wins: map[uint64]*popupWin{},
	}
	activePopups = m
	return m, nil
}

func (m *popupManager) wake() {
	_, _, _ = procPostMessageW.Call(uintptr(m.host), wmPopupWake, 0, 0)
}

func (m *popupManager) show(cards []Card) {
	m.mu.Lock()
	m.pending = append(m.pending, cards...)
	m.mu.Unlock()
	m.wake()
}

func (m *popupManager) setTheme(th Theme) {
	m.mu.Lock()
	m.nextTheme = &th
	m.mu.Unlock()
	m.wake()
}

func (m *popupManager) setAutoHide(d time.Duration) {
	m.mu.Lock()
	m.nextHide = d
	m.mu.Unlock()
	m.wake()
}

func (m *popupManager) simulateClick() {
	m.mu.Lock()
	m.simulate = true
	m.mu.Unlock()
	m.wake()
}

// onWake applies queued work (tray thread).
func (m *popupManager) onWake() {
	m.mu.Lock()
	cards, th, hide, sim := m.pending, m.nextTheme, m.nextHide, m.simulate
	m.pending, m.nextTheme, m.nextHide, m.simulate = nil, nil, 0, false
	m.mu.Unlock()

	if hide > 0 {
		m.st.autoHide = hide
	}
	if th != nil {
		m.theme = *th
		for _, w := range m.wins {
			w.scale = 0 // re-render
		}
	}
	for _, c := range cards {
		m.st.push(c)
	}
	if sim {
		if v := m.st.visible(); len(v) > 0 {
			m.click(v[0].id)
			return
		}
	}
	m.sync()
}

// primaryMetrics returns the primary monitor and work area in physical pixels
// and its scale (effective DPI / 96).
func primaryMetrics() (monitor, work image.Rectangle, scale float64) {
	hmon, _, _ := procMonitorFromPoint.Call(0, monitorPrimary)
	mi := monitorInfo{}
	mi.Size = uint32(unsafe.Sizeof(mi))
	scale = 1
	if r, _, _ := procGetMonitorInfoW.Call(hmon, uintptr(unsafe.Pointer(&mi))); r == 0 {
		return image.Rect(0, 0, 1920, 1080), image.Rect(0, 0, 1920, 1040), scale
	}
	rect := func(r winRect) image.Rectangle {
		return image.Rect(int(r.Left), int(r.Top), int(r.Right), int(r.Bottom))
	}
	var dx, dy uint32
	if procGetDpiForMonitor.Find() == nil {
		if r, _, _ := procGetDpiForMonitor.Call(hmon, 0, uintptr(unsafe.Pointer(&dx)), uintptr(unsafe.Pointer(&dy))); r == 0 && dx > 0 {
			scale = float64(dx) / 96
		}
	}
	return rect(mi.Monitor), rect(mi.Work), scale
}

// sync matches the windows to the stack: creates and renders shown cards,
// sets their target positions and starts closing the rest.
func (m *popupManager) sync() {
	restore := enterPerMonitorDPI()
	defer restore()
	monitor, work, scale := primaryMetrics()
	vis := m.st.visible()
	live := make(map[uint64]bool, len(vis))
	var (
		shown []*popupWin
		sizes []image.Point
	)
	for _, e := range vis {
		w := m.wins[e.id]
		if w == nil {
			var err error
			if w, err = m.newWin(e.id); err != nil {
				m.log.Error("popup: create window", "err", err)
				continue
			}
		}
		if w.card != e.card || w.scale != scale {
			w.card = e.card
			if err := m.render(w, scale); err != nil {
				m.log.Error("popup: render", "err", err)
				continue
			}
		}
		live[e.id] = true
		shown = append(shown, w)
		sizes = append(sizes, w.size)
	}
	slide := float64(px(slideIn, scale))
	for i, p := range placeStack(monitor, work, sizes, scale) {
		w := shown[i]
		w.tx, w.ty = p.X, p.Y
		if !w.shown {
			w.x, w.y, w.alpha = float64(p.X)+slide, float64(p.Y), 0
			m.present(w)
			_, _, _ = procShowWindow.Call(uintptr(w.hwnd), swShowNoActivate)
			w.shown = true
		}
	}
	for id, w := range m.wins {
		if !live[id] && !w.closing {
			w.closing = true
			w.tx = int(w.x + slide)
		}
	}
	m.startTimer(frameMs)
}

func (m *popupManager) newWin(id uint64) (*popupWin, error) {
	cls, _ := windows.UTF16PtrFromString("IssueWatcherPopup")
	title, _ := windows.UTF16PtrFromString("IssueWatcher")
	instance, _, _ := procGetModuleHandleW.Call(0)
	h, _, err := procCreateWindowExW.Call(wsExLayered|wsExTopmost|wsExToolWindow|wsExNoActivate,
		uintptr(unsafe.Pointer(cls)), uintptr(unsafe.Pointer(title)), wsPopup,
		0, 0, 0, 0, 0, 0, instance, 0)
	if h == 0 {
		return nil, fmt.Errorf("CreateWindowEx: %w", err)
	}
	w := &popupWin{id: id, hwnd: windows.HWND(h)}
	m.wins[id] = w
	return w, nil
}

// render draws w.card at scale into the window's DIB (premultiplied BGRA).
func (m *popupManager) render(w *popupWin, scale float64) error {
	img, l, err := renderCard(w.card, m.theme, scale, w.hot)
	if err != nil {
		return err
	}
	w.scale, w.bodyH = scale, l.bodyH
	size := img.Rect.Size()
	if size != w.size || w.dc == 0 {
		m.freeBitmap(w)
		dc, _, _ := procCreateCompatibleDC.Call(0)
		if dc == 0 {
			return errors.New("CreateCompatibleDC failed")
		}
		bi := bitmapInfo{Width: int32(size.X), Height: -int32(size.Y), Planes: 1, BitCount: 32} // top-down
		bi.Size = uint32(unsafe.Sizeof(bi) - unsafe.Sizeof(bi.Colors))
		var bits unsafe.Pointer
		bmp, _, err := procCreateDIBSection.Call(dc, uintptr(unsafe.Pointer(&bi)), 0, uintptr(unsafe.Pointer(&bits)), 0, 0)
		if bmp == 0 || bits == nil {
			_, _, _ = procDeleteDC.Call(dc)
			return fmt.Errorf("CreateDIBSection: %w", err)
		}
		_, _, _ = procSelectObject.Call(dc, bmp)
		w.dc, w.bmp, w.size = dc, bmp, size
		w.bits = unsafe.Slice((*byte)(bits), size.X*size.Y*4)
	}
	for i := 0; i+3 < len(img.Pix); i += 4 { // RGBA → BGRA, both premultiplied
		w.bits[i], w.bits[i+1], w.bits[i+2], w.bits[i+3] = img.Pix[i+2], img.Pix[i+1], img.Pix[i], img.Pix[i+3]
	}
	if w.shown {
		m.present(w)
	}
	return nil
}

// present pushes the bitmap at the window's current position and opacity.
func (m *popupManager) present(w *popupWin) {
	dst := point{X: int32(math.Round(w.x)), Y: int32(math.Round(w.y))}
	src := point{}
	sz := winSize{CX: int32(w.size.X), CY: int32(w.size.Y)}
	bf := blendFunction{SourceConstantAlpha: uint8(math.Round(255 * min(max(w.alpha, 0), 1))), AlphaFormat: acSrcAlpha}
	_, _, _ = procUpdateLayeredWindow.Call(uintptr(w.hwnd), 0, uintptr(unsafe.Pointer(&dst)), uintptr(unsafe.Pointer(&sz)),
		w.dc, uintptr(unsafe.Pointer(&src)), 0, uintptr(unsafe.Pointer(&bf)), ulwAlpha)
}

func (m *popupManager) freeBitmap(w *popupWin) {
	if w.dc != 0 {
		_, _, _ = procDeleteDC.Call(w.dc)
	}
	if w.bmp != 0 {
		_, _, _ = procDeleteObject.Call(w.bmp)
	}
	w.dc, w.bmp, w.bits, w.size = 0, 0, nil, image.Point{}
}

func (m *popupManager) destroyWin(w *popupWin) {
	if m.hover == w.id {
		m.hover = 0
		m.st.setHovered(false)
	}
	_, _, _ = procDestroyWindow.Call(uintptr(w.hwnd))
	m.freeBitmap(w)
	delete(m.wins, w.id)
}

func (m *popupManager) startTimer(ms int) {
	if m.timerMs == ms {
		return
	}
	if m.timerMs == 0 {
		m.last = time.Now()
	}
	m.timerMs = ms
	_, _, _ = procSetTimer.Call(uintptr(m.host), popupTimerID, uintptr(ms), 0)
}

func (m *popupManager) stopTimer() {
	if m.timerMs != 0 {
		_, _, _ = procKillTimer.Call(uintptr(m.host), popupTimerID)
		m.timerMs = 0
	}
}

// onTimer advances the countdown and the animation (tray thread).
func (m *popupManager) onTimer() {
	now := time.Now()
	dt := min(now.Sub(m.last), 100*time.Millisecond)
	m.last = now
	if m.st.tick(dt) {
		m.sync()
	}
	sec := dt.Seconds()
	k := 1 - math.Exp(-sec/moveTau)
	animating := false
	for _, w := range m.wins {
		ox, oy, oa := w.x, w.y, w.alpha
		w.x += (float64(w.tx) - w.x) * k
		w.y += (float64(w.ty) - w.y) * k
		if math.Abs(float64(w.tx)-w.x) < 0.5 {
			w.x = float64(w.tx)
		}
		if math.Abs(float64(w.ty)-w.y) < 0.5 {
			w.y = float64(w.ty)
		}
		if w.closing {
			w.alpha = max(w.alpha-sec/fadeTime, 0)
		} else {
			w.alpha = min(w.alpha+sec/fadeTime, 1)
		}
		if w.closing && w.alpha == 0 {
			m.destroyWin(w)
			continue
		}
		if w.x != ox || w.y != oy || w.alpha != oa {
			m.present(w)
			animating = true
		}
	}
	switch {
	case len(m.wins) == 0:
		m.stopTimer()
	case animating:
		m.startTimer(frameMs)
	default:
		m.startTimer(idleMs)
	}
}

func (m *popupManager) click(id uint64) {
	path, ok := m.st.click(id)
	if ok {
		m.log.Info("notification clicked", "path", path)
		if m.onClick != nil {
			allowForeground() // a new browser tab may raise its window
			go m.onClick(path)
		}
	}
	m.sync()
}

func (m *popupManager) handle(w *popupWin, message uint32, lParam uintptr) (uintptr, bool) {
	switch message {
	case wmMouseActivate:
		return maNoActivate, true
	case wmSetCursor:
		cur, _, _ := procLoadCursorW.Call(0, idcHand)
		_, _, _ = procSetCursor.Call(cur)
		return 1, true
	case wmMouseMove:
		if m.hover != w.id {
			m.hover = w.id
			m.st.setHovered(true)
			tm := trackMouse{Flags: trackLeave, Track: w.hwnd}
			tm.Size = uint32(unsafe.Sizeof(tm))
			_, _, _ = procTrackMouseEvent.Call(uintptr(unsafe.Pointer(&tm)))
		}
		m.setHot(w, hitZone(int(int16(lParam&0xFFFF)), int(int16(lParam>>16&0xFFFF)), w.bodyH, w.scale))
		return 0, true
	case wmMouseLeave:
		if m.hover == w.id {
			m.hover = 0
			m.st.setHovered(false)
		}
		m.setHot(w, hitNone)
		return 0, true
	case wmLButtonUp:
		if w.closing {
			return 0, true
		}
		switch hitZone(int(int16(lParam&0xFFFF)), int(int16(lParam>>16&0xFFFF)), w.bodyH, w.scale) {
		case hitClose:
			m.st.close(w.id)
			m.sync()
		case hitBody:
			m.click(w.id)
		}
		return 0, true
	}
	return 0, false
}

func (m *popupManager) setHot(w *popupWin, hot int) {
	if w.hot == hot || w.closing {
		return
	}
	w.hot = hot
	if err := m.render(w, w.scale); err != nil {
		m.log.Error("popup: render", "err", err)
	}
}

// shutdown destroys every window (tray thread, on exit).
func (m *popupManager) shutdown() {
	m.stopTimer()
	for _, w := range m.wins {
		m.destroyWin(w)
	}
	_, _, _ = procDestroyWindow.Call(uintptr(m.host))
	activePopups = nil
}

func popupHostWndProc(hwnd, message, wParam, lParam uintptr) uintptr {
	if m := activePopups; m != nil && windows.HWND(hwnd) == m.host {
		switch uint32(message) {
		case wmPopupWake:
			m.onWake()
			return 0
		case wmTimer:
			if wParam == popupTimerID {
				m.onTimer()
				return 0
			}
		}
	}
	r, _, _ := procDefWindowProcW.Call(hwnd, message, wParam, lParam)
	return r
}

func popupWndProc(hwnd, message, wParam, lParam uintptr) uintptr {
	if m := activePopups; m != nil {
		for _, w := range m.wins {
			if w.hwnd == windows.HWND(hwnd) {
				if r, ok := m.handle(w, uint32(message), lParam); ok {
					return r
				}
				break
			}
		}
	}
	r, _, _ := procDefWindowProcW.Call(hwnd, message, wParam, lParam)
	return r
}

// WindowsBusy reports whether Windows asks apps to hold notifications: a
// full-screen app or game, or presentation mode (SHQueryUserNotificationState).
// Focus assist / Do Not Disturb has no public API and is not detected.
func WindowsBusy() bool {
	if procSHQueryUserNotificationState.Find() != nil {
		return false
	}
	var state int32
	if r, _, _ := procSHQueryUserNotificationState.Call(uintptr(unsafe.Pointer(&state))); r != 0 {
		return false
	}
	switch state {
	case 2, 3, 4: // QUNS_BUSY, QUNS_RUNNING_D3D_FULL_SCREEN, QUNS_PRESENTATION_MODE
		return true
	}
	return false
}
