package syncer

import (
	"context"
	"errors"
	"sync"

	"github.com/UberMorgott/issuewatcher/internal/store"
)

// ErrNoSource means no connected account serves the item's source.
var ErrNoSource = errors.New("syncer: no connected account serves this item")

// labelLocked names the syncer's source: platform, or platform:account once
// the account is known. s.mu must be held.
func (s *Syncer) labelLocked() string {
	if s.opts.Provider == nil {
		return ""
	}
	if s.login == "" {
		return s.opts.Provider.Platform()
	}
	return s.opts.Provider.Platform() + ":" + s.login
}

// sourceID is the store id of the account this syncer serves (0 until known).
func (s *Syncer) sourceID() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.source
}

// Group runs one Syncer per connected account (source). Each syncer keeps its
// own cursor, budget, rate and error state, so one failing source never stops
// the others; replies are routed by the item's source.
//
// Syncers may be added and removed while the group runs (a mod platform
// switched on or off in Settings › Платформы applies without a restart).
type Group struct {
	mu       sync.Mutex
	syncers  []*Syncer
	ctx      context.Context // set by Run; nil before
	wg       sync.WaitGroup
	stops    map[*Syncer]context.CancelFunc
	progress []func(Progress)
	plan     *Plan
}

// NewGroup groups the syncers; the first is the primary one whose status is
// reported at the top level of GroupStatus (the GitHub syncer).
func NewGroup(syncers ...*Syncer) *Group {
	return &Group{syncers: syncers, stops: map[*Syncer]context.CancelFunc{}}
}

// Add appends syncers; while the group runs they start at once, with the
// group's progress listeners and polling plan.
func (g *Group) Add(syncers ...*Syncer) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, s := range syncers {
		for _, f := range g.progress {
			s.OnProgress(f)
		}
		if g.plan != nil {
			s.SetPlan(*g.plan)
		}
		g.syncers = append(g.syncers, s)
		if g.ctx != nil {
			g.startLocked(g.ctx, s)
		}
	}
}

// Remove stops and drops every syncer of platform (never the primary one)
// and returns them.
func (g *Group) Remove(platform string) []*Syncer {
	g.mu.Lock()
	defer g.mu.Unlock()
	var removed []*Syncer
	kept := g.syncers[:0:0]
	for i, s := range g.syncers {
		if i > 0 && s.opts.Provider.Platform() == platform {
			removed = append(removed, s)
			if stop := g.stops[s]; stop != nil {
				stop()
				delete(g.stops, s)
			}
			continue
		}
		kept = append(kept, s)
	}
	g.syncers = kept
	return removed
}

// startLocked runs s under the group's context. g.mu must be held.
func (g *Group) startLocked(parent context.Context, s *Syncer) {
	ctx, stop := context.WithCancel(parent)
	g.stops[s] = stop
	g.wg.Go(func() { s.Run(ctx) })
}

// Syncers returns the grouped syncers.
func (g *Group) Syncers() []*Syncer {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]*Syncer(nil), g.syncers...)
}

// Run runs every syncer's schedule (and those added later) until ctx ends.
func (g *Group) Run(ctx context.Context) {
	g.mu.Lock()
	g.ctx = ctx
	for _, s := range g.syncers {
		g.startLocked(ctx, s)
	}
	g.mu.Unlock()
	<-ctx.Done()
	g.wg.Wait()
}

// Trigger requests a full sync of every source.
func (g *Group) Trigger() {
	for _, s := range g.Syncers() {
		s.Trigger()
	}
}

// SetPlan applies polling settings to every syncer (and to those added later).
func (g *Group) SetPlan(p Plan) {
	g.mu.Lock()
	g.plan = &p
	g.mu.Unlock()
	for _, s := range g.Syncers() {
		s.SetPlan(p)
	}
}

// OnProgress registers f with every syncer, also those added later
// (Progress.Source tells them apart).
func (g *Group) OnProgress(f func(Progress)) {
	g.mu.Lock()
	g.progress = append(g.progress, f)
	g.mu.Unlock()
	for _, s := range g.Syncers() {
		s.OnProgress(f)
	}
}

// SourceStatus is one source's sync status.
type SourceStatus struct {
	Platform string `json:"platform"`
	Account  string `json:"account"`
	Status
}

// GroupStatus is the primary syncer's status (legacy top-level fields) plus
// every source's own status.
type GroupStatus struct {
	Status
	Sources []SourceStatus `json:"sources"`
}

// Status returns a snapshot of every source.
func (g *Group) Status() GroupStatus {
	syncers := g.Syncers()
	out := GroupStatus{Sources: make([]SourceStatus, 0, len(syncers))}
	for i, s := range syncers {
		st := s.Status()
		if i == 0 {
			out.Status = st
		}
		s.mu.Lock()
		login := s.login
		s.mu.Unlock()
		out.Sources = append(out.Sources, SourceStatus{Platform: s.opts.Provider.Platform(), Account: login, Status: st})
	}
	return out
}

// Reply posts body on item id through the syncer of the item's source. An
// item of a platform with a single syncer goes to it even when its source is
// not known yet (signed out → provider.ErrNotSignedIn, account switched → the
// current account, as before per-source syncers).
func (g *Group) Reply(ctx context.Context, itemID int64, body string) (store.Comment, error) {
	syncers := g.Syncers()
	if len(syncers) == 0 {
		return store.Comment{}, ErrNoSource
	}
	ref, err := syncers[0].opts.Store.ItemRef(ctx, itemID)
	if err != nil {
		return store.Comment{}, err
	}
	if s := syncerFor(syncers, ref.Platform, ref.SourceID); s != nil {
		return s.reply(ctx, ref, body)
	}
	return store.Comment{}, ErrNoSource
}

// syncerFor is the syncer of source sourceID, else the only syncer of platform, else nil.
func syncerFor(syncers []*Syncer, platform string, sourceID int64) *Syncer {
	var same []*Syncer
	for _, s := range syncers {
		if s.opts.Provider.Platform() != platform {
			continue
		}
		if s.sourceID() == sourceID {
			return s
		}
		same = append(same, s)
	}
	if len(same) == 1 {
		return same[0]
	}
	return nil
}

// SyncProjects syncs targets now (a project and its linked mod pages), each
// through the syncer of its source, in the background: ctx's values without
// its cancel (the syncs outlive the request), stopped when the group stops. It
// returns the platforms started and those no connected account serves.
func (g *Group) SyncProjects(ctx context.Context, targets []store.SyncTarget) (started, missing []string) {
	run, stop := context.WithCancel(context.WithoutCancel(ctx))
	unhook := func() bool { return false }
	g.mu.Lock()
	if g.ctx != nil {
		unhook = context.AfterFunc(g.ctx, stop) //nolint:contextcheck // only a stop signal: the group's run context ends the syncs; run inherits ctx

	}
	g.mu.Unlock()
	syncers := g.Syncers()
	started, missing = []string{}, []string{}
	var wg sync.WaitGroup
	for _, t := range targets {
		s := syncerFor(syncers, t.Platform, t.SourceID)
		if s == nil {
			missing = append(missing, t.Platform)
			continue
		}
		started = append(started, t.Platform)
		wg.Go(func() {
			if err := s.SyncProject(run, t.Project); err != nil {
				s.opts.Log.Warn("sync project", "project", t.Name, "err", err)
			}
		})
	}
	go func() { wg.Wait(); unhook(); stop() }()
	return started, missing
}
