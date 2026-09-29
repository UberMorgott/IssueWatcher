package syncer

import (
	"database/sql"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/provider/github"
	"github.com/UberMorgott/issuewatcher/internal/provider/github/githubtest"
	"github.com/UberMorgott/issuewatcher/internal/store"
)

// clock is a settable fake time source.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// tiered builds a signed-in syncer over the fake with a fake clock; repos get
// one issue each, recently updated (active).
func tiered(t *testing.T, repos []string, plan Plan) (*Syncer, *githubtest.Server, *clock, chan update) {
	t.Helper()
	s, gh, clk, updates, _ := tieredDB(t, repos, plan)
	return s, gh, clk, updates
}

// tieredDB is tiered that also returns the database.
func tieredDB(t *testing.T, repos []string, plan Plan) (*Syncer, *githubtest.Server, *clock, chan update, *sql.DB) {
	t.Helper()
	gh := githubtest.New(t)
	now := time.Now().UTC().Truncate(time.Second)
	gh.Mu.Lock()
	gh.Repos = repos
	for i, r := range repos {
		gh.Issues = append(gh.Issues, &githubtest.Issue{ID: "I_" + r, Repo: r, Number: i + 1, Title: "t", Author: "alice",
			Open: true, CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-time.Hour)})
	}
	gh.Mu.Unlock()
	clk := &clock{t: now}
	a := github.NewAuth(filepath.Join(t.TempDir(), "secrets"))
	a.WebURL, a.APIURL, a.HTTP, a.Now = gh.URL, gh.URL, gh.Client(), clk.Now
	if _, err := a.ConvertManifest(t.Context(), githubtest.ManifestCode, 1); err != nil {
		t.Fatal(err)
	}
	v, c := github.PKCE()
	if _, err := a.Exchange(t.Context(), gh.IssueCode(c), v, "r"); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	updates := make(chan update, 64)
	s := New(Options{
		Store: store.New(db), Provider: github.NewProvider(a), Log: slog.New(slog.DiscardHandler), Plan: plan, Now: clk.Now,
		OnUpdate: func(evs []store.Event, unread int) { updates <- update{evs, unread} },
	})
	return s, gh, clk, updates, db
}

func drain(ch chan update) []store.Event {
	var out []store.Event
	for {
		select {
		case u := <-ch:
			out = append(out, u.events...)
		default:
			return out
		}
	}
}

func plan5() Plan {
	return Plan{Active: 5 * time.Minute, Idle: 30 * time.Minute, Reconcile: time.Hour, ActiveWindow: 24 * time.Hour, Budget: 1000, Concurrency: 2}
}

func TestNotModifiedCostsNothing(t *testing.T) {
	s, gh, clk, updates := tiered(t, []string{"octo/a", "octo/b"}, plan5())
	s.Step(t.Context()) // baseline reconcile
	drain(updates)
	gql := gh.Hits().GraphQL

	clk.Add(6 * time.Minute)
	s.Step(t.Context()) // first checks: 200, ETags stored
	clk.Add(6 * time.Minute)
	s.Step(t.Context()) // nothing changed: 304 for every request
	h := gh.Hits()
	if h.REST != 8 || h.NotModified != 4 {
		t.Fatalf("hits %+v, want 8 REST with 4 not modified", h)
	}
	if h.GraphQL != gql {
		t.Fatalf("unchanged repos fetched over GraphQL: %d → %d", gql, h.GraphQL)
	}
	if evs := drain(updates); len(evs) != 0 {
		t.Fatalf("events without changes: %+v", evs)
	}
	if st := s.Status(); st.NotModified != 4 || st.Checks != 8 {
		t.Fatalf("status %+v", st)
	}
}

func TestChangeFetchesOnlyTheItem(t *testing.T) {
	s, gh, clk, updates := tiered(t, []string{"octo/a", "octo/b"}, plan5())
	s.Step(t.Context())
	clk.Add(6 * time.Minute)
	s.Step(t.Context())
	drain(updates)
	gql := gh.Hits().GraphQL

	gh.Mu.Lock()
	is := gh.Issues[0] // octo/a#1
	is.Comments = append(is.Comments, githubtest.Comment{ID: "C_9", Author: "bob", Body: "hi", CreatedAt: clk.Now()})
	is.UpdatedAt = clk.Now()
	gh.Mu.Unlock()

	clk.Add(6 * time.Minute)
	s.Step(t.Context())
	evs := drain(updates)
	if len(evs) != 1 || evs[0].Kind != store.EventNewComment || evs[0].Repo != "octo/a" {
		t.Fatalf("events %+v", evs)
	}
	if d := gh.Hits().GraphQL - gql; d != 1 {
		t.Fatalf("GraphQL requests for one changed item: %d, want 1", d)
	}
}

// A PR and a PR comment in the since window (the REST endpoints list both)
// must not fail the change check; the issue changed alongside is still fetched.
func TestPullRequestActivityDoesNotFailTheCheck(t *testing.T) {
	s, gh, clk, updates := tiered(t, []string{"octo/a"}, plan5())
	s.Step(t.Context())
	clk.Add(6 * time.Minute)
	s.Step(t.Context())
	drain(updates)

	gh.Mu.Lock()
	gh.PullRequests = []*githubtest.Issue{{Repo: "octo/a", Number: 5, UpdatedAt: clk.Now(),
		Comments: []githubtest.Comment{{ID: "C_pr", Author: "bob", Body: "lgtm", CreatedAt: clk.Now()}}}}
	is := gh.Issues[0] // octo/a#1
	is.Comments = append(is.Comments, githubtest.Comment{ID: "C_9", Author: "bob", Body: "hi", CreatedAt: clk.Now()})
	is.UpdatedAt = clk.Now()
	gh.Mu.Unlock()

	clk.Add(6 * time.Minute)
	s.Step(t.Context())
	evs := drain(updates)
	if len(evs) != 1 || evs[0].Kind != store.EventNewComment || evs[0].Repo != "octo/a" {
		t.Fatalf("events %+v", evs)
	}
	if st := s.Status(); st.LastError != "" {
		t.Fatalf("status error %q", st.LastError)
	}
}

func TestRateLimitPausesTheSchedule(t *testing.T) {
	s, gh, clk, updates := tiered(t, []string{"octo/a"}, plan5())
	s.Step(t.Context())
	drain(updates)
	gh.FailNext(1, 403, map[string]string{"Retry-After": "600"}, "You have exceeded a secondary rate limit")
	clk.Add(6 * time.Minute)
	s.Step(t.Context())
	st := s.Status()
	if st.RateLimitedUntil == "" {
		t.Fatalf("not paused: %+v", st)
	}
	before := gh.Hits().REST
	clk.Add(5 * time.Minute) // still inside Retry-After
	if wait := s.Step(t.Context()); wait <= 0 || gh.Hits().REST != before {
		t.Fatalf("requests during the pause: %d → %d (wait %v)", before, gh.Hits().REST, wait)
	}
	clk.Add(6 * time.Minute) // past it
	s.Step(t.Context())
	if gh.Hits().REST == before {
		t.Fatal("schedule did not resume after Retry-After")
	}
}

func TestBudgetDefersChecks(t *testing.T) {
	repos := []string{"o/a", "o/b", "o/c", "o/d", "o/e"}
	p := plan5()
	p.Budget = 6 // three change checks (2 requests each) per hour
	s, gh, clk, updates := tiered(t, repos, p)
	s.Step(t.Context())
	drain(updates)
	for range 4 {
		clk.Add(6 * time.Minute)
		s.Step(t.Context())
	}
	if n := gh.Hits().REST; n > 6 {
		t.Fatalf("spent %d requests with a budget of 6/h", n)
	}
	if st := s.Status(); st.BudgetUsed != 6 {
		t.Fatalf("budget used %d, want 6", st.BudgetUsed)
	}
	clk.Add(time.Hour) // the window slides: checks resume (after the hourly reconcile now due)
	before := gh.Hits().REST
	s.Step(t.Context())
	s.Step(t.Context())
	if gh.Hits().REST == before {
		t.Fatal("checks did not resume after the budget window")
	}
}

func TestSetPlanAppliesLive(t *testing.T) {
	s, _, _, _ := tiered(t, []string{"octo/a"}, plan5())
	fast := plan5()
	fast.Mode, fast.Active = "fast", 2*time.Minute
	s.SetPlan(fast)
	if st := s.Status(); st.Mode != "fast" || st.ActiveEvery != "2m0s" {
		t.Fatalf("status %+v", st)
	}
}

func TestPollIntervalHeaderIsHonoured(t *testing.T) {
	s, gh, clk, updates := tiered(t, []string{"octo/a"}, plan5())
	gh.Mu.Lock()
	gh.PollInterval = 1200 // 20 min
	gh.PullRequests = []*githubtest.Issue{{Repo: "octo/a", Number: 99, UpdatedAt: clk.Now().Add(time.Minute)}}
	gh.Mu.Unlock()
	s.Step(t.Context())
	drain(updates)
	clk.Add(6 * time.Minute)
	s.Step(t.Context()) // first check learns X-Poll-Interval; the PR is not fetched
	if h := gh.Hits(); h.REST != 2 {
		t.Fatalf("hits %+v", h)
	}
	gql := gh.Hits().GraphQL
	for range 3 { // 6-minute steps: the next check waits for the 20 minutes
		clk.Add(6 * time.Minute)
		s.Step(t.Context())
	}
	if h := gh.Hits(); h.REST != 2 || h.GraphQL != gql {
		t.Fatalf("checked before X-Poll-Interval elapsed: %+v", h)
	}
	clk.Add(4 * time.Minute)
	s.Step(t.Context())
	if h := gh.Hits(); h.REST != 4 {
		t.Fatalf("no check after X-Poll-Interval: %+v", h)
	}
}
