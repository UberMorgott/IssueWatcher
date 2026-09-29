package syncer

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/provider"
	"github.com/UberMorgott/issuewatcher/internal/provider/github"
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

