package syncer

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/provider"
	"github.com/UberMorgott/issuewatcher/internal/provider/github"
	"github.com/UberMorgott/issuewatcher/internal/provider/github/githubtest"
	"github.com/UberMorgott/issuewatcher/internal/provider/modkit"
	"github.com/UberMorgott/issuewatcher/internal/store"
)

func openStore(t *testing.T) *store.Store {
	t.Helper()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return store.New(db)
}

// A completed reconcile is persisted (sources.reconciled_at); a failed one
// leaves the stored time as it was.
func TestSyncOnceMarksReconciled(t *testing.T) {
	st := openStore(t)
	clk := &clock{t: t0}
	fp := &fakeProvider{platform: "nexus", account: "me", item: "x"}
	s := New(Options{Store: st, Provider: fp, Log: slog.New(slog.DiscardHandler), Now: clk.Now})
	if err := s.SyncOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	src, ok, err := st.LastSource(t.Context(), "nexus", "me")
	if err != nil || !ok || !src.ReconciledAt.Equal(t0) {
		t.Fatalf("after success: %+v %v %v", src, ok, err)
	}
	clk.Add(2 * time.Hour)
	fp.mu.Lock()
	fp.err = errors.New("boom")
	fp.mu.Unlock()
	if err := s.SyncOnce(t.Context()); err == nil {
		t.Fatal("want the failure")
	}
	if src, _, _ := st.LastSource(t.Context(), "nexus", "me"); !src.ReconciledAt.Equal(t0) {
		t.Fatalf("a failed reconcile moved reconciled_at: %+v", src)
	}
}

// counting wraps the GitHub provider and counts the full-read calls.
type counting struct {
	*github.Provider
	lists, syncs atomic.Int32
}

func (c *counting) ListProjects(ctx context.Context) ([]provider.Project, error) {
	c.lists.Add(1)
	return c.Provider.ListProjects(ctx)
}

func (c *counting) SyncItems(ctx context.Context, p provider.Project, since time.Time) ([]provider.Item, error) {
	c.syncs.Add(1)
	return c.Provider.SyncItems(ctx, p, since)
}

// restarted is a new process over the same store, provider and clock: a fresh
// Syncer (nothing in memory) with a counting provider and its progress states.
func restarted(s *Syncer) (*Syncer, *counting, func() []string) {
	opts := s.opts
	cp := &counting{Provider: s.opts.Provider.(*github.Provider)} //nolint:forcetypeassert // tiered builds a GitHub provider
	opts.Provider = cp
	n := New(opts)
	var (
		mu     sync.Mutex
		states []string
	)
	n.OnProgress(func(p Progress) { mu.Lock(); states = append(states, p.State); mu.Unlock() })
	return n, cp, func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), states...) }
}

// A restart with the schedule in SQLite reconciles nothing: no project list,
// no item reads, no progress; the first work is a conditional change check,
// spread over the first minute, answered 304.
func TestRestartResumesFromTheStore(t *testing.T) {
	s, gh, clk, updates := tiered(t, []string{"octo/a", "octo/b", "octo/c"}, plan5())
	s.Step(t.Context()) // first run: bootstrap reconcile
	clk.Add(6 * time.Minute)
	s.Step(t.Context()) // first checks store the ETags
	drain(updates)
	h0 := gh.Hits()

	clk.Add(6 * time.Minute) // restart with every check overdue
	r, cp, states := restarted(s)
	wait := r.Step(t.Context())
	if h := gh.Hits(); h != h0 || cp.lists.Load() != 0 || cp.syncs.Load() != 0 {
		t.Fatalf("work at t0 after a restart: hits %+v → %+v, lists %d, syncs %d", h0, h, cp.lists.Load(), cp.syncs.Load())
	}
	if wait > time.Minute {
		t.Fatalf("next step in %v, want the checks within the first minute", wait)
	}
	st := r.Status()
	if !st.SignedIn || st.Projects != 3 || st.LastSync == "" || st.Running {
		t.Fatalf("resumed status %+v", st)
	}
	if nr, _ := time.Parse(time.RFC3339, st.NextReconcile); nr.Before(clk.Now().Add(40 * time.Minute)) {
		t.Fatalf("next reconcile %v, want about an hour after the stored one", nr)
	}
	for range 7 { // restart+10s … +70s
		clk.Add(10 * time.Second)
		r.Step(t.Context())
	}
	h := gh.Hits()
	if cp.lists.Load() != 0 || cp.syncs.Load() != 0 || h.GraphQL != h0.GraphQL {
		t.Fatalf("full reads after a restart: lists %d syncs %d graphql %d → %d", cp.lists.Load(), cp.syncs.Load(), h0.GraphQL, h.GraphQL)
	}
	if h.REST-h0.REST != 6 || h.NotModified-h0.NotModified != 6 {
		t.Fatalf("checks after a restart: %+v → %+v, want 6 conditional requests, all 304", h0, h)
	}
	if got := states(); len(got) != 0 {
		t.Fatalf("progress after a restart without changes: %v", got)
	}
}

// A restart after the reconcile period reconciles once, a few minutes in, not
// at t0.
func TestRestartOverdueReconcileIsSpread(t *testing.T) {
	s, gh, clk, updates := tiered(t, []string{"octo/a"}, plan5())
	s.Step(t.Context())
	drain(updates)
	clk.Add(3 * time.Hour)
	r, cp, states := restarted(s)
	r.Step(t.Context())
	if cp.lists.Load() != 0 {
		t.Fatal("overdue reconcile ran at t0")
	}
	nr, _ := time.Parse(time.RFC3339, r.Status().NextReconcile)
	if d := nr.Sub(clk.Now()); d < 2*time.Minute || d > 5*time.Minute {
		t.Fatalf("overdue reconcile in %v, want 2–5 min", d)
	}
	gql := gh.Hits().GraphQL
	for range 6 {
		clk.Add(time.Minute)
		r.Step(t.Context())
	}
	if cp.lists.Load() != 1 || gh.Hits().GraphQL == gql {
		t.Fatalf("reconciles after the spread: %d", cp.lists.Load())
	}
	if got := states(); len(got) == 0 || got[0] != ProgressStarted {
		t.Fatalf("progress %v", got)
	}
}

// An empty store (first run) bootstraps with a full reconcile at once.
func TestEmptyStoreBootstraps(t *testing.T) {
	s, _, _, _ := tiered(t, []string{"octo/a"}, plan5())
	r, cp, _ := restarted(s)
	r.Step(t.Context())
	if cp.lists.Load() != 1 || cp.syncs.Load() != 1 {
		t.Fatalf("bootstrap: lists %d syncs %d", cp.lists.Load(), cp.syncs.Load())
	}
}

// A change check whose findings cannot be stored moves nothing: poll state,
// checked_at and cursor stay, and the next check fetches and stores the same
// change.
func TestFailedStoreKeepsPollState(t *testing.T) {
	s, gh, clk, updates, db := tieredDB(t, []string{"octo/a"}, plan5())
	s.Step(t.Context())
	clk.Add(6 * time.Minute)
	s.Step(t.Context()) // ETags stored
	drain(updates)
	var poll0, checked0, cursor0 string
	row := `SELECT poll_state, checked_at, sync_cursor FROM projects WHERE external_id = 'octo/a'`
	if err := db.QueryRowContext(t.Context(), row).Scan(&poll0, &checked0, &cursor0); err != nil {
		t.Fatal(err)
	}

	gh.Mu.Lock()
	is := gh.Issues[0]
	is.Comments = append(is.Comments, githubtest.Comment{ID: "C_9", Author: "bob", Body: "hi", CreatedAt: clk.Now()})
	is.UpdatedAt = clk.Now()
	gh.Mu.Unlock()
	if _, err := db.ExecContext(t.Context(), `CREATE TRIGGER fail_once BEFORE INSERT ON comments BEGIN SELECT RAISE(ABORT, 'boom'); END`); err != nil {
		t.Fatal(err)
	}
	clk.Add(6 * time.Minute)
	s.Step(t.Context())
	var poll1, checked1, cursor1 string
	if err := db.QueryRowContext(t.Context(), row).Scan(&poll1, &checked1, &cursor1); err != nil {
		t.Fatal(err)
	}
	if poll1 != poll0 || checked1 != checked0 || cursor1 != cursor0 {
		t.Fatalf("failed store moved the state: poll %q→%q checked %q→%q cursor %q→%q", poll0, poll1, checked0, checked1, cursor0, cursor1)
	}
	if evs := drain(updates); len(evs) != 0 {
		t.Fatalf("events from a failed store: %+v", evs)
	}

	if _, err := db.ExecContext(t.Context(), `DROP TRIGGER fail_once`); err != nil {
		t.Fatal(err)
	}
	gql := gh.Hits().GraphQL
	clk.Add(6 * time.Minute)
	s.Step(t.Context())
	evs := drain(updates)
	if len(evs) != 1 || evs[0].Kind != store.EventNewComment || gh.Hits().GraphQL == gql {
		t.Fatalf("change not re-detected after the failure: %+v", evs)
	}
	var n int
	if err := db.QueryRowContext(t.Context(), `SELECT count(*) FROM comments WHERE external_id = 'C_9'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("comment stored %d %v", n, err)
	}
}

// A failed first reconcile (nothing to check yet) is retried at 1, 2 … minutes,
// not after the hourly reconcile period.
func TestFailedBootstrapRetriesWithBackoff(t *testing.T) {
	st := openStore(t)
	clk := &clock{t: t0}
	fp := &fakeProvider{platform: "nexus", account: "me", item: "x", err: errors.New("server down")}
	s := New(Options{Store: st, Provider: fp, Log: slog.New(slog.DiscardHandler), Now: clk.Now, Plan: plan5()})
	next := func() time.Duration {
		nr, _ := time.Parse(time.RFC3339, s.Status().NextReconcile)
		return nr.Sub(clk.Now())
	}
	s.Step(t.Context())
	if d := next(); d != time.Minute {
		t.Fatalf("first retry in %v, want 1m", d)
	}
	if w := s.Step(t.Context()); w > time.Minute {
		t.Fatalf("wait %v", w)
	}
	clk.Add(time.Minute)
	s.Step(t.Context())
	if d := next(); d != 2*time.Minute {
		t.Fatalf("second retry in %v, want 2m", d)
	}
	fp.mu.Lock()
	fp.err = nil
	fp.mu.Unlock()
	clk.Add(2 * time.Minute)
	s.Step(t.Context())
	if st := s.Status(); st.LastSync == "" || st.LastError != "" {
		t.Fatalf("no reconcile at +3m: %+v", st)
	}
	if d := next(); d < 50*time.Minute {
		t.Fatalf("after success next reconcile in %v, want the hourly period", d)
	}
}

// slowProvider serves n projects, each read taking delay; failAt fails that
// project's read with a rate limit. With gate > 0 the reads hold until gate
// of them are in flight at once (then the gate stays open), so overlap is
// proven by counting, not by wall-clock time; maxInFlight records the peak.
type slowProvider struct {
	fakeProvider
	n      int
	delay  time.Duration
	failAt string
	reads  atomic.Int32

	gate        int32
	gateOnce    sync.Once
	gateOpen    chan struct{} // closed once gate reads were in flight together
	inFlight    atomic.Int32
	maxInFlight atomic.Int32
}

// waitGate counts the read in and holds it until the gate opens (or ctx ends).
func (f *slowProvider) waitGate(ctx context.Context) error {
	n := f.inFlight.Add(1)
	for {
		peak := f.maxInFlight.Load()
		if n <= peak || f.maxInFlight.CompareAndSwap(peak, n) {
			break
		}
	}
	if f.gate <= 0 {
		return nil
	}
	if n >= f.gate {
		f.gateOnce.Do(func() { close(f.gateOpen) })
	}
	select {
	case <-f.gateOpen:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (f *slowProvider) ListProjects(context.Context) ([]provider.Project, error) {
	var out []provider.Project
	for i := range f.n {
		id := "p" + strconv.Itoa(i)
		out = append(out, provider.Project{ExternalID: id, Name: id})
	}
	return out, nil
}

func (f *slowProvider) SyncItems(ctx context.Context, p provider.Project, _ time.Time) ([]provider.Item, error) {
	f.reads.Add(1)
	if p.ExternalID == f.failAt {
		return nil, &provider.RateLimitError{Reset: t0.Add(time.Hour)}
	}
	defer f.inFlight.Add(-1)
	if err := f.waitGate(ctx); err != nil {
		return nil, err
	}
	select {
	case <-time.After(f.delay):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return []provider.Item{{ExternalID: "x-" + p.ExternalID, Kind: "issue", Number: 1, Title: "t", Author: "u", Open: true, CreatedAt: t0, UpdatedAt: t0}}, nil
}

// A reconcile reads Plan.Concurrency projects at a time; progress counts up
// in completion order. Parallelism is proven by the provider: its first reads
// hold until 4 are in flight together (a sequential reconcile never gets
// there and the safety deadline fails the test), and no more than 4 ever are.
// No wall-clock bound: a loaded machine (race detector, parallel packages)
// slows every read alike without making a parallel reconcile sequential.
func TestReconcileIsParallel(t *testing.T) {
	st := openStore(t)
	fp := &slowProvider{platform: "nexus", account: "me", n: 20, delay: 5 * time.Millisecond, gate: 4, gateOpen: make(chan struct{})}
	s := New(Options{Store: st, Provider: fp, Log: slog.New(slog.DiscardHandler), Plan: plan5()}) // Concurrency 2
	p := plan5()
	p.Concurrency = 4
	s.SetPlan(p)
	var (
		mu    sync.Mutex
		dones []int
	)
	s.OnProgress(func(p Progress) {
		if p.State == ProgressRepo {
			mu.Lock()
			dones = append(dones, p.Done)
			mu.Unlock()
		}
	})
	// Safety deadline only (a sequential reconcile would wait at the gate forever).
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	if err := s.SyncOnce(ctx); err != nil {
		t.Fatalf("%v (peak reads in flight %d, want 4)", err, fp.maxInFlight.Load())
	}
	if peak := fp.maxInFlight.Load(); peak != 4 {
		t.Fatalf("peak reads in flight %d, want Concurrency 4", peak)
	}
	if len(dones) != 20 || dones[0] != 1 || dones[19] != 20 {
		t.Fatalf("progress %v", dones)
	}
	if n, _ := st.UnreadCount(t.Context()); n != 0 {
		t.Fatalf("baseline unread %d", n)
	}
}

// A rate limit stops the parallel reconcile: it reports the limit, reads no
// further project, and the cycle is not stamped as complete.
func TestReconcileStopsOnRateLimit(t *testing.T) {
	st := openStore(t)
	fp := &slowProvider{platform: "nexus", account: "me", n: 20, delay: 20 * time.Millisecond, failAt: "p3"}
	s := New(Options{Store: st, Provider: fp, Log: slog.New(slog.DiscardHandler), Plan: plan5()})
	err := s.SyncOnce(t.Context())
	if _, ok := errors.AsType[*provider.RateLimitError](err); !ok {
		t.Fatalf("err %v, want the rate limit", err)
	}
	if n := fp.reads.Load(); n >= 20 {
		t.Fatalf("read %d projects after the limit", n)
	}
	if src, _, _ := st.LastSource(t.Context(), "nexus", "me"); !src.ReconciledAt.IsZero() {
		t.Fatalf("a stopped reconcile was stamped complete: %+v", src)
	}
}

// pageFake is a mod-platform poller: a page-1 fingerprint (sig) checked with
// modkit.PageChanged, full reads counted.
type pageFake struct {
	fakeProvider
	sig          string
	checks, full atomic.Int32
	now          func() time.Time
}

func (f *pageFake) Scheduling() provider.Scheduling { return provider.Scheduling{} }

func (f *pageFake) DetectChanges(_ context.Context, _ provider.Project, st *provider.PollState) (provider.Changes, error) {
	f.checks.Add(1)
	return provider.Changes{Requests: 1, Overflow: modkit.PageChanged(st, "fake:page1", f.sig, f.now(), time.Hour)}, nil
}

func (f *pageFake) FetchChanged(ctx context.Context, p provider.Project, _ []int) ([]provider.Item, error) {
	return f.SyncItems(ctx, p, time.Time{})
}

func (f *pageFake) FullReconcile(ctx context.Context, p provider.Project, _ time.Time) ([]provider.Item, error) {
	f.full.Add(1)
	return f.SyncItems(ctx, p, time.Time{})
}

// A reconcile stamps the change-check baseline: the next checks read page 1
// only (the fingerprint taken after the reconcile is adopted), and FullEvery
// still forces one full read an hour after it.
func TestReconcileStampsPageBaseline(t *testing.T) {
	st := openStore(t)
	clk := &clock{t: t0}
	fp := &pageFake{platform: "nexus", account: "me", item: "x", sig: "a", now: clk.Now}
	plan := plan5()
	plan.Reconcile = 3 * time.Hour // FullEvery (1 h), not the scheduled reconcile, must force the full read
	s := New(Options{Store: st, Provider: fp, Log: slog.New(slog.DiscardHandler), Now: clk.Now, Plan: plan})
	s.Step(t.Context()) // bootstrap reconcile
	for range 3 {       // checks at +6, +12, +18 min
		clk.Add(6 * time.Minute)
		s.Step(t.Context())
	}
	if c, f := fp.checks.Load(), fp.full.Load(); c != 3 || f != 0 {
		t.Fatalf("after a reconcile: %d checks, %d full reads; want 3 page-1 checks and no full read", c, f)
	}
	// A restart resumes the baseline from SQLite too.
	r := New(Options{Store: st, Provider: fp, Log: slog.New(slog.DiscardHandler), Now: clk.Now, Plan: plan})
	clk.Add(6 * time.Minute)
	for range 3 {
		r.Step(t.Context())
		clk.Add(20 * time.Second)
	}
	if f := fp.full.Load(); f != 0 {
		t.Fatalf("full reads after a restart: %d", f)
	}
	fp.sig = "b" // page 1 changed
	clk.Add(6 * time.Minute)
	r.Step(t.Context())
	if f := fp.full.Load(); f != 1 {
		t.Fatalf("a changed page 1: %d full reads, want 1", f)
	}
	for range 12 { // 72 min more: over an hour since that full read
		clk.Add(6 * time.Minute)
		r.Step(t.Context())
	}
	if f := fp.full.Load(); f < 2 {
		t.Fatalf("FullEvery did not force a full read: %d", f)
	}
}
