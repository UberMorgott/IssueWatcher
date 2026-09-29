// Package syncer keeps the store in step with the providers and reports
// changes (new issue, new comment, issue closed) plus the unread count.
//
// Tiered sync: a full reconcile (list projects, re-read every project's items
// since its cursor) runs on start, on demand and every Plan.Reconcile; between
// reconciles every project gets a cheap change check (provider.Poller) every
// Plan.Active (recently active projects) or Plan.Idle, with jitter, bounded
// concurrency and an hourly request budget. Rate limits pause the schedule.
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

// Options configures a Syncer.
type Options struct {
	Store    *store.Store
	Provider provider.Provider
	Plan     Plan
	// OnUpdate runs after every reconcile and every change check that found
	// something, with the detected events (possibly none) and the unread count.
	OnUpdate func(events []store.Event, unread int)
	Log      *slog.Logger
	Now      func() time.Time // default time.Now (tests use a fake clock)
}

// Status is reported by GET /api/sync.
type Status struct {
	Running          bool                 `json:"running"`
	SignedIn         bool                 `json:"signedIn"`
	LastSync         string               `json:"lastSync"` // last full reconcile
	LastCheck        string               `json:"lastCheck"`
	LastError        string               `json:"lastError"`
	RateLimitedUntil string               `json:"rateLimitedUntil"`
	Interval         string               `json:"interval"` // active-project check period (legacy field)
	Mode             string               `json:"mode"`
	ActiveEvery      string               `json:"activeEvery"`
	IdleEvery        string               `json:"idleEvery"`
	ReconcileEvery   string               `json:"reconcileEvery"`
	NextReconcile    string               `json:"nextReconcile"`
	Projects         int                  `json:"projects"`
	ActiveProjects   int                  `json:"activeProjects"`
	Budget           int                  `json:"budget"`      // requests per hour
	BudgetUsed       int                  `json:"budgetUsed"`  // in the last hour
	Checks           int                  `json:"checks"`      // change-check requests since start
	NotModified      int                  `json:"notModified"` // of them answered 304 (free)
	Rate             *provider.RateStatus `json:"rate,omitempty"`
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
	Source  string `json:"source,omitempty"` // platform[:account] of the syncer that reports
	Repo    string `json:"repo,omitempty"`
	Done    int    `json:"done"`
	Total   int    `json:"total"`
	Changed int    `json:"changed"`
	Unread  int    `json:"unread"`
	Error   string `json:"error,omitempty"`
}

// Syncer runs the schedule.
type Syncer struct {
	opts    Options
	trigger chan struct{}
	wake    chan struct{}
	cycle   sync.Mutex // one reconcile at a time
	spend   budget
	minPoll time.Duration

	mu             sync.Mutex
	status         Status
	onProgress     []func(Progress)
	plan_          Plan
	targets        map[int64]*target
	source         int64
	login          string
	nextReconcile  time.Time
	reconciledOnce bool
	forceReconcile bool
	pausedUntil    time.Time
	backoff        time.Duration
	budgetFree     time.Time
	checks         int
	notModified    int
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
	p.Source = s.labelLocked()
	s.mu.Unlock()
	for _, f := range fs {
		f(p)
	}
}

// New creates a Syncer; call Run to start the schedule.
func New(opts Options) *Syncer {
	if opts.OnUpdate == nil {
		opts.OnUpdate = func([]store.Event, int) {}
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Log == nil {
		opts.Log = slog.New(slog.DiscardHandler)
	}
	s := &Syncer{
		opts: opts, trigger: make(chan struct{}, 1), wake: make(chan struct{}, 1),
		targets: map[int64]*target{},
	}
	if pl, ok := opts.Provider.(provider.Poller); ok {
		s.minPoll = pl.Scheduling().PollMinInterval
	}
	s.SetPlan(opts.Plan)
	return s
}

func (s *Syncer) now() time.Time { return s.opts.Now() }

func (s *Syncer) plan() Plan {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.plan_
}

// SetPlan applies new polling settings (live, from settings.changed): the next
// reconcile and every project's next check are re-derived from it.
func (s *Syncer) SetPlan(p Plan) {
	p = p.normalized()
	now := s.now()
	s.mu.Lock()
	old := s.plan_
	s.plan_ = p
	s.status.Interval, s.status.Mode = p.Active.String(), p.Mode
	s.status.ActiveEvery, s.status.IdleEvery, s.status.ReconcileEvery = p.Active.String(), p.Idle.String(), p.Reconcile.String()
	s.status.Budget = p.Budget
	if s.reconciledOnce && old.Reconcile != p.Reconcile {
		s.nextReconcile = now.Add(jitter(p.Reconcile, 0, now.Unix()/60))
	}
	for _, t := range s.targets { // shorter intervals take effect now, longer ones after the pending check
		d := p.Idle
		if !t.activity.IsZero() && now.Sub(t.activity) <= p.ActiveWindow {
			d = p.Active
		}
		if t.next.After(now.Add(d)) {
			t.next = now.Add(jitter(d, t.ID, now.Unix()/60))
		}
	}
	s.budgetFree = time.Time{}
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Run follows the schedule until ctx ends.
func (s *Syncer) Run(ctx context.Context) {
	for {
		wait := s.Step(ctx)
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		case <-s.trigger:
			t.Stop()
			s.mu.Lock()
			s.forceReconcile = true
			s.mu.Unlock()
		case <-s.wake:
			t.Stop()
		}
	}
}

// Trigger requests a full sync as soon as possible (coalesced).
func (s *Syncer) Trigger() {
	select {
	case s.trigger <- struct{}{}:
	default:
	}
}

// Status returns a snapshot.
func (s *Syncer) Status() Status {
	now := s.now()
	used := s.spend.used(now)
	s.mu.Lock()
	st := s.status
	st.BudgetUsed, st.Checks, st.NotModified = used, s.checks, s.notModified
	st.Projects, st.ActiveProjects = len(s.targets), 0
	for _, t := range s.targets {
		if !t.activity.IsZero() && now.Sub(t.activity) <= s.plan_.ActiveWindow {
			st.ActiveProjects++
		}
	}
	if !s.nextReconcile.IsZero() {
		st.NextReconcile = s.nextReconcile.UTC().Format(time.RFC3339)
	}
	if s.pausedUntil.After(now) {
		st.RateLimitedUntil = s.pausedUntil.UTC().Format(time.RFC3339)
	}
	s.mu.Unlock()
	if rr, ok := s.opts.Provider.(provider.RateReporter); ok {
		if r, ok := rr.RateStatus(); ok {
			st.Rate = &r
		}
	}
	return st
}

func (s *Syncer) update(f func(*Status)) {
	s.mu.Lock()
	f(&s.status)
	s.mu.Unlock()
}

// SyncOnce runs one full reconcile. Signed out is not an error: nothing to do.
func (s *Syncer) SyncOnce(ctx context.Context) error {
	s.cycle.Lock()
	defer s.cycle.Unlock()
	s.update(func(st *Status) { st.Running = true })
	var changed int
	events, err := s.cycleOnce(ctx, &changed)
	now := s.now()
	var rl *provider.RateLimitError
	s.update(func(st *Status) {
		st.Running = false
		st.SignedIn = !errors.Is(err, provider.ErrNotSignedIn)
		st.RateLimitedUntil = ""
		switch {
		case err == nil:
			st.LastSync, st.LastError = now.UTC().Format(time.RFC3339), ""
		case errors.Is(err, provider.ErrNotSignedIn):
			st.LastError = ""
		case errors.As(err, &rl):
			st.RateLimitedUntil, st.LastError = rl.Reset.UTC().Format(time.RFC3339), err.Error()
		default:
			st.LastError = err.Error()
		}
	})
	s.mu.Lock()
	s.reconciledOnce = true
	s.nextReconcile = now.Add(jitter(s.plan_.Reconcile, 0, now.Unix()/60))
	if errors.Is(err, provider.ErrNotSignedIn) {
		// Signed out: look again soon (sign-in also triggers a sync).
		s.nextReconcile = now.Add(s.plan_.Active)
	}
	s.mu.Unlock()
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
	s.mu.Lock()
	s.source, s.login = src, login
	s.mu.Unlock()
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
				s.refreshTargets(ctx, src, login)
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
	s.refreshTargets(ctx, src, login)
	return events, errors.Join(errs...)
}

// refreshTargets reloads the change-check targets after a reconcile: active
// projects with their cursor, activity and persisted poll state. A project's
// next check keeps its slot when it already had one.
func (s *Syncer) refreshTargets(ctx context.Context, src int64, login string) {
	rows, err := s.opts.Store.PollTargets(ctx, src)
	if err != nil {
		s.opts.Log.Error("sync: load poll targets", "err", err)
		return
	}
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.source, s.login = src, login
	next := map[int64]*target{}
	for _, r := range rows {
		t, ok := s.targets[r.ID]
		if !ok {
			t = &target{Project: r.Project, poll: r.Poll}
		}
		t.Project, t.activity = r.Project, r.Activity
		if !ok || t.next.IsZero() {
			// First slot: spread the projects over the interval instead of a burst.
			d := s.plan_.Idle
			if !t.activity.IsZero() && now.Sub(t.activity) <= s.plan_.ActiveWindow {
				d = s.plan_.Active
			}
			t.next = now.Add(jitter(d, t.ID, 0))
		}
		next[r.ID] = t
	}
	s.targets = next
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
	return s.reply(ctx, ref, body)
}

// ErrReplyOff means the item's platform does not offer replies (yet): every
// path (API, rule jobs, JobView send) ends here, not only the hidden UI button.
var ErrReplyOff = errors.New("syncer: replies are off for this platform")

func (s *Syncer) reply(ctx context.Context, ref store.ItemRef, body string) (store.Comment, error) {
	if !s.opts.Provider.Capabilities().Reply {
		return store.Comment{}, ErrReplyOff
	}
	itemID := ref.ID
	c, err := s.opts.Provider.Reply(ctx, ref.ExternalID, body)
	if err != nil {
		return store.Comment{}, err
	}
	return s.opts.Store.AddComment(ctx, itemID, c)
}
