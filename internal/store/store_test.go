package store

import (
	"errors"
	"path/filepath"
	"strconv"
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
	if n, _ := s.UnreadCount(ctx); n != 2 { // I1 (comment), I3 (new); the closed I2 is read
		t.Fatalf("unread %d, want 2", n)
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
	if n, _ := s.UnreadCount(ctx); n != 1 {
		t.Fatalf("unread after read %d", n)
	}
	if err := s.MarkRead(ctx, 9999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("mark missing: %v", err)
	}
}

// A closed item never counts as unread: closing an unread item (a sync close,
// or our own push/Fixes seen on sync) marks it read, a comment on a closed item
// keeps it read, and every counter and the unread filter skip it. Reopening
// with news makes it unread again.
func TestClosedItemsNeverUnread(t *testing.T) {
	s, src, p := setup(t)
	ctx := t.Context()
	if _, err := s.ApplyItems(ctx, src, p.ID, []provider.Item{item("A", 1, true, t0), item("B", 2, true, t0)}, "me"); err != nil {
		t.Fatal(err)
	}
	// News on both: unread.
	evs, err := s.ApplyItems(ctx, src, p.ID, []provider.Item{
		item("A", 1, true, t0.Add(time.Hour), comment("C1", "bob")),
		item("B", 2, true, t0.Add(time.Hour), comment("C2", "bob")),
	}, "me")
	if err != nil || len(evs) != 2 {
		t.Fatalf("events %v %v", kinds(evs), err)
	}
	if n, _ := s.UnreadCount(ctx); n != 2 {
		t.Fatalf("unread before close %d", n)
	}
	// B closed (with a new comment in the same sync): read, but the events still come.
	evs, err = s.ApplyItems(ctx, src, p.ID, []provider.Item{
		item("B", 2, false, t0.Add(2*time.Hour), comment("C2", "bob"), comment("C3", "carol")),
	}, "me")
	if err != nil || len(evs) != 2 {
		t.Fatalf("close events %v %v", kinds(evs), err)
	}
	// A comment on the closed B later: still read.
	if evs, err = s.ApplyItems(ctx, src, p.ID, []provider.Item{
		item("B", 2, false, t0.Add(3*time.Hour), comment("C2", "bob"), comment("C3", "carol"), comment("C4", "dave")),
	}, "me"); err != nil || len(evs) != 1 {
		t.Fatalf("comment on closed %v %v", kinds(evs), err)
	}
	wantUnread := func(n int, why string) {
		t.Helper()
		if got, _ := s.UnreadCount(ctx); got != n {
			t.Fatalf("%s: tray unread %d, want %d", why, got, n)
		}
		repos, err := s.Repos(ctx)
		if err != nil || len(repos) != 1 || repos[0].Unread != n {
			t.Fatalf("%s: project unread %+v %v", why, repos, err)
		}
		chunk, err := s.ReposChunk(ctx, RepoQuery{})
		if err != nil || len(chunk.Items) != 1 || chunk.Items[0].Unread != n {
			t.Fatalf("%s: sidebar unread %+v %v", why, chunk.Items, err)
		}
		all, err := s.Issues(ctx, IssueFilter{Limit: 50})
		if err != nil || all.Counts == nil || all.Counts.Unread != n {
			t.Fatalf("%s: header counts %+v %v", why, all.Counts, err)
		}
		un, err := s.Issues(ctx, IssueFilter{Unread: true, Limit: 50})
		if err != nil || len(un.Items) != n {
			t.Fatalf("%s: unread filter %d %v", why, len(un.Items), err)
		}
		for _, it := range append(all.Items, un.Items...) {
			if it.State == "closed" && it.Unread {
				t.Fatalf("%s: closed item %d unread", why, it.Number)
			}
		}
	}
	wantUnread(1, "after close")
	// Reopened with a new comment: unread again.
	if _, err := s.ApplyItems(ctx, src, p.ID, []provider.Item{
		item("B", 2, true, t0.Add(4*time.Hour), comment("C2", "bob"), comment("C3", "carol"), comment("C4", "dave"), comment("C5", "erin")),
	}, "me"); err != nil {
		t.Fatal(err)
	}
	wantUnread(2, "after reopen")
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

func TestApplyItemsKinds(t *testing.T) {
	s, src, p := setup(t)
	ctx := t.Context()
	kinded := func(id, kind, author string, comments ...provider.Comment) provider.Item {
		it := item(id, 0, true, t0, comments...)
		it.Kind, it.Author = kind, author
		return it
	}
	// Baseline: a comment thread stays silent; an empty kind is stored as issue.
	legacy := item("I0", 1, true, t0)
	legacy.Kind = ""
	if evs, err := s.ApplyItems(ctx, src, p.ID, []provider.Item{kinded("comment:1", KindComment, "bob"), legacy}, "me"); err != nil || len(evs) != 0 {
		t.Fatalf("baseline %v %v", kinds(evs), err)
	}
	var k string
	if err := s.db.QueryRowContext(ctx, "SELECT kind FROM items WHERE external_id = 'I0'").Scan(&k); err != nil || k != KindIssue {
		t.Fatalf("legacy kind %q %v", k, err)
	}
	evs, err := s.ApplyItems(ctx, src, p.ID, []provider.Item{
		kinded("comment:1", KindComment, "bob", comment("r1", "carol"), comment("r2", "me")), // reply in the thread; own reply silent
		kinded("comment:2", KindComment, "me"),                                               // own new thread: silent
		kinded("bug:3", KindBug, "dave"),
	}, "me")
	if err != nil || len(evs) != 2 {
		t.Fatalf("events %+v %v", evs, err)
	}
	if evs[0].Kind != EventNewComment || evs[0].ItemKind != KindComment || evs[0].Actor != "carol" ||
		evs[1].Kind != EventNewItem || evs[1].ItemKind != KindBug || evs[1].Actor != "dave" {
		t.Fatalf("events %+v", evs)
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
	if err != nil || page.Total == nil || *page.Total != 0 {
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
	// Header counters follow every filter but state and unread.
	if pg, err := s.Issues(ctx, IssueFilter{State: "open"}); err != nil || pg.Counts == nil || *pg.Counts != (IssueCounts{Open: 1, Closed: 1}) {
		t.Fatalf("counts open: %+v %v", pg.Counts, err)
	}
	if pg, err := s.Issues(ctx, IssueFilter{State: "open", Label: "feature", Unread: true}); err != nil || pg.Counts == nil || *pg.Counts != (IssueCounts{Closed: 1}) {
		t.Fatalf("counts label: %+v %v", pg.Counts, err)
	}
	if pg, err := s.Issues(ctx, IssueFilter{Platform: "nexus"}); err != nil || pg.Counts == nil || *pg.Counts != (IssueCounts{}) {
		t.Fatalf("counts other platform: %+v %v", pg.Counts, err)
	}
	page, _ := s.Issues(ctx, IssueFilter{Text: "#1"})
	d, err := s.Issue(ctx, page.Items[0].ID)
	if err != nil || d.Body != "body I1" || d.Comments != 2 || d.Labels[0] != "bug" || d.Repo != "o/app" {
		t.Fatalf("detail %+v %v", d, err)
	}
	if _, err := s.Issue(ctx, 9999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing issue: %v", err)
	}
	c, err := s.AddComment(ctx, d.ID, comment("C9", "me"))
	if err != nil || c.ExternalID != "C9" {
		t.Fatalf("add comment %+v %v", c, err)
	}
	ch, err := s.Comments(ctx, d.ID, "", 2)
	if err != nil || len(ch.Items) != 2 || !ch.More {
		t.Fatalf("comments chunk 1 %+v %v", ch, err)
	}
	ch, err = s.Comments(ctx, d.ID, ch.NextCursor, 2)
	if err != nil || len(ch.Items) != 1 || ch.More || ch.Items[0].ExternalID != "C9" {
		t.Fatalf("comments chunk 2 %+v %v", ch, err)
	}
	if again, _ := s.Comments(ctx, d.ID, ch.NextCursor, 2); len(again.Items) != 0 || again.NextCursor != ch.NextCursor {
		t.Fatalf("comments tail %+v", again)
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

// Keyset chunks: every row exactly once in order, ties on updated_at broken by
// id, head refresh sees only newer rows, IDs re-check the filter.
func TestIssuesKeysetChunks(t *testing.T) {
	s, src, p := setup(t)
	ctx := t.Context()
	var items []provider.Item
	for n := 1; n <= 23; n++ {
		items = append(items, item("K"+strconv.Itoa(n), n, n%2 == 0, t0.Add(time.Duration(n/3)*time.Minute)))
	}
	if _, err := s.ApplyItems(ctx, src, p.ID, items, "me"); err != nil {
		t.Fatal(err)
	}
	var (
		seen   []int64
		cursor string
		chunks int
		head   string
	)
	for {
		c, err := s.Issues(ctx, IssueFilter{Cursor: cursor, Limit: 5})
		if err != nil {
			t.Fatal(err)
		}
		chunks++
		if cursor == "" {
			head = c.HeadCursor
			if c.Total == nil || *c.Total != 23 {
				t.Fatalf("total %v", c.Total)
			}
		}
		for _, is := range c.Items {
			seen = append(seen, is.ID)
		}
		if !c.More {
			break
		}
		cursor = c.NextCursor
	}
	uniq := map[int64]bool{}
	for _, id := range seen {
		uniq[id] = true
	}
	if len(seen) != 23 || len(uniq) != 23 || chunks != 5 {
		t.Fatalf("rows %d unique %d chunks %d", len(seen), len(uniq), chunks)
	}
	all, _ := s.Issues(ctx, IssueFilter{Limit: 200})
	for i, is := range all.Items {
		if is.ID != seen[i] {
			t.Fatalf("chunk order differs at %d", i)
		}
	}
	if c, _ := s.Issues(ctx, IssueFilter{After: head}); len(c.Items) != 0 {
		t.Fatalf("after head: %d rows", len(c.Items))
	}
	newer := item("K99", 99, true, t0.Add(time.Hour))
	if _, err := s.ApplyItems(ctx, src, p.ID, []provider.Item{newer}, "me"); err != nil {
		t.Fatal(err)
	}
	c, _ := s.Issues(ctx, IssueFilter{After: head})
	if len(c.Items) != 1 || c.Items[0].Number != 99 || c.More {
		t.Fatalf("after head: %+v", c)
	}
	ids := []int64{all.Items[0].ID, all.Items[1].ID}
	c, _ = s.Issues(ctx, IssueFilter{IDs: ids, State: "open"})
	if len(c.Items) != 1 {
		t.Fatalf("ids+filter: %d rows", len(c.Items))
	}
	if _, err := s.Issues(ctx, IssueFilter{Cursor: "junk"}); !errors.Is(err, ErrBadCursor) {
		t.Fatalf("bad cursor: %v", err)
	}
	ch, err := s.ReposChunk(ctx, RepoQuery{Sort: "open", Desc: true, Limit: 1})
	if err != nil || len(ch.Items) != 1 || ch.More || ch.Total != 1 {
		t.Fatalf("repos chunk %+v %v", ch, err)
	}
}
