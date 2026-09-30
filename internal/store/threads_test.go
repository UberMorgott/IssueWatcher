package store

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/provider"
)

// thread is a mod page comment thread by author with replies.
func thread(id string, n int, author string, replies ...provider.Comment) provider.Item {
	it := item(id, n, true, t0, replies...)
	it.Kind, it.Author, it.Labels = KindComment, author, nil
	return it
}

func reply(id, author string, at time.Time) provider.Comment {
	return provider.Comment{ExternalID: id, Author: author, Body: "reply " + id, CreatedAt: at, UpdatedAt: at}
}

// states maps each listed item's external number to its state (+ resolved).
func states(t *testing.T, s *Store, f IssueFilter) map[int]string {
	t.Helper()
	c, err := s.Issues(t.Context(), f)
	if err != nil {
		t.Fatal(err)
	}
	out := map[int]string{}
	for _, it := range c.Items {
		st := it.State
		if it.Resolved {
			st += "+resolved"
		}
		if it.Unread {
			st += "+unread"
		}
		out[it.Number] = st
	}
	return out
}

// Comment threads behave like comments: open = waiting for the owner's answer;
// the owner's own threads (a Steam comment) never show; «Решено» is local and
// a new message from someone else reopens the thread.
func TestCommentThreadStates(t *testing.T) {
	s, src, p := setup(t)
	ctx := t.Context()
	h := t0.Add(time.Hour)
	base := []provider.Item{
		thread("T1", 1, "bob"),                       // waiting
		thread("T2", 2, "bob", reply("r1", "me", h)), // answered
		thread("T3", 3, "me"),                        // own only: hidden
		thread("T4", 4, "me", reply("r2", "bob", h)), // own thread, bob replied: waiting
		thread("T5", 5, "bob", reply("r3", "me", h), reply("r4", "bob", h.Add(time.Minute))), // bob wrote again
	}
	if _, err := s.ApplyItems(ctx, src, p.ID, base, "me"); err != nil {
		t.Fatal(err)
	}
	all := IssueFilter{Kinds: []string{KindComment}}
	want := map[int]string{1: "open", 2: "closed", 4: "open", 5: "open"}
	if got := states(t, s, all); !equalMap(got, want) {
		t.Fatalf("states %v, want %v", got, want)
	}

	// Resolve T1 and T4: closed, counted apart; a repeat changes nothing.
	if n, err := s.SetResolved(ctx, []int64{id(t, s, "T1"), id(t, s, "T4")}, true); err != nil || n != 2 {
		t.Fatalf("resolve: %d %v", n, err)
	}
	if n, _ := s.SetResolved(ctx, []int64{id(t, s, "T1")}, true); n != 0 {
		t.Fatalf("resolve again changed %d", n)
	}
	c, err := s.Issues(ctx, IssueFilter{Kinds: []string{KindComment}, State: "resolved"})
	if err != nil || len(c.Items) != 2 || c.Counts == nil || *c.Counts != (IssueCounts{Open: 1, Closed: 3, Resolved: 2}) {
		t.Fatalf("resolved list %+v %+v %v", c.Items, c.Counts, err)
	}

	// Sync again: bob replies to the resolved T1 (reopens, unread); T4 stays resolved; the owner answers T5.
	next := []provider.Item{
		thread("T1", 1, "bob", reply("r5", "bob", h)),
		base[1], base[2], base[3],
		thread("T5", 5, "bob", reply("r3", "me", h), reply("r4", "bob", h.Add(time.Minute)), reply("r6", "me", h.Add(2*time.Minute))),
	}
	evs, err := s.ApplyItems(ctx, src, p.ID, next, "me")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range evs {
		if e.Kind == EventClosed {
			t.Fatalf("answering a thread is no platform close: %+v", evs)
		}
	}
	want = map[int]string{1: "open+unread", 2: "closed", 4: "closed+resolved", 5: "closed"}
	if got := states(t, s, all); !equalMap(got, want) {
		t.Fatalf("after sync %v, want %v", got, want)
	}

	// Reopen T4: bob wrote last, so it waits again. The owner's reply from the app answers T1.
	if n, err := s.SetResolved(ctx, []int64{id(t, s, "T4")}, false); err != nil || n != 1 {
		t.Fatalf("unresolve: %d %v", n, err)
	}
	if _, err := s.AddComment(ctx, id(t, s, "T1"), reply("r7", "me", h.Add(time.Hour))); err != nil {
		t.Fatal(err)
	}
	want = map[int]string{1: "closed", 2: "closed", 4: "open", 5: "closed"}
	if got := states(t, s, all); !equalMap(got, want) {
		t.Fatalf("after reopen/answer %v, want %v", got, want)
	}

	// Issues are never resolved; the own thread stays out of every count.
	if n, _ := s.SetResolved(ctx, []int64{id(t, s, "T3")}, false); n != 0 {
		t.Fatalf("hidden thread changed")
	}
	repos, err := s.Repos(ctx)
	if err != nil || repos[0].Open != 1 || repos[0].Closed != 3 || repos[0].OpenComments != 1 {
		t.Fatalf("repo counts %+v %v", repos, err)
	}
	if c, _ := s.Comments(ctx, id(t, s, "T5"), "", 10); len(c.Items) != 3 || !c.Items[0].Mine || c.Items[1].Mine {
		t.Fatalf("comment mine flags %+v", c.Items)
	}
}

func id(t *testing.T, s *Store, ext string) int64 {
	t.Helper()
	var n int64
	if err := s.db.QueryRowContext(t.Context(), `SELECT id FROM items WHERE external_id = ?`, ext).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func equalMap(a, b map[int]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// Bulk read / unread report the rows that really changed; a closed item never becomes unread.
func TestSetReadBulk(t *testing.T) {
	s, src, p := setup(t)
	ctx := t.Context()
	if _, err := s.ApplyItems(ctx, src, p.ID, []provider.Item{item("A", 1, true, t0), item("B", 2, false, t0)}, "me"); err != nil {
		t.Fatal(err)
	}
	a, b := id(t, s, "A"), id(t, s, "B")
	if n, err := s.SetRead(ctx, []int64{a, b}, false); err != nil || n != 1 {
		t.Fatalf("unread: %d %v", n, err)
	}
	if n, _ := s.UnreadCount(ctx); n != 1 {
		t.Fatalf("unread count %d", n)
	}
	if n, err := s.SetRead(ctx, []int64{a, b}, true); err != nil || n != 1 {
		t.Fatalf("read: %d %v", n, err)
	}
	if n, _ := s.SetRead(ctx, []int64{a}, true); n != 0 {
		t.Fatalf("read again changed %d", n)
	}
}

// Stats count issues and bug reports; comment threads are their own series.
func TestStatsKindSplit(t *testing.T) {
	s, src, p := setup(t)
	ctx := t.Context()
	bug := item("B", 3, true, t0)
	bug.Kind = KindBug
	if _, err := s.ApplyItems(ctx, src, p.ID, []provider.Item{
		item("I", 1, true, t0), item("J", 2, false, t0.Add(time.Hour)), bug,
		thread("C1", 4, "bob"), thread("C2", 5, "bob", reply("r", "me", t0)), thread("C3", 6, "me"),
	}, "me"); err != nil {
		t.Fatal(err)
	}
	st, err := s.Stats(ctx, 0, 2, t0)
	if err != nil {
		t.Fatal(err)
	}
	last := st.Weekly[len(st.Weekly)-1]
	if st.Open != 2 || st.Closed != 1 || st.OpenComments != 1 || st.Comments != 2 || last.Opened != 3 || last.Closed != 1 || last.Comments != 2 {
		t.Fatalf("stats %+v", st)
	}
}

// Search: text matches replies too; a bare number matches the number or the text.
func TestIssueSearch(t *testing.T) {
	s, src, p := setup(t)
	ctx := t.Context()
	a := item("A", 7, true, t0)
	b := item("B", 8, true, t0, provider.Comment{ExternalID: "c", Author: "x", Body: "crash after 7 minutes", CreatedAt: t0, UpdatedAt: t0})
	if _, err := s.ApplyItems(ctx, src, p.ID, []provider.Item{a, b}, "me"); err != nil {
		t.Fatal(err)
	}
	if l, err := s.ItemLabels(ctx, []string{KindIssue}); err != nil || len(l) != 1 || l[0] != "bug" {
		t.Fatalf("labels %v %v", l, err)
	}
	if l, _ := s.ItemLabels(ctx, []string{KindComment}); len(l) != 0 {
		t.Fatalf("comment labels %v", l)
	}
	for q, want := range map[string]int{"7": 2, "#7": 1, "crash": 1, "minutes": 1, "nothing": 0} {
		c, err := s.Issues(ctx, IssueFilter{Text: q})
		if err != nil || len(c.Items) != want {
			t.Errorf("q=%q: %d items %v, want %d", q, len(c.Items), err, want)
		}
	}
}

// Migration 015 backfills thread states: answered threads close, own-only
// threads hide, and neither stays unread.
func TestMigration015CommentState(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	if err := migrateTo(ctx, db, 14); err != nil {
		t.Fatal(err)
	}
	s := New(db)
	src, err := s.UpsertSource(ctx, "nexus", "me")
	if err != nil {
		t.Fatal(err)
	}
	projects, err := seedProjects(ctx, db, src, []provider.Project{{ExternalID: "g/1", Name: "Mod", URL: "u"}})
	if err != nil {
		t.Fatal(err)
	}
	h := t0.Add(time.Hour)
	if _, err := seedItems(ctx, db, src, projects[0].ID, []provider.Item{
		thread("W", 1, "bob"), thread("A", 2, "bob", reply("r", "me", h)), thread("O", 3, "me"), item("I", 4, true, t0),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE items SET unread = 1`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	rows, err := db.QueryContext(ctx, `SELECT external_id, status, hidden, unread FROM items ORDER BY number`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var got []string
	for rows.Next() {
		var (
			ext, status    string
			hidden, unread int
		)
		if err := rows.Scan(&ext, &status, &hidden, &unread); err != nil {
			t.Fatal(err)
		}
		got = append(got, fmt.Sprintf("%s:%s:%d%d", ext, status, hidden, unread))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	want := []string{"W:open:01", "A:closed:00", "O:closed:10", "I:open:01"}
	if len(got) != len(want) {
		t.Fatalf("rows %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("rows %v, want %v", got, want)
		}
	}
}
