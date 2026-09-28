// Package syncer polls a provider into the store and reports changes
// (new issue, new comment, issue closed) plus the unread count for the tray.
package syncer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/provider"
	"github.com/UberMorgott/issuewatcher/internal/store"
)

// DefaultInterval is the poll period when none is configured.
const DefaultInterval = 5 * time.Minute

// Options configures a Syncer.
type Options struct {
	Store    *store.Store
	Provider provider.Provider
	Interval time.Duration // default DefaultInterval
	// OnUpdate runs after every completed cycle with the detected events
	// (possibly none) and the current unread count.
	OnUpdate func(events []store.Event, unread int)
	Log      *slog.Logger
}

// Status is reported by GET /api/sync.
type Status struct {
	Running          bool   `json:"running"`
	SignedIn         bool   `json:"signedIn"`
	LastSync         string `json:"lastSync"`
	LastError        string `json:"lastError"`
	RateLimitedUntil string `json:"rateLimitedUntil"`
	Interval         string `json:"interval"`
}

// Progress states reported to OnProgress listeners (SSE sync.status).
const (
	ProgressStarted = "started"
	ProgressRepo    = "progress"
	ProgressDone    = "done"
	ProgressError   = "error"
)

// Progress is one step of a sync cycle: started (project list known), one
// progress per project, then done or error. Changed counts rows that really
// changed since the previous step (first sync included: silent only means no
// notifications), so listeners know when the dashboard data is stale.
type Progress struct {
	State   string `json:"state"`
	Repo    string `json:"repo,omitempty"`
	Done    int    `json:"done"`
	Total   int    `json:"total"`
	Changed int    `json:"changed"`
	Unread  int    `json:"unread"`
	Error   string `json:"error,omitempty"`
}

// Syncer runs sync cycles on a timer and on demand.
type Syncer struct {
	opts    Options
	trigger chan struct{}
	cycle   sync.Mutex // one cycle at a time

	mu         sync.Mutex
	status     Status
	onProgress []func(Progress)
}

// OnProgress registers a listener for cycle progress (called on the sync goroutine).
func (s *Syncer) OnProgress(f func(Progress)) {
	s.mu.Lock()
	s.onProgress = append(s.onProgress, f)
	s.mu.Unlock()
}

func (s *Syncer) progress(p Progress) {
	s.mu.Lock()
	fs := append([]func(Progress){}, s.onProgress...)
	s.mu.Unlock()
	for _, f := range fs {
		f(p)
	}
}

// New creates a Syncer; call Run to start polling.
func New(opts Options) *Syncer {
	if opts.Interval <= 0 {
		opts.Interval = DefaultInterval
	}
	if opts.OnUpdate == nil {
		opts.OnUpdate = func([]store.Event, int) {}
	}
	return &Syncer{opts: opts, trigger: make(chan struct{}, 1), status: Status{Interval: opts.Interval.String()}}
}

// Run syncs now, then every Interval or on Trigger, until ctx ends.
func (s *Syncer) Run(ctx context.Context) {
	for {
		if err := s.SyncOnce(ctx); err != nil && ctx.Err() == nil {
			s.opts.Log.Warn("sync failed", "err", err)
		}
		t := time.NewTimer(s.interval())
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		case <-s.trigger:
			t.Stop()
		}
	}
}

func (s *Syncer) interval() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.opts.Interval
}

// SetInterval changes the poll period (settings, live); it applies after the
// pending wait.
func (s *Syncer) SetInterval(d time.Duration) {
	if d <= 0 {
		d = DefaultInterval
	}
	s.mu.Lock()
	s.opts.Interval = d
	s.status.Interval = d.String()
	s.mu.Unlock()
}

// Trigger requests a sync as soon as possible (coalesced).
func (s *Syncer) Trigger() {
	select {
	case s.trigger <- struct{}{}:
	default:
	}
}

// Status returns a snapshot.
func (s *Syncer) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status
}

func (s *Syncer) update(f func(*Status)) {
	s.mu.Lock()
	f(&s.status)
	s.mu.Unlock()
}

// SyncOnce runs one full cycle. Signed out is not an error: nothing to do.
func (s *Syncer) SyncOnce(ctx context.Context) error {
	s.cycle.Lock()
	defer s.cycle.Unlock()
	s.update(func(st *Status) { st.Running = true })
	var changed int
	events, err := s.cycleOnce(ctx, &changed)
	now := time.Now().UTC().Format(time.RFC3339)
	var rl *provider.RateLimitError
	s.update(func(st *Status) {
		st.Running = false
		st.SignedIn = !errors.Is(err, provider.ErrNotSignedIn)
		st.RateLimitedUntil = ""
		switch {
		case err == nil:
			st.LastSync, st.LastError = now, ""
		case errors.Is(err, provider.ErrNotSignedIn):
			st.LastError = ""
		case errors.As(err, &rl):
			st.RateLimitedUntil, st.LastError = rl.Reset.UTC().Format(time.RFC3339), err.Error()
		default:
			st.LastError = err.Error()
		}
	})
	if errors.Is(err, provider.ErrNotSignedIn) {
		return nil
	}
	unread, uerr := s.opts.Store.UnreadCount(ctx)
	if uerr == nil {
		s.opts.OnUpdate(events, unread)
	}
	p := Progress{State: ProgressDone, Unread: unread, Changed: changed}
	if err := errors.Join(err, uerr); err != nil {
		p.State, p.Error = ProgressError, err.Error()
	}
	s.progress(p)
	return errors.Join(err, uerr)
}

// cycleOnce syncs every project; *changed accumulates rows that really changed.
func (s *Syncer) cycleOnce(ctx context.Context, changed *int) ([]store.Event, error) {
	p := s.opts.Provider
	login, err := p.Account(ctx)
	if err != nil {
		return nil, err
	}
	src, err := s.opts.Store.UpsertSource(ctx, p.Platform(), login)
	if err != nil {
		return nil, err
	}
	list, err := p.ListProjects(ctx)
	if err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}
	c0 := s.opts.Store.Changes()
	projects, err := s.opts.Store.SyncProjects(ctx, src, list)
	if err != nil {
		return nil, err
	}
	step := func() int { // rows changed since the previous report
		c := s.opts.Store.Changes()
		n := int(c - c0)
		c0 = c
		*changed += n
		return n
	}
	s.progress(Progress{State: ProgressStarted, Total: len(projects), Changed: step()})
	var (
		events []store.Event
		errs   []error
	)
	for i, pr := range projects {
		items, err := p.SyncItems(ctx, provider.Project{ExternalID: pr.ExternalID, Name: pr.Name, URL: pr.URL}, pr.Cursor)
		if err != nil {
			var rl *provider.RateLimitError
			if errors.As(err, &rl) || errors.Is(err, provider.ErrNotSignedIn) || ctx.Err() != nil {
				return events, err // stop the cycle; the rest waits for the next one
			}
			errs = append(errs, fmt.Errorf("%s: %w", pr.ExternalID, err))
			s.progress(Progress{State: ProgressRepo, Repo: pr.Name, Done: i + 1, Total: len(projects)})
			continue
		}
		evs, err := s.opts.Store.ApplyItems(ctx, src, pr.ID, items, login)
		if err != nil {
			return events, err
		}
		events = append(events, evs...)
		s.progress(Progress{State: ProgressRepo, Repo: pr.Name, Done: i + 1, Total: len(projects), Changed: step()})
	}
	return events, errors.Join(errs...)
}

// ErrWrongPlatform means the item belongs to a platform this syncer does not serve.
var ErrWrongPlatform = errors.New("syncer: item belongs to another platform")

// Reply posts body as a comment on item id and stores the result.
func (s *Syncer) Reply(ctx context.Context, itemID int64, body string) (store.Comment, error) {
	ref, err := s.opts.Store.ItemRef(ctx, itemID)
	if err != nil {
		return store.Comment{}, err
	}
	if ref.Platform != s.opts.Provider.Platform() {
		return store.Comment{}, ErrWrongPlatform
	}
	c, err := s.opts.Provider.Reply(ctx, ref.ExternalID, body)
	if err != nil {
		return store.Comment{}, err
	}
	return s.opts.Store.AddComment(ctx, itemID, c)
}
