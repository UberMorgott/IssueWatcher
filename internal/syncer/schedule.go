package syncer

import (
	"context"
	"encoding/binary"
	"errors"
	"hash/fnv"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/provider"
	"github.com/UberMorgott/issuewatcher/internal/store"
)

// Plan is the polling plan (settings → Sync; config.ProviderSync).
type Plan struct {
	Mode         string        // balanced | fast | custom (status only)
	Active       time.Duration // change check, projects active within ActiveWindow
	Idle         time.Duration // change check, the rest
	Reconcile    time.Duration // full re-read (list projects + every project's items)
	ActiveWindow time.Duration // "active" = an item changed within this window
	Budget       int           // requests per hour for change checks and fetches
	Concurrency  int           // projects checked at once
}

// DefaultPlan is the balanced mode.
func DefaultPlan() Plan {
	return Plan{Mode: "balanced", Active: 5 * time.Minute, Idle: 30 * time.Minute, Reconcile: time.Hour,
		ActiveWindow: 14 * 24 * time.Hour, Budget: 1500, Concurrency: 4}
}

func (p Plan) normalized() Plan {
	d := DefaultPlan()
	if p.Active <= 0 {
		p.Active = d.Active
	}
	if p.Idle < p.Active {
		p.Idle = max(p.Active, d.Idle)
	}
	if p.Reconcile <= 0 {
		p.Reconcile = d.Reconcile
	}
	if p.ActiveWindow <= 0 {
		p.ActiveWindow = d.ActiveWindow
	}
	if p.Budget <= 0 {
		p.Budget = d.Budget
	}
	if p.Concurrency <= 0 {
		p.Concurrency = d.Concurrency
	}
	return p
}

// budget is a sliding one-hour window of spent requests.
type budget struct {
	mu    sync.Mutex
	spent []time.Time
}

func (b *budget) prune(now time.Time) {
	i := 0
	for i < len(b.spent) && now.Sub(b.spent[i]) >= time.Hour {
		i++
	}
	b.spent = b.spent[i:]
}

// used is the number of requests in the last hour.
func (b *budget) used(now time.Time) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.prune(now)
	return len(b.spent)
}

// reserve takes n requests if they fit into limit; else it returns when the
// next ones free up.
func (b *budget) reserve(now time.Time, n, limit int) (bool, time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.prune(now)
	if len(b.spent)+n <= limit {
		for range n {
			b.spent = append(b.spent, now)
		}
		return true, time.Time{}
	}
	k := len(b.spent) + n - limit // this many must expire first
	return false, b.spent[min(k, len(b.spent))-1].Add(time.Hour)
}

// refund returns requests reserved but not spent.
func (b *budget) refund(n int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.spent = b.spent[:max(0, len(b.spent)-n)]
}

// target is one project on the change-check schedule.
type target struct {
	store.Project
	next     time.Time
	activity time.Time // newest item update
	poll     provider.PollState
	minEvery time.Duration // platform's X-Poll-Interval
}

// jitter spreads checks: ±10 %, stable per project and round.
func jitter(d time.Duration, key int64, round int64) time.Duration {
	h := fnv.New32a()
	var b [16]byte
	binary.LittleEndian.PutUint64(b[:8], uint64(key))   //nolint:gosec // G115: hashing the bits, sign irrelevant
	binary.LittleEndian.PutUint64(b[8:], uint64(round)) //nolint:gosec // G115: hashing the bits, sign irrelevant
	_, _ = h.Write(b[:])
	f := 0.9 + 0.2*float64(h.Sum32()%1000)/1000
	return time.Duration(float64(d) * f)
}

func (s *Syncer) interval(t *target, now time.Time) time.Duration {
	p := s.plan()
	d := p.Idle
	if !t.activity.IsZero() && now.Sub(t.activity) <= p.ActiveWindow {
		d = p.Active
	}
	return max(jitter(d, t.ID, now.Unix()/60), t.minEvery, s.minPoll)
}

// Step runs whatever is due at now (a full reconcile, or change checks of the
// due projects within the budget) and returns how long to wait for the next
// due work. Run calls it in a loop; tests call it with a fake clock.
func (s *Syncer) Step(ctx context.Context) time.Duration {
	now := s.now()
	s.mu.Lock()
	warm := !s.warmed
	s.warmed = true
	s.mu.Unlock()
	if warm {
		s.warmStart(ctx, now)
	}
	s.mu.Lock()
	paused, force, user := s.pausedUntil, s.forceReconcile, s.userForce
	s.mu.Unlock()
	if now.Before(paused) && !force {
		return paused.Sub(now)
	}
	if force || s.targetsEmpty() || !now.Before(s.nextReconcileAt()) {
		s.mu.Lock()
		s.forceReconcile, s.userForce = false, false
		s.mu.Unlock()
		err := s.syncOnce(ctx, !force || !user)
		if err != nil && ctx.Err() == nil {
			s.opts.Log.Warn("sync failed", "err", err)
		}
		s.afterError(err, now)
		return s.untilNext(s.now())
	}
	if _, ok := s.opts.Provider.(provider.Poller); ok && s.accountConfirmed(ctx, now) {
		s.checkDue(ctx, now)
	}
	return s.untilNext(s.now())
}

func (s *Syncer) afterError(err error, now time.Time) {
	var rl *provider.RateLimitError
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case errors.As(err, &rl):
		until := rl.Reset
		if rl.Secondary || until.IsZero() || !until.After(now) {
			// Secondary limit: back off exponentially from a minute (up to 15).
			s.backoff = min(max(2*s.backoff, time.Minute), 15*time.Minute)
			if b := now.Add(s.backoff); b.After(until) {
				until = b
			}
		}
		s.pausedUntil = until
		s.opts.Log.Warn("sync: rate limited, pausing", "until", until.UTC().Format(time.RFC3339), "secondary", rl.Secondary)
	case err == nil:
		s.backoff = 0
	}
}

func (s *Syncer) targetsEmpty() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.targets) == 0 && !s.reconciledOnce
}

func (s *Syncer) nextReconcileAt() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.nextReconcile
}

func (s *Syncer) untilNext(now time.Time) time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.nextReconcile
	if s.pausedUntil.After(next) {
		return s.pausedUntil.Sub(now)
	}
	if _, ok := s.opts.Provider.(provider.Poller); ok {
		for _, t := range s.targets {
			if t.next.Before(next) {
				next = t.next
			}
		}
		if s.budgetFree.After(now) && s.budgetFree.After(next) {
			next = s.budgetFree
		}
	}
	return max(next.Sub(now), time.Second)
}

// result of one project's change check.
type checkResult struct {
	t     *target
	prev  provider.PollState // the poll state before the check (restored when storing fails)
	items []provider.Item
	ch    provider.Changes
	err   error
	cost  int
}

// checkDue runs the change checks that are due, at most Concurrency at a time,
// and applies what they found.
func (s *Syncer) checkDue(ctx context.Context, now time.Time) {
	pl := s.opts.Provider.(provider.Poller) //nolint:forcetypeassert // checked by the caller
	plan := s.plan()
	s.mu.Lock()
	var due []*target
	for _, t := range s.targets {
		if !t.next.After(now) {
			due = append(due, t)
		}
	}
	src, login := s.source, s.login
	s.mu.Unlock()
	if len(due) == 0 {
		return
	}
	slices.SortFunc(due, func(a, b *target) int { return a.next.Compare(b.next) })

	const perCheck = 2 // issues + comments
	var batch []*target
	for _, t := range due {
		ok, free := s.spend.reserve(now, perCheck, plan.Budget)
		if !ok {
			s.mu.Lock()
			s.budgetFree = free
			s.mu.Unlock()
			s.opts.Log.Info("sync: hourly request budget used up; checks deferred", "budget", plan.Budget, "until", free.UTC().Format(time.RFC3339))
			break
		}
		batch = append(batch, t)
	}
	if len(batch) == 0 {
		return
	}
	results := make(chan checkResult, len(batch))
	sem := make(chan struct{}, plan.Concurrency)
	var wg sync.WaitGroup
	for _, t := range batch {
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			results <- s.checkOne(ctx, pl, t)
		})
	}
	wg.Wait()
	close(results)

	var (
		events  []store.Event
		changed int
		stop    error
	)
	c0 := s.opts.Store.Changes()
	for r := range results {
		t := r.t
		if extra := r.cost - perCheck; extra > 0 {
			_, _ = s.spend.reserve(now, extra, 1<<30) // fetches: recorded, never refused mid-flight
		} else if extra < 0 {
			s.spend.refund(-extra)
		}
		s.noteChecks(r.ch.Requests, r.ch.NotModified)
		if r.err != nil {
			var rl *provider.RateLimitError
			if errors.As(r.err, &rl) || errors.Is(r.err, provider.ErrNotSignedIn) {
				stop = r.err
			} else {
				s.opts.Log.Warn("sync: change check failed", "project", t.ExternalID, "err", r.err)
			}
			s.reschedule(t, now)
			continue
		}
		if r.ch.MinInterval > 0 {
			t.minEvery = r.ch.MinInterval
		}
		if len(r.items) == 0 {
			if err := s.opts.Store.SavePollState(ctx, t.ID, t.poll, now); err != nil {
				s.opts.Log.Error("sync: save poll state", "project", t.ExternalID, "err", err)
			}
			s.reschedule(t, now)
			continue
		}
		// Items, cursor and poll state in one transaction: when storing fails
		// nothing moves (cursor, since marks, ETags), so the next check sees
		// the same changes again instead of losing them.
		evs, err := s.opts.Store.ApplyChecked(ctx, src, t.ID, r.items, login, t.poll, now)
		if err != nil {
			s.opts.Log.Error("sync: store changes", "project", t.ExternalID, "err", err)
			t.poll = r.prev
			s.reschedule(t, now)
			continue
		}
		events = append(events, evs...)
		for _, it := range r.items {
			if it.UpdatedAt.After(t.activity) {
				t.activity = it.UpdatedAt
			}
			if it.UpdatedAt.After(t.Cursor) {
				t.Cursor = it.UpdatedAt
			}
		}
		s.reschedule(t, now)
	}
	s.mu.Lock()
	s.status.LastCheck = now.UTC().Format(time.RFC3339)
	s.mu.Unlock()
	if stop != nil {
		s.afterError(stop, now)
		s.update(func(st *Status) { st.LastError = stop.Error() })
	}
	changed = int(s.opts.Store.Changes() - c0)
	if changed == 0 && len(events) == 0 {
		return
	}
	unread, err := s.opts.Store.UnreadCount(ctx)
	if err != nil {
		return
	}
	s.opts.OnUpdate(events, unread)
	s.progress(Progress{State: ProgressRepo, Changed: changed, Unread: unread, Done: len(batch), Total: len(batch), Background: true})
	s.progress(Progress{State: ProgressDone, Changed: changed, Unread: unread, Background: true})
}

func (s *Syncer) reschedule(t *target, now time.Time) {
	d := s.interval(t, now)
	s.mu.Lock()
	t.next = now.Add(d)
	s.mu.Unlock()
}

// checkOne: cheap check, then a targeted fetch (or a project reconcile when
// the check overflowed).
func (s *Syncer) checkOne(ctx context.Context, pl provider.Poller, t *target) checkResult {
	p := provider.Project{ExternalID: t.ExternalID, Name: t.Name, URL: t.URL}
	if t.poll.IssuesSince.IsZero() {
		t.poll.IssuesSince = t.Cursor
	}
	if t.poll.CommentsSince.IsZero() {
		t.poll.CommentsSince = t.Cursor
	}
	prev := t.poll // a failed check leaves the poll state as it was: the changes are re-detected next time
	prev.ETags = maps.Clone(t.poll.ETags)
	ch, err := pl.DetectChanges(ctx, p, &t.poll)
	r := checkResult{t: t, prev: prev, ch: ch, err: err, cost: ch.Requests}
	switch {
	case err != nil:
	case ch.Overflow:
		r.items, r.err = pl.FullReconcile(ctx, p, t.Cursor)
		r.cost += 1 + len(r.items)/50
	case len(ch.Numbers) > 0:
		r.items, r.err = pl.FetchChanged(ctx, p, ch.Numbers)
		r.cost += (len(ch.Numbers) + 19) / 20
		if sk, ok := errors.AsType[*provider.SkippedError](r.err); ok {
			for n, e := range sk.Items { // not retried: they would fail the same way
				s.opts.Log.Warn("sync: item skipped", "project", t.ExternalID, "number", n, "err", e)
			}
			r.err = nil
		}
	}
	if r.err != nil {
		t.poll = prev
	}
	return r
}

func (s *Syncer) noteChecks(requests, notModified int) {
	s.mu.Lock()
	s.checks += requests
	s.notModified += notModified
	s.mu.Unlock()
}
