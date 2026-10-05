package release

import (
	"context"
	"sync"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/provider"
)

// targetCacheTTL is how long a mod page's last PublishTargets answer is shown
// by a local plan before it is refreshed in the background.
const targetCacheTTL = 10 * time.Minute

// targetCache keeps each mod page's last PublishTargets answer and each code
// repository's last default branch, so the local plan (the publish profile
// GET) shows auth state, latest versions and the branch without a network call.
type targetCache struct {
	mu       sync.Mutex
	entries  map[string]cachedTargets // mod page key → last answer
	loading  map[string]bool          // mod page keys being refreshed
	branches map[string]string        // owner/repo → default branch
}

type cachedTargets struct {
	list provider.PublishTargets
	err  error
	at   time.Time
}

func (c *targetCache) get(key string) (cachedTargets, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.entries[key]
	return v, ok
}

func (c *targetCache) put(key string, list provider.PublishTargets, err error, at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = map[string]cachedTargets{}
	}
	c.entries[key] = cachedTargets{list: list, err: err, at: at}
}

// claim marks key as being refreshed; false when a refresh already runs.
func (c *targetCache) claim(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.loading[key] {
		return false
	}
	if c.loading == nil {
		c.loading = map[string]bool{}
	}
	c.loading[key] = true
	return true
}

func (c *targetCache) release(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.loading, key)
}

func (c *targetCache) branch(repo string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.branches[repo]
}

func (c *targetCache) setBranch(repo, branch string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.branches == nil {
		c.branches = map[string]string{}
	}
	c.branches[repo] = branch
}

// publishTargets is the mod page's PublishTargets answer. A full plan asks
// the platform (30 s cap) and caches the answer; a local plan only reads the
// cache (known = false: never asked yet) and refreshes a missing or stale entry
// in the background, so the next read has it.
func (e *Engine) publishTargets(ctx context.Context, pub provider.Publisher, t Target, local bool) (list provider.PublishTargets, known bool, err error) {
	if local {
		c, ok := e.targets.get(t.Key)
		if !ok || e.d.Now().Sub(c.at) > targetCacheTTL {
			e.refreshTargets(pub, t) //nolint:contextcheck // the refresh outlives the request: the engine's context
		}
		return c.list, ok, c.err
	}
	tctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	list, err = pub.PublishTargets(tctx, t.project())
	cancel()
	if ctx.Err() == nil { // a cancelled request says nothing about the platform
		e.targets.put(t.Key, list, err, e.d.Now())
	}
	return list, true, err
}

// refreshTargets asks the platform for t's targets in the background (the
// engine's context; not before Start).
func (e *Engine) refreshTargets(pub provider.Publisher, t Target) {
	e.mu.Lock()
	ctx := e.ctx
	e.mu.Unlock()
	if ctx == nil || !e.targets.claim(t.Key) {
		return
	}
	e.wg.Go(func() {
		defer e.targets.release(t.Key)
		tctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		list, err := pub.PublishTargets(tctx, t.project())
		if ctx.Err() == nil {
			e.targets.put(t.Key, list, err, e.d.Now())
		}
	})
}
