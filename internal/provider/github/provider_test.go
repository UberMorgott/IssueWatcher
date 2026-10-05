package github

import (
	"errors"
	"strings"
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

// Pull requests share the REST issues and comments endpoints; they are not
// synced, so a PR and a PR comment in the since window must not reach the
// targeted issue(number:) fetch, and a PR number that gets there anyway only
// skips itself.
func TestChangeCheckSkipsPullRequests(t *testing.T) {
	gh := githubtest.New(t)
	seed(gh)
	t1 := t0.Add(11 * time.Hour)
	gh.Mu.Lock()
	is := gh.Issues[1] // octo/app#2
	is.Comments = append(is.Comments, githubtest.Comment{ID: "C_new", Author: "bob", Body: "more", CreatedAt: t1})
	is.UpdatedAt = t1
	gh.PullRequests = []*githubtest.Issue{{Repo: "octo/app", Number: 9, UpdatedAt: t1,
		Comments: []githubtest.Comment{{ID: "C_pr", Author: "bob", Body: "lgtm", CreatedAt: t1}}}}
	gh.Mu.Unlock()
	a := newAuth(t, gh)
	signIn(t, gh, a)
	p := NewProvider(a)
	project := provider.Project{ExternalID: "octo/app"}

	st := provider.PollState{IssuesSince: t0.Add(10 * time.Hour), CommentsSince: t0.Add(10 * time.Hour)}
	ch, err := p.DetectChanges(t.Context(), project, &st)
	if err != nil || len(ch.Numbers) != 1 || ch.Numbers[0] != 2 {
		t.Fatalf("changes %+v %v, want only #2", ch, err)
	}
	if !st.IssuesSince.Equal(t1) || !st.CommentsSince.Equal(t1) {
		t.Fatalf("since not advanced past the PR: %+v", st)
	}

	items, err := p.FetchChanged(t.Context(), project, []int{2, 9})
	sk, ok := errors.AsType[*provider.SkippedError](err)
	if !ok || len(sk.Items) != 1 || sk.Items[9] == nil {
		t.Fatalf("err %v, want #9 skipped", err)
	}
	if len(items) != 1 || items[0].Number != 2 || len(items[0].Comments) != 2 {
		t.Fatalf("items %+v", items)
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

// The release run's close step and reply probe: IssueStatus reads the state
// and comments, CloseIssue closes as completed (closing twice is fine).
func TestProviderCloseIssueAndStatus(t *testing.T) {
	gh := githubtest.New(t)
	seed(gh)
	a := newAuth(t, gh)
	signIn(t, gh, a)
	p := NewProvider(a)
	if _, err := p.Reply(t.Context(), "I_1", "Fixed in v1.0.1.\n<!-- issuewatcher:reply:3:1 -->"); err != nil {
		t.Fatal(err)
	}
	open, comments, err := p.IssueStatus(t.Context(), "I_1")
	if err != nil || !open || len(comments) == 0 || !strings.Contains(comments[len(comments)-1].Body, "issuewatcher:reply:3:1") ||
		comments[len(comments)-1].Author != githubtest.Login {
		t.Fatalf("status %v %v %+v", open, err, comments)
	}
	for range 2 {
		if err := p.CloseIssue(t.Context(), "I_1"); err != nil {
			t.Fatal(err)
		}
	}
	if open, _, err := p.IssueStatus(t.Context(), "I_1"); err != nil || open {
		t.Fatalf("after close: open %v %v", open, err)
	}
	if _, _, err := p.IssueStatus(t.Context(), "I_missing"); !errors.Is(err, ErrNotIssue) {
		t.Fatalf("missing: %v", err)
	}
}

// The regression breaker's collaborator check: the issue author's association.
func TestProviderIssueAuthorAssociation(t *testing.T) {
	gh := githubtest.New(t)
	seed(gh)
	gh.Mu.Lock()
	gh.Issues[0].Association = "COLLABORATOR"
	id := gh.Issues[0].ID
	gh.Mu.Unlock()
	a := newAuth(t, gh)
	signIn(t, gh, a)
	p := NewProvider(a)
	if as, err := p.IssueAuthorAssociation(t.Context(), id); err != nil || as != "COLLABORATOR" || !Collaborator(as) {
		t.Fatalf("association %q %v", as, err)
	}
	if Collaborator("NONE") || Collaborator("CONTRIBUTOR") || !Collaborator("OWNER") || !Collaborator("MEMBER") {
		t.Fatal("Collaborator")
	}
	if _, err := p.IssueAuthorAssociation(t.Context(), "I_missing"); !errors.Is(err, ErrNotIssue) {
		t.Fatalf("missing: %v", err)
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
