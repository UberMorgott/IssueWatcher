package api

import (
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/provider/github"
	"github.com/UberMorgott/issuewatcher/internal/provider/github/githubtest"
	"github.com/UberMorgott/issuewatcher/internal/store"
	"github.com/UberMorgott/issuewatcher/internal/syncer"
)

// syncedEnv is signed in and has synced two issues, then sees a new comment.
func syncedEnv(t *testing.T, extra ...func(*Options)) *env {
	t.Helper()
	e := newEnv(t, extra...)
	if _, err := e.auth.ConvertManifest(t.Context(), githubtest.ManifestCode, e.s.Port()); err != nil {
		t.Fatal(err)
	}
	v, c := github.PKCE()
	if _, err := e.auth.Exchange(t.Context(), e.gh.IssueCode(c), v, "r"); err != nil {
		t.Fatal(err)
	}
	t0 := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Second)
	e.gh.Mu.Lock()
	e.gh.Repos = []string{"octo/app"}
	e.gh.Issues = []*githubtest.Issue{
		{ID: "I_1", Repo: "octo/app", Number: 1, Title: "crash on start", Author: "alice", Open: true,
			Labels: []string{"bug"}, CreatedAt: t0, UpdatedAt: t0},
		{ID: "I_2", Repo: "octo/app", Number: 2, Title: "dark mode", Author: "bob", Open: false,
			CreatedAt: t0, UpdatedAt: t0, ClosedAt: t0},
	}
	e.gh.Mu.Unlock()
	if err := e.sync.SyncOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	e.gh.Mu.Lock()
	e.gh.Issues[0].Comments = []githubtest.Comment{{ID: "C_1", Author: "carol", Body: "same here", CreatedAt: t0}}
	e.gh.Issues[0].UpdatedAt = t0.Add(time.Hour)
	e.gh.Mu.Unlock()
	if err := e.sync.SyncOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	return e
}

func TestReposIssuesAndDetail(t *testing.T) {
	e := syncedEnv(t)
	var repos []store.Repo
	if code := e.call(t, http.MethodGet, "/api/projects", "", &repos); code != http.StatusOK || len(repos) != 1 ||
		repos[0].Name != "octo/app" || repos[0].Open != 1 || repos[0].Closed != 1 || repos[0].Unread != 1 {
		t.Fatalf("repos %d %+v", code, repos)
	}
	cases := map[string][]int{
		"/api/items":                                            {1, 2},
		"/api/items?state=open":                                 {1},
		"/api/items?state=closed":                               {2},
		"/api/items?label=bug":                                  {1},
		"/api/items?q=dark":                                     {2},
		"/api/items?unread=1":                                   {1},
		"/api/items?repo=" + strconv.Itoa(99):                   {},
		"/api/items?limit=1&state=all":                          {1},
		"/api/items?repo=" + strconv.FormatInt(repos[0].ID, 10): {1, 2},
	}
	for path, want := range cases {
		var page store.IssueChunk
		if code := e.call(t, http.MethodGet, path, "", &page); code != http.StatusOK || len(page.Items) != len(want) {
			t.Errorf("%s: %d %+v, want %v", path, code, page.Items, want)
			continue
		}
		for i, n := range want {
			if page.Items[i].Number != n {
				t.Errorf("%s: item %d = #%d, want #%d", path, i, page.Items[i].Number, n)
			}
		}
	}
	var labels []string
	if code := e.call(t, http.MethodGet, "/api/items/labels?kind=issue", "", &labels); code != http.StatusOK || len(labels) == 0 {
		t.Errorf("labels: %d %v", code, labels)
	}
	for _, bad := range []string{"/api/items/labels?kind=x", "/api/items?state=weird", "/api/items?limit=-1", "/api/items?repo=x", "/api/items?cursor=zz", "/api/items?ids=1,x", "/api/projects?limit=5&sort=bogus"} {
		if code := e.call(t, http.MethodGet, bad, "", nil); code != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400", bad, code)
		}
	}

	var page store.IssueChunk
	e.call(t, http.MethodGet, "/api/items?q=%231", "", &page)
	id := strconv.FormatInt(page.Items[0].ID, 10)
	var d store.IssueDetail
	if code := e.call(t, http.MethodGet, "/api/items/"+id, "", &d); code != http.StatusOK || d.Comments != 1 || !d.Unread {
		t.Fatalf("detail %d %+v", code, d)
	}
	var cc store.CommentChunk
	if code := e.call(t, http.MethodGet, "/api/items/"+id+"/comments?limit=10", "", &cc); code != http.StatusOK ||
		len(cc.Items) != 1 || cc.Items[0].Author != "carol" || cc.More {
		t.Fatalf("comments %d %+v", code, cc)
	}
	if code := e.call(t, http.MethodGet, "/api/items/9999", "", nil); code != http.StatusNotFound {
		t.Fatalf("missing issue: %d", code)
	}

	// Mark read → badge callback, unread gone.
	if code := e.call(t, http.MethodPost, "/api/items/"+id+"/read", "", nil); code != http.StatusNoContent {
		t.Fatalf("read: %d", code)
	}
	<-e.unread
	e.call(t, http.MethodGet, "/api/items/"+id, "", &d)
	if d.Unread {
		t.Fatal("still unread")
	}

	// Bulk: unread again, read (only the rows that changed count), resolve (comment threads only).
	var res struct{ Changed int }
	if code := e.call(t, http.MethodPost, "/api/items/read", `{"ids":[`+id+`],"unread":true}`, &res); code != http.StatusOK || res.Changed != 1 {
		t.Fatalf("bulk unread: %d %+v", code, res)
	}
	<-e.unread
	if code := e.call(t, http.MethodPost, "/api/items/read", `{"ids":[`+id+`,9999]}`, &res); code != http.StatusOK || res.Changed != 1 {
		t.Fatalf("bulk read: %d %+v", code, res)
	}
	<-e.unread
	if code := e.call(t, http.MethodPost, "/api/items/resolve", `{"ids":[`+id+`],"resolved":true}`, &res); code != http.StatusOK || res.Changed != 0 {
		t.Fatalf("resolve an issue: %d %+v", code, res)
	}
	for _, bad := range []string{`{}`, `{"ids":[]}`, `{"ids":[1],"unread":"x"}`, `nope`} {
		if code := e.call(t, http.MethodPost, "/api/items/read", bad, nil); code != http.StatusBadRequest {
			t.Errorf("bulk read %s: %d, want 400", bad, code)
		}
	}
}

func TestReplyPostsToGitHub(t *testing.T) {
	e := syncedEnv(t)
	var page store.IssueChunk
	e.call(t, http.MethodGet, "/api/items?q=%231", "", &page)
	path := "/api/items/" + strconv.FormatInt(page.Items[0].ID, 10) + "/comments"

	var c store.Comment
	if code := e.call(t, http.MethodPost, path, `{"body":"fixed in 1.2"}`, &c); code != http.StatusCreated ||
		c.Body != "fixed in 1.2" || c.Author != githubtest.Login {
		t.Fatalf("reply %d %+v", code, c)
	}
	e.gh.Mu.Lock()
	n := len(e.gh.Issues[0].Comments)
	e.gh.Mu.Unlock()
	if n != 2 {
		t.Fatalf("fake GitHub has %d comments, want 2", n)
	}
	if code := e.call(t, http.MethodPost, path, `{"body":"  "}`, nil); code != http.StatusBadRequest {
		t.Fatalf("empty reply: %d", code)
	}
	if code := e.call(t, http.MethodPost, "/api/items/9999/comments", `{"body":"x"}`, nil); code != http.StatusNotFound {
		t.Fatalf("reply to missing: %d", code)
	}
	if err := e.auth.Logout(); err != nil {
		t.Fatal(err)
	}
	var refused struct{ Code, Platform, Error string }
	if code := e.callAny(t, http.MethodPost, path, `{"body":"x"}`, &refused); code != http.StatusConflict ||
		refused.Code != "not_signed_in" || refused.Platform != "github" || refused.Error != "not signed in to github" {
		t.Fatalf("reply signed out: %d %+v", code, refused)
	}
	if code := e.call(t, http.MethodGet, "/api/items?kind=comment", "", &page); code != http.StatusOK || len(page.Items) != 0 {
		t.Fatalf("kind=comment: %d %d items", code, len(page.Items))
	}
	if code := e.call(t, http.MethodGet, "/api/items?kind=issue", "", &page); code != http.StatusOK || len(page.Items) == 0 ||
		page.Items[0].Kind != store.KindIssue || page.Items[0].Platform != "github" {
		t.Fatalf("kind=issue: %d %+v", code, page.Items)
	}
	if code := e.call(t, http.MethodGet, "/api/items?kind=issue,bug", "", &page); code != http.StatusOK || len(page.Items) == 0 {
		t.Fatalf("kind=issue,bug: %d %d items", code, len(page.Items))
	}
	if code := e.call(t, http.MethodGet, "/api/items?kind=issue,x", "", nil); code != http.StatusBadRequest {
		t.Fatalf("bad kind in list: %d", code)
	}
	if code := e.call(t, http.MethodGet, "/api/items?kind=x", "", nil); code != http.StatusBadRequest {
		t.Fatalf("bad kind: %d", code)
	}
}

func TestContractRoutes(t *testing.T) {
	e := syncedEnv(t)
	var projects []store.Repo
	if code := e.call(t, http.MethodGet, "/api/projects", "", &projects); code != http.StatusOK || len(projects) != 1 ||
		projects[0].LastSync == "" || projects[0].Platform != "github" {
		t.Fatalf("projects %d %+v", code, projects)
	}
	pid := strconv.FormatInt(projects[0].ID, 10)
	for path, want := range map[string]int{
		"/api/items?project=" + pid + "&limit=1": 1,
		"/api/items?source=github&state=open":    1,
		"/api/items?source=steam":                0,
	} {
		var page store.IssueChunk
		if code := e.call(t, http.MethodGet, path, "", &page); code != http.StatusOK || len(page.Items) != want {
			t.Errorf("%s: %d %d items, want %d", path, code, len(page.Items), want)
		}
	}
	var page store.IssueChunk
	e.call(t, http.MethodGet, "/api/items?q=%231", "", &page)
	id := strconv.FormatInt(page.Items[0].ID, 10)
	var d store.IssueDetail
	if code := e.call(t, http.MethodGet, "/api/items/"+id, "", &d); code != http.StatusOK || d.Comments != 1 {
		t.Fatalf("item %d %+v", code, d)
	}
	var c store.Comment
	if code := e.call(t, http.MethodPost, "/api/items/"+id+"/comments", `{"body":"ok"}`, &c); code != http.StatusCreated {
		t.Fatalf("item reply %d", code)
	}
	if code := e.call(t, http.MethodPost, "/api/items/"+id+"/read", "", nil); code != http.StatusNoContent {
		t.Fatalf("item read %d", code)
	}
	<-e.unread
	var st statsResponse
	if code := e.call(t, http.MethodGet, "/api/stats?project="+pid, "", &st); code != http.StatusOK || st.Open != 1 || len(st.Projects) != 0 {
		t.Fatalf("project stats %d %+v", code, st)
	}
	e.call(t, http.MethodGet, "/api/stats", "", &st)
	if len(st.Projects) != 1 || len(st.Repos) != 1 {
		t.Fatalf("global stats projects %+v", st.Projects)
	}
}

func TestStatsAndSync(t *testing.T) {
	e := syncedEnv(t)
	var st statsResponse
	if code := e.call(t, http.MethodGet, "/api/stats?weeks=4", "", &st); code != http.StatusOK ||
		st.Open != 1 || st.Closed != 1 || len(st.Weekly) != 4 || len(st.Repos) != 1 {
		t.Fatalf("stats %d %+v", code, st)
	}
	opened, closed := 0, 0
	for _, w := range st.Weekly {
		opened, closed = opened+w.Opened, closed+w.Closed
	}
	if opened != 2 || closed != 1 {
		t.Fatalf("weekly sums opened %d closed %d", opened, closed)
	}
	var repoStats statsResponse
	e.call(t, http.MethodGet, "/api/stats?repo="+strconv.FormatInt(st.Repos[0].ID, 10), "", &repoStats)
	if repoStats.Open != 1 || len(repoStats.Repos) != 0 || len(repoStats.Weekly) != 26 {
		t.Fatalf("repo stats %+v", repoStats)
	}

	var ss syncer.Status
	if code := e.call(t, http.MethodGet, "/api/sync", "", &ss); code != http.StatusOK || !ss.SignedIn || ss.LastSync == "" {
		t.Fatalf("sync status %d %+v", code, ss)
	}
	if code := e.call(t, http.MethodPost, "/api/sync", "", nil); code != http.StatusAccepted {
		t.Fatalf("sync now: %d", code)
	}
}

func TestDataEndpointsNeedSession(t *testing.T) {
	e := newEnv(t)
	for _, p := range []string{"/api/projects", "/api/items", "/api/stats", "/api/sync", "/api/auth/status"} {
		if r := get(t, &http.Client{}, e.s.BaseURL()+p, ""); r.status != http.StatusUnauthorized {
			t.Errorf("%s without session: %d", p, r.status)
		}
	}
}
