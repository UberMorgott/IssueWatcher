package syncer

import (
	"errors"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

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
