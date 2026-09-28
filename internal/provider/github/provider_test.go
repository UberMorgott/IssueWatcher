package github

import (
	"errors"
	"testing"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/provider"
	"github.com/UberMorgott/issuewatcher/internal/provider/github/githubtest"
)

var t0 = time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)

func seed(gh *githubtest.Server) {
	gh.Mu.Lock()
	defer gh.Mu.Unlock()
	gh.Repos = []string{"octo/app", "octo/lib"}
	for i := 1; i <= 5; i++ {
		gh.Issues = append(gh.Issues, &githubtest.Issue{
			ID: "I_" + string(rune('0'+i)), Repo: "octo/app", Number: i, Title: "issue", Author: "alice",
			Open: i != 3, Labels: []string{"bug"}, CreatedAt: t0, UpdatedAt: t0.Add(time.Duration(i) * time.Hour),
			ClosedAt: t0.Add(time.Hour),
			Comments: []githubtest.Comment{{ID: "C_" + string(rune('0'+i)), Author: "bob", Body: "hi", CreatedAt: t0}},
		})
	}
}

func TestProviderListsReposAndPagesIssues(t *testing.T) {
	gh := githubtest.New(t)
	seed(gh)
	a := newAuth(t, gh)
	signIn(t, gh, a)
	p := NewProvider(a)

	acc, err := p.Account(t.Context())
	if err != nil || acc != githubtest.Login {
		t.Fatalf("account %q %v", acc, err)
	}
	projects, err := p.ListProjects(t.Context())
	if err != nil || len(projects) != 2 || projects[0].ExternalID != "octo/app" {
		t.Fatalf("projects %+v %v", projects, err)
	}
	items, err := p.SyncItems(t.Context(), projects[0], time.Time{})
	if err != nil || len(items) != 5 { // 3 fake pages of 2
		t.Fatalf("items %d %v", len(items), err)
	}
	if it := items[2]; it.Open || it.ClosedAt.IsZero() || it.RawStatus != "closed" || len(it.Comments) != 1 ||
		it.Labels[0] != "bug" || it.Author != "alice" {
		t.Fatalf("closed item %+v", it)
	}
	// Incremental: only issues updated at/after since.
	items, err = p.SyncItems(t.Context(), projects[0], t0.Add(4*time.Hour))
	if err != nil || len(items) != 2 {
		t.Fatalf("since: %d %v", len(items), err)
	}
}

func TestProviderReply(t *testing.T) {
	gh := githubtest.New(t)
	seed(gh)
	a := newAuth(t, gh)
	signIn(t, gh, a)
	c, err := NewProvider(a).Reply(t.Context(), "I_1", "thanks")
	if err != nil || c.Body != "thanks" || c.Author != githubtest.Login || c.ExternalID == "" {
		t.Fatalf("reply %+v %v", c, err)
	}
}

func TestProviderUnauthorizedSignsOut(t *testing.T) {
	gh := githubtest.New(t)
	a := newAuth(t, gh)
	signIn(t, gh, a)
	gh.RevokeAccess()
	if _, err := NewProvider(a).ListProjects(t.Context()); !errors.Is(err, provider.ErrNotSignedIn) {
		t.Fatalf("revoked: %v", err)
	}
	if a.Login() != "" {
		t.Fatal("token kept after 401")
	}
}

func TestRateLimitGuard(t *testing.T) {
	c := &client{now: time.Now}
	c.remaining, c.reset, c.known = 10, time.Now().Add(time.Hour), true
	var rl *provider.RateLimitError
	if err := c.checkQuota(minRemaining); !errors.As(err, &rl) {
		t.Fatalf("low quota: %v", err)
	}
	if err := c.checkQuota(0); err != nil { // user actions still allowed
		t.Fatalf("reserve 0: %v", err)
	}
	c.reset = time.Now().Add(-time.Second)
	if err := c.checkQuota(minRemaining); err != nil {
		t.Fatalf("after reset: %v", err)
	}
}
