package picker

import (
	"sync"
	"testing"

	"golang.org/x/sys/windows"
)

// TestRunReusesOneThread: every job runs on the same OS thread, even when
// submitted from many goroutines, so COM is initialised once.
func TestRunReusesOneThread(t *testing.T) {
	p := New(nil)
	var mu sync.Mutex
	tids := map[uint32]int{}
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			p.run(func() {
				mu.Lock()
				tids[windows.GetCurrentThreadId()]++
				mu.Unlock()
			})
		})
	}
	wg.Wait()
	if len(tids) != 1 || tids[p.thread] != 20 {
		t.Fatalf("jobs ran on threads %v, want all 20 on %d", tids, p.thread)
	}
}

// TestWarmCreatesDialogClass: Warm initialises COM and creates (and releases)
// the dialog object without showing anything.
func TestWarmCreatesDialogClass(t *testing.T) {
	p := New(nil)
	p.Warm()
	var err error
	p.run(func() {
		var dlg *comObj
		if dlg, err = createDialog(); err == nil {
			dlg.release()
		}
	})
	if err != nil {
		t.Fatal(err)
	}
}
