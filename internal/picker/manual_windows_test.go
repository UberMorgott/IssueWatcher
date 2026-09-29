package picker

import (
	"context"
	"os"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var procFindWindowW = user32.NewProc("FindWindowW")

const (
	wmCommand = 0x0111
	idOK      = 1
)

// TestManualTiming opens the real dialog a few times, closes it as soon as it
// is in front and logs how long each step took. It needs a desktop, so it
// only runs with IW_PICKER_MANUAL=1 (never in CI or plain `go test`).
func TestManualTiming(t *testing.T) {
	if os.Getenv("IW_PICKER_MANUAL") != "1" {
		t.Skip("set IW_PICKER_MANUAL=1 to open real folder dialogs")
	}
	const title = "IW picker timing"
	p := New(nil)
	if os.Getenv("IW_PICKER_WARM") == "1" {
		t0 := time.Now()
		p.Warm()
		t.Logf("warm %v", time.Since(t0).Round(time.Millisecond))
	}
	dirs := []string{"", `\\iw-no-such-host\share`}
	for range 8 {
		dirs = append(dirs, os.TempDir())
	}
	for _, initial := range dirs {
		ctx, cancel := context.WithCancel(context.Background())
		start := time.Now()
		var visible, clicked atomic.Int64
		go func() { // wait until the dialog is on screen, then press its OK button
			defer cancel()
			cls, _ := windows.UTF16PtrFromString(dialogClass)
			name, _ := windows.UTF16PtrFromString(title)
			for time.Since(start) < 10*time.Second {
				h, _, _ := procFindWindowW.Call(uintptr(unsafe.Pointer(cls)), uintptr(unsafe.Pointer(name))) //nolint:gosec // G103: Win32 call takes UTF-16 pointers
				if h != 0 && windows.IsWindowVisible(windows.HWND(h)) {
					visible.Store(int64(time.Since(start)))
					time.Sleep(300 * time.Millisecond)
					clicked.Store(int64(time.Since(start)))
					_, _, _ = procPostMessageW.Call(h, wmCommand, idOK, 0)
					return
				}
				time.Sleep(2 * time.Millisecond)
			}
		}()
		path, ok, err := p.PickFolder(ctx, title, initial)
		total := time.Since(start)
		cancel()
		t.Logf("initial=%q visible=%v ok->return=%v picked=%v %q | %s err=%v", initial,
			time.Duration(visible.Load()).Round(time.Millisecond),
			(total - time.Duration(clicked.Load())).Round(time.Millisecond), ok, path, p.phaseLog(), err)
	}
}
