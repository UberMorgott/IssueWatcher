package folders

import (
	"testing"
	"time"
)

// Cached checks a folder once per CacheTTL; Invalidate forces a new check.
func TestCachedHitsOncePerTTL(t *testing.T) {
	calls := 0
	now := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	checkFn = func(string, string) Status { calls++; return StatusOK }
	cache.now = func() time.Time { return now }
	t.Cleanup(func() { checkFn, cache.now = Check, time.Now; Invalidate() })
	Invalidate()

	for range 5 {
		if st := Cached(`C:\src\app`, "https://github.com/o/app"); st != StatusOK {
			t.Fatalf("status %s", st)
		}
	}
	if calls != 1 {
		t.Fatalf("checks within the TTL: %d, want 1", calls)
	}
	if Cached("", "u") != StatusNone || calls != 1 {
		t.Fatal("an unmapped folder was checked")
	}
	now = now.Add(CacheTTL)
	Cached(`C:\src\app`, "https://github.com/o/app")
	if calls != 2 {
		t.Fatalf("checks after the TTL: %d, want 2", calls)
	}
	Invalidate()
	Cached(`C:\src\app`, "https://github.com/o/app")
	if calls != 3 {
		t.Fatalf("checks after Invalidate: %d, want 3", calls)
	}
}
