package syncer

import (
	"log/slog"
	"path/filepath"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/provider/github"
	"github.com/UberMorgott/issuewatcher/internal/provider/github/githubtest"
	"github.com/UberMorgott/issuewatcher/internal/store"
)

var t0 = time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)

type update struct {
	events []store.Event
	unread int
}

func setup(t *testing.T, signIn bool) (*Syncer, *githubtest.Server, *store.Store, chan update) {
	t.Helper()
	gh := githubtest.New(t)
	gh.Mu.Lock()
	gh.Repos = []string{"octo/app"}
	gh.Issues = []*githubtest.Issue{
		{ID: "I_1", Repo: "octo/app", Number: 1, Title: "one", Author: "alice", Open: true, CreatedAt: t0, UpdatedAt: t0},
		{ID: "I_2", Repo: "octo/app", Number: 2, Title: "two", Author: "alice", Open: true, CreatedAt: t0, UpdatedAt: t0},
	}
	gh.Mu.Unlock()

	a := github.NewAuth(filepath.Join(t.TempDir(), "secrets"))
	a.WebURL, a.APIURL, a.HTTP = gh.URL, gh.URL, gh.Client()
	if signIn {
		if _, err := a.ConvertManifest(t.Context(), githubtest.ManifestCode, 1); err != nil {
			t.Fatal(err)
		}
		v, c := github.PKCE()
		if _, err := a.Exchange(t.Context(), gh.IssueCode(c), v, "r"); err != nil {
			t.Fatal(err)
		}
	}
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := store.New(db)
	updates := make(chan update, 8)
	s := New(Options{
		Store: st, Provider: github.NewProvider(a), Log: slog.New(slog.DiscardHandler),
		OnUpdate: func(evs []store.Event, unread int) { updates <- update{evs, unread} },
	})
	return s, gh, st, updates
}

func TestSyncDetectsNewIssueCommentAndClose(t *testing.T) {
	s, gh, st, updates := setup(t, true)
	if err := s.SyncOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	if u := <-updates; len(u.events) != 0 || u.unread != 0 {
		t.Fatalf("baseline update %+v", u)
	}
	if st := s.Status(); !st.SignedIn || st.LastSync == "" || st.LastError != "" {
		t.Fatalf("status %+v", st)
	}

	t1 := t0.Add(time.Hour)
	gh.Mu.Lock()
	gh.Issues[0].Comments = append(gh.Issues[0].Comments, githubtest.Comment{ID: "C_1", Author: "bob", Body: "ping", CreatedAt: t1})
	gh.Issues[0].UpdatedAt = t1
	gh.Issues[1].Open, gh.Issues[1].ClosedAt, gh.Issues[1].UpdatedAt = false, t1, t1
	gh.Issues = append(gh.Issues, &githubtest.Issue{ID: "I_3", Repo: "octo/app", Number: 3, Title: "three",
		Author: "carol", Open: true, CreatedAt: t1, UpdatedAt: t1})
	gh.Mu.Unlock()

	if err := s.SyncOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	u := <-updates
	got := []string{}
	for _, e := range u.events {
		got = append(got, string(e.Kind)+"#"+strconv.Itoa(e.Number))
	}
	sort.Strings(got)
	want := []string{"issue_closed#2", "new_comment#1", "new_issue#3"}
	if len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] || u.unread != 2 { // the closed #2 is read
		t.Fatalf("events %v unread %d, want %v / 2", got, u.unread, want)
	}

	// Reply goes to GitHub, lands in the store, and the next sync stays quiet.
	page, err := st.Issues(t.Context(), store.IssueFilter{Text: "#1"})
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("lookup: %+v %v", page, err)
	}
	c, err := s.Reply(t.Context(), page.Items[0].ID, "fixed")
	if err != nil || c.Body != "fixed" || c.Author != githubtest.Login {
		t.Fatalf("reply %+v %v", c, err)
	}
	if err := s.SyncOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	if u := <-updates; len(u.events) != 0 {
		t.Fatalf("own reply produced events %+v", u.events)
	}
}

func TestSignedOutIsQuiet(t *testing.T) {
	s, _, _, updates := setup(t, false)
	if err := s.SyncOnce(t.Context()); err != nil {
		t.Fatalf("signed out: %v", err)
	}
	if st := s.Status(); st.SignedIn || st.LastError != "" {
		t.Fatalf("status %+v", st)
	}
	select {
	case u := <-updates:
		t.Fatalf("update while signed out: %+v", u)
	default:
	}
}

func TestTriggerCoalesces(t *testing.T) {
	s := New(Options{})
	s.Trigger()
	s.Trigger() // must not block
	if len(s.trigger) != 1 {
		t.Fatalf("pending triggers %d", len(s.trigger))
	}
}
