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
type Group struct {
	syncers []*Syncer
}

// NewGroup groups the syncers; the first is the primary one whose status is
// reported at the top level of GroupStatus (the GitHub syncer).
func NewGroup(syncers ...*Syncer) *Group { return &Group{syncers: syncers} }

// Syncers returns the grouped syncers.
func (g *Group) Syncers() []*Syncer { return g.syncers }

// Run runs every syncer's schedule until ctx ends.
func (g *Group) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for _, s := range g.syncers {
		wg.Go(func() { s.Run(ctx) })
	}
	wg.Wait()
}

// Trigger requests a full sync of every source.
func (g *Group) Trigger() {
	for _, s := range g.syncers {
		s.Trigger()
	}
}

// SetPlan applies polling settings to every syncer.
func (g *Group) SetPlan(p Plan) {
	for _, s := range g.syncers {
		s.SetPlan(p)
	}
}

// OnProgress registers f with every syncer (Progress.Source tells them apart).
func (g *Group) OnProgress(f func(Progress)) {
	for _, s := range g.syncers {
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
	out := GroupStatus{Sources: make([]SourceStatus, 0, len(g.syncers))}
	for i, s := range g.syncers {
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
	if len(g.syncers) == 0 {
		return store.Comment{}, ErrNoSource
	}
	ref, err := g.syncers[0].opts.Store.ItemRef(ctx, itemID)
	if err != nil {
		return store.Comment{}, err
	}
	var same []*Syncer
	for _, s := range g.syncers {
		if s.opts.Provider.Platform() != ref.Platform {
			continue
		}
		if s.sourceID() == ref.SourceID {
			return s.reply(ctx, ref, body)
		}
		same = append(same, s)
	}
	if len(same) == 1 {
		return same[0].reply(ctx, ref, body)
	}
	return store.Comment{}, ErrNoSource
}
