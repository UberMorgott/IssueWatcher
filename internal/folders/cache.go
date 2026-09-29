package folders

import (
	"sync"
	"time"
)

// CacheTTL is how long Cached reuses a folder's status.
const CacheTTL = 30 * time.Second

type cached struct {
	st Status
	at time.Time
}

var cache = struct {
	sync.Mutex
	m   map[[2]string]cached
	now func() time.Time
}{m: map[[2]string]cached{}, now: time.Now}

// checkFn is Check (tests count the calls).
var checkFn = Check

// Cached is Check for read paths (project and issue lists, folder page): a
// status is reused for CacheTTL, so a page read does no disk I/O per row.
// Paths that act on a folder (a fix, a mapping) call Check; changes that can
// move a status (a mapping, a finished job, settings) call Invalidate.
func Cached(dir, projectURL string) Status {
	if dir == "" {
		return StatusNone
	}
	k := [2]string{dir, projectURL}
	cache.Lock()
	c, ok := cache.m[k]
	now := cache.now()
	cache.Unlock()
	if ok && now.Sub(c.at) < CacheTTL {
		return c.st
	}
	st := checkFn(dir, projectURL)
	cache.Lock()
	cache.m[k] = cached{st: st, at: now}
	cache.Unlock()
	return st
}

// Invalidate drops every cached status.
func Invalidate() {
	cache.Lock()
	clear(cache.m)
	cache.Unlock()
}
