package store

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/provider"
)

var t0 = time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC) // Wednesday

func newStore(t *testing.T) *Store {
	t.Helper()
	db, err := Open(t.Context(), filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return New(db)
}

func item(id string, n int, open bool, upd time.Time, comments ...provider.Comment) provider.Item {
	it := provider.Item{
		ExternalID: id, Kind: "issue", Number: n, Title: "title " + id, Body: "body " + id, Author: "alice",
		Open: open, Labels: []string{"bug"}, CreatedAt: t0, UpdatedAt: upd, Comments: comments,
	}
	if !open {
		it.ClosedAt = upd
	}
	return it
}

func comment(id, author string) provider.Comment {
	return provider.Comment{ExternalID: id, Author: author, Body: "text " + id, CreatedAt: t0, UpdatedAt: t0}
}

func setup(t *testing.T) (*Store, int64, Project) {
	t.Helper()
	s := newStore(t)
	src, err := s.UpsertSource(t.Context(), "github", "me")
	if err != nil {
		t.Fatal(err)
	}
	projects, err := s.SyncProjects(t.Context(), src, []provider.Project{{ExternalID: "o/app", Name: "o/app", URL: "u"}})
	if err != nil || len(projects) != 1 || !projects[0].Cursor.IsZero() {
		t.Fatalf("projects %+v %v", projects, err)
	}
	return s, src, projects[0]
}

func kinds(evs []Event) []EventKind {
	out := []EventKind{}
	for _, e := range evs {
		out = append(out, e.Kind)
	}
	return out
}

func TestApplyItemsBaselineThenDiffEvents(t *testing.T) {
	s, src, p := setup(t)
	ctx := t.Context()

	// First sync: silent baseline.
	evs, err := s.ApplyItems(ctx, src, p.ID, []provider.Item{
		item("I1", 1, true, t0, comment("C1", "bob")),
		item("I2", 2, true, t0.Add(time.Hour)),
	}, "me")
	if err != nil || len(evs) != 0 {
		t.Fatalf("baseline events %v err %v", kinds(evs), err)
	}
	if n, _ := s.UnreadCount(ctx); n != 0 {
		t.Fatalf("baseline unread %d", n)
	}

	// Second sync: new issue, new comment by someone else, own comment, close.
	evs, err = s.ApplyItems(ctx, src, p.ID, []provider.Item{
		item("I1", 1, true, t0.Add(2*time.Hour), comment("C1", "bob"), comment("C2", "carol"), comment("C3", "me")),
		item("I2", 2, false, t0.Add(3*time.Hour)),
		item("I3", 3, true, t0.Add(4*time.Hour), comment("C4", "dave")),
	}, "me")
	if err != nil {
		t.Fatal(err)
	}
	got := kinds(evs)
	want := []EventKind{EventNewComment, EventClosed, EventNewIssue}
	if len(got) != len(want) {
		t.Fatalf("events %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("events %v, want %v", got, want)
		}
	}
	if evs[0].Actor != "carol" || evs[0].Repo != "o/app" || evs[0].Number != 1 || evs[2].Actor != "alice" {
		t.Fatalf("event fields %+v", evs)
	}
	if n, _ := s.UnreadCount(ctx); n != 3 {
		t.Fatalf("unread %d, want 3", n)
	}

	// Re-applying the same batch (inclusive since cursor) is idempotent.
	evs, err = s.ApplyItems(ctx, src, p.ID, []provider.Item{
		item("I3", 3, true, t0.Add(4*time.Hour), comment("C4", "dave")),
	}, "me")
	if err != nil || len(evs) != 0 {
		t.Fatalf("replay events %v err %v", kinds(evs), err)
	}

	// Cursor advanced to the newest updated_at.
	projects, err := s.SyncProjects(ctx, src, []provider.Project{{ExternalID: "o/app", Name: "o/app"}})
	if err != nil || !projects[0].Cursor.Equal(t0.Add(4*time.Hour)) {
		t.Fatalf("cursor %v err %v", projects[0].Cursor, err)
	}

	if err := s.MarkRead(ctx, itemID(t, s, "I1")); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.UnreadCount(ctx); n != 2 {
		t.Fatalf("unread after read %d", n)
	}
	if err := s.MarkRead(ctx, 9999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("mark missing: %v", err)
	}
}

func itemID(t *testing.T, s *Store, ext string) int64 {
	t.Helper()
	var id int64
	if err := s.db.QueryRowContext(t.Context(), "SELECT id FROM items WHERE external_id = ?", ext).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestEmptyFirstSyncStillBaselines(t *testing.T) {
	s, src, p := setup(t)
	if _, err := s.ApplyItems(t.Context(), src, p.ID, nil, "me"); err != nil {
		t.Fatal(err)
	}
	evs, err := s.ApplyItems(t.Context(), src, p.ID, []provider.Item{item("I1", 1, true, t0)}, "me")
	if err != nil || len(evs) != 1 || evs[0].Kind != EventNewIssue {
		t.Fatalf("issue after empty baseline: %v %v", kinds(evs), err)
	}
}

func TestSyncProjectsDeactivatesMissing(t *testing.T) {
	s, src, p := setup(t)
	if _, err := s.ApplyItems(t.Context(), src, p.ID, []provider.Item{item("I1", 1, true, t0)}, "me"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SyncProjects(t.Context(), src, []provider.Project{{ExternalID: "o/lib", Name: "o/lib"}}); err != nil {
		t.Fatal(err)
	}
	repos, err := s.Repos(t.Context())
	if err != nil || len(repos) != 1 || repos[0].Name != "o/lib" {
		t.Fatalf("repos %+v %v", repos, err)
	}
	page, err := s.Issues(t.Context(), IssueFilter{})
	if err != nil || page.Total != 0 {
		t.Fatalf("issues of inactive repo visible: %+v %v", page, err)
	}
}

func TestIssuesFiltersAndDetail(t *testing.T) {
	s, src, p := setup(t)
	ctx := t.Context()
	other := item("I2", 2, false, t0.Add(time.Hour))
	other.Labels, other.Title = []string{"feature"}, "50% faster"
	if _, err := s.ApplyItems(ctx, src, p.ID, []provider.Item{
		item("I1", 1, true, t0, comment("C1", "bob"), comment("C2", "eve")), other,
	}, "me"); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		f    IssueFilter
		want []int
	}{
		{IssueFilter{}, []int{2, 1}},
		{IssueFilter{State: "open"}, []int{1}},
		{IssueFilter{State: "closed"}, []int{2}},
		{IssueFilter{Label: "feature"}, []int{2}},
		{IssueFilter{Text: "50%"}, []int{2}},
		{IssueFilter{Text: "#1"}, []int{1}},
		{IssueFilter{Text: "body I1"}, []int{1}},
		{IssueFilter{RepoID: p.ID + 1}, []int{}},
		{IssueFilter{PerPage: 1, Page: 2}, []int{1}},
	}
	for _, c := range cases {
		page, err := s.Issues(ctx, c.f)
		if err != nil {
			t.Fatal(err)
		}
		got := []int{}
		for _, is := range page.Items {
			got = append(got, is.Number)
		}
		if len(got) != len(c.want) || (len(got) > 0 && got[0] != c.want[0]) {
			t.Errorf("filter %+v: got %v want %v", c.f, got, c.want)
		}
	}
	page, _ := s.Issues(ctx, IssueFilter{Text: "#1"})
	d, err := s.Issue(ctx, page.Items[0].ID)
	if err != nil || d.Body != "body I1" || len(d.CommentsList) != 2 || d.Comments != 2 || d.Labels[0] != "bug" || d.Repo != "o/app" {
		t.Fatalf("detail %+v %v", d, err)
	}
	if _, err := s.Issue(ctx, 9999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing issue: %v", err)
	}
	c, err := s.AddComment(ctx, d.ID, comment("C9", "me"))
	if err != nil || c.ExternalID != "C9" {
		t.Fatalf("add comment %+v %v", c, err)
	}
	ref, err := s.ItemRef(ctx, d.ID)
	if err != nil || ref.ExternalID != "I1" || ref.Platform != "github" {
		t.Fatalf("ref %+v %v", ref, err)
	}
}

func TestStatsWeekly(t *testing.T) {
	s, src, p := setup(t)
	ctx := t.Context()
	closed := item("I2", 2, false, t0.AddDate(0, 0, 7)) // closed a week later
	if _, err := s.ApplyItems(ctx, src, p.ID, []provider.Item{item("I1", 1, true, t0), closed}, "me"); err != nil {
		t.Fatal(err)
	}
	now := t0.AddDate(0, 0, 8) // Thursday of the following week
	st, err := s.Stats(ctx, 0, 3, now)
	if err != nil {
		t.Fatal(err)
	}
	if st.Open != 1 || st.Closed != 1 || len(st.Weekly) != 3 {
		t.Fatalf("stats %+v", st)
	}
	// Weeks: 2026-08-24, 2026-08-31 (both created), 2026-09-07 (one closed).
	w := st.Weekly
	if w[1].Start != "2026-08-31" || w[1].Opened != 2 || w[2].Start != "2026-09-07" || w[2].Closed != 1 || w[0].Opened != 0 {
		t.Fatalf("weekly %+v", w)
	}
	st, err = s.Stats(ctx, p.ID+1, 3, now)
	if err != nil || st.Open != 0 || st.Weekly[1].Opened != 0 {
		t.Fatalf("other repo stats %+v %v", st, err)
	}
}
