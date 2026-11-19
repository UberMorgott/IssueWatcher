package syncer

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/provider"
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

// A transferred issue vanishes from the old repo's listings: the reconcile
// closes the stale copy instead of keeping it open under the old project.
func TestTransferredIssueClosedInOldRepo(t *testing.T) {
	s, gh, st, _ := setup(t, true)
	gh.Mu.Lock()
	gh.Repos = append(gh.Repos, "octo/tools")
	gh.Mu.Unlock()
	if err := s.SyncOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	t1 := t0.Add(time.Hour)
	gh.Mu.Lock()
	*gh.Issues[1] = githubtest.Issue{ID: "I_7", Repo: "octo/tools", Number: 7, Title: "two", Author: "alice", Open: true, CreatedAt: t0, UpdatedAt: t1}
	gh.Mu.Unlock()
	if err := s.SyncOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	page, err := st.Issues(t.Context(), store.IssueFilter{Text: "two"})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, it := range page.Items {
		got[it.Repo+"#"+strconv.Itoa(it.Number)] = it.State + ":" + it.RawStatus
	}
	if got["octo/app#2"] != "closed:gone" || got["octo/tools#7"] != "open:open" || len(got) != 2 {
		t.Fatalf("items %v", got)
	}
}

// Issues turned off on a repo fail every issue lookup: that must not close them.
func TestIssuesDisabledKeepsItemsOpen(t *testing.T) {
	s, gh, st, _ := setup(t, true)
	if err := s.SyncOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	gh.Mu.Lock()
	gh.Issues = nil
	gh.IssuesDisabled = map[string]bool{"octo/app": true}
	gh.Mu.Unlock()
	if err := s.SyncOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	page, err := st.Issues(t.Context(), store.IssueFilter{})
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range page.Items {
		if it.State != "open" {
			t.Fatalf("#%d %s:%s, want open", it.Number, it.State, it.RawStatus)
		}
	}
	if len(page.Items) != 2 {
		t.Fatalf("items %d, want 2", len(page.Items))
	}
}

type failingVanisher struct{ *github.Provider }

func (failingVanisher) Missing(context.Context, provider.Project, []int) ([]int, error) {
	return nil, errors.New("boom")
}

// A failing vanish check is best-effort: the reconcile still succeeds and
// reports the stored items' events.
func TestMissingErrorDoesNotFailReconcile(t *testing.T) {
	s, gh, _, updates := setup(t, true)
	s.opts.Provider = failingVanisher{s.opts.Provider.(*github.Provider)}
	if err := s.SyncOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	<-updates
	t1 := t0.Add(time.Hour)
	gh.Mu.Lock()
	gh.Issues = append(gh.Issues, &githubtest.Issue{ID: "I_3", Repo: "octo/app", Number: 3, Title: "three",
		Author: "carol", Open: true, CreatedAt: t1, UpdatedAt: t1})
	gh.Mu.Unlock()
	if err := s.SyncOnce(t.Context()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if u := <-updates; len(u.events) != 1 || u.events[0].Number != 3 {
		t.Fatalf("events %+v, want new #3", u.events)
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
