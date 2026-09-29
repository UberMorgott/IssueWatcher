package store

import (
	"strconv"
	"testing"

	"github.com/UberMorgott/issuewatcher/internal/provider"
)

// noopBatch is 500 items with 3 comments each.
func noopBatch() []provider.Item {
	items := make([]provider.Item, 500)
	for i := range items {
		id := strconv.Itoa(i)
		items[i] = item("i"+id, i+1, i%3 != 0, t0, comment("c1-"+id, "bob"), comment("c2-"+id, "carol"), comment("c3-"+id, "dave"))
	}
	return items
}

// BenchmarkApplyItemsNoop re-applies an unchanged batch (a full-history source
// re-read by every reconcile).
func BenchmarkApplyItemsNoop(b *testing.B) {
	db, err := Open(b.Context(), b.TempDir()+"/b.db")
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = db.Close() })
	s := New(db)
	src, _ := s.UpsertSource(b.Context(), "nexus", "me")
	projects, err := s.SyncProjects(b.Context(), src, []provider.Project{{ExternalID: "m", Name: "m"}})
	if err != nil {
		b.Fatal(err)
	}
	items := noopBatch()
	if _, err := s.ApplyItems(b.Context(), src, projects[0].ID, items, "me"); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for range b.N {
		if _, err := s.ApplyItems(b.Context(), src, projects[0].ID, items, "me"); err != nil {
			b.Fatal(err)
		}
	}
}
// Re-applying an unchanged batch writes no item or comment row (only the
// project's sync stamp) and reports nothing; a real edit is still written.
func TestApplyItemsNoopWritesNothing(t *testing.T) {
	ctx := t.Context()
	db, err := Open(ctx, t.TempDir()+"/n.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s := New(db)
	src, _ := s.UpsertSource(ctx, "nexus", "me")
	projects, err := s.SyncProjects(ctx, src, []provider.Project{{ExternalID: "m", Name: "m"}})
	if err != nil {
		t.Fatal(err)
	}
	pid := projects[0].ID
	items := noopBatch()[:20]
	if _, err := s.ApplyItems(ctx, src, pid, items, "me"); err != nil {
		t.Fatal(err)
	}
	total := func() int {
		var n int
		if err := db.QueryRowContext(ctx, `SELECT total_changes()`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	before, c0 := total(), s.Changes()
	evs, err := s.ApplyItems(ctx, src, pid, items, "me")
	if err != nil {
		t.Fatal(err)
	}
	if d := total() - before; d != 1 || len(evs) != 0 || s.Changes() != c0 {
		t.Fatalf("no-op re-apply: %d rows written (want 1: the project stamp), %d events, changes %d → %d", d, len(evs), c0, s.Changes())
	}
	items[0].Title = "edited"
	items[1].Comments[0].Body = "edited too"
	before = total()
	if _, err := s.ApplyItems(ctx, src, pid, items, "me"); err != nil {
		t.Fatal(err)
	}
	if d := total() - before; d != 3 {
		t.Fatalf("edit: %d rows written, want 3 (item, comment, project)", d)
	}
	var title, body string
	if err := db.QueryRowContext(ctx, `SELECT (SELECT title FROM items WHERE external_id = 'i0'), (SELECT body FROM comments WHERE external_id = 'c1-1')`).Scan(&title, &body); err != nil ||
		title != "edited" || body != "edited too" {
		t.Fatalf("edits not stored: %q %q %v", title, body, err)
	}
}