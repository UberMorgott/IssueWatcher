package nexus

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/browser"
	"github.com/UberMorgott/issuewatcher/internal/provider"
	"github.com/UberMorgott/issuewatcher/internal/signin"
	"github.com/UberMorgott/issuewatcher/internal/store"
	"github.com/UberMorgott/issuewatcher/internal/websession"
)

func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name)) //nolint:gosec // G304: fixed test fixtures
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// fakeSite answers the browser's in-page fetches of www.nexusmods.com.
type fakeSite struct {
	t        *testing.T
	mu       sync.Mutex
	comments map[int]string // page → widget html
	bugs     string
	bugPosts map[string]string // issue id → reply list html
	fail     map[string]int    // URL substring → HTTP status
	calls    []browser.Request
	onPost   func(req browser.Request) (browser.Response, error)
}

func (f *fakeSite) Fetch(_ context.Context, req browser.Request) (browser.Response, error) {
	f.mu.Lock()
	f.calls = append(f.calls, req)
	fail, onPost := f.fail, f.onPost
	f.mu.Unlock()
	for k, st := range fail {
		if strings.Contains(req.URL, k) {
			return browser.Response{Status: st, URL: req.URL, Body: "boom"}, nil
		}
	}
	u, _ := url.Parse(req.URL)
	ok := func(body string) (browser.Response, error) {
		return browser.Response{Status: 200, URL: req.URL, Body: body}, nil
	}
	switch {
	case req.Method == http.MethodPost && (u.Path == "/mod/comment" || strings.HasPrefix(u.Path, "/mod_bug/")):
		return onPost(req)
	case strings.HasSuffix(u.Path, "/mods/147"):
		return ok(`<div data-target="/Core/Libs/Common/Widgets/CommentContainer?game_id=8851&amp;object_id=147&amp;object_type=1&amp;thread_id=16796543"></div>`)
	case strings.Contains(req.URL, "CommentContainer"):
		var page int
		_, _ = fmt.Sscanf(req.URL[strings.LastIndex(req.URL, "page:")+5:], "%d", &page)
		f.mu.Lock()
		body, found := f.comments[page]
		f.mu.Unlock()
		if !found {
			body = f.comments[1]
		}
		return ok(body)
	case strings.Contains(req.URL, "ModBugsTab"):
		return ok(f.bugs)
	case strings.Contains(req.URL, "ModBugReplyList"):
		form, _ := url.ParseQuery(req.Body)
		f.mu.Lock()
		body, found := f.bugPosts[form.Get("issue_id")]
		f.mu.Unlock()
		if !found {
			body = fixture(f.t, "bug_deleted.html")
		}
		return ok(body)
	}
	return browser.Response{Status: 404, URL: req.URL}, nil
}

// gqlServer is the keyless v2 GraphQL + the api-router.
func gqlServer(t *testing.T, signedIn func(cookie string) bool) (*httptest.Server, *http.Client) {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var q struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		_ = json.NewDecoder(r.Body).Decode(&q)
		switch {
		case strings.Contains(q.Query, "game(domainName"):
			_, _ = w.Write([]byte(`{"data":{"game":{"id":8851}}}`))
		case strings.Contains(q.Query, "mods(filter"):
			_, _ = w.Write([]byte(`{"data":{"mods":{"totalCount":1,"nodes":[{"modId":147,"name":"ShareShip","description":"src: https://github.com/UberMorgott/ShareShip","uploader":{"name":"UberMorgott","memberId":1234},"game":{"domainName":"windrose"}}]}}}`))
		case strings.Contains(q.Query, "preferences"):
			if signedIn == nil || !signedIn(r.Header.Get("Cookie")) {
				_, _ = w.Write([]byte(`{"errors":[{"message":"Unauthorized","extensions":{"code":"UNAUTHORIZED"}}],"data":{"preferences":null}}`))
				return
			}
			_, _ = w.Write([]byte(`{"data":{"preferences":{"id":"Z2lkOi8vYXBpL01lbWJlcnNQcmVmZXJlbmNlLzEyMzQ="}}}`)) // gid://api/MembersPreference/1234
		case strings.Contains(q.Query, "user(id"):
			_, _ = w.Write([]byte(`{"data":{"user":{"memberId":1234,"name":"UberMorgott"}}}`))
		default:
			http.Error(w, "unknown query", 400)
		}
	}))
	t.Cleanup(srv.Close)
	hc := srv.Client()
	if tr, ok := hc.Transport.(*http.Transport); ok {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // G402: httptest certificate
	}
	return srv, hc
}

func newFakeNative(t *testing.T) (*Provider, *fakeSite) {
	t.Helper()
	fs := &fakeSite{t: t, comments: map[int]string{1: fixture(t, "comments_1.html"), 2: fixture(t, "comments_2.html")},
		bugs: fixture(t, "bugs_1.html"), bugPosts: map[string]string{"1062068": fixture(t, "bug_0.html"), "958735": fixture(t, "bug_1.html")}}
	srv, hc := gqlServer(t, nil)
	p := New(Options{Native: &NativeOptions{Browser: fs, HTTP: hc, GraphQL: srv.URL + "/v2/graphql", APIRouter: srv.URL + "/graphql"},
		Author: func() string { return "UberMorgott" }})
	return p, fs
}

func TestParseCommentsFixture(t *testing.T) {
	cp, err := parseComments(fixture(t, "comments_1.html"))
	if err != nil {
		t.Fatal(err)
	}
	if cp.Page != 1 || cp.Pages != 2 || cp.Total != 32 || len(cp.Comments) != 11 {
		t.Fatalf("page %d/%d total %d threads %d", cp.Page, cp.Pages, cp.Total, len(cp.Comments))
	}
	sticky := cp.Comments[0]
	if sticky.ID != "168798341" || sticky.Author != "UberMorgott" || !sticky.Sticky || !sticky.Locked ||
		*sticky.CreatedAt != "2026-04-26T10:10:25.000Z" || !strings.HasPrefix(sticky.Body, "UPDATE 2026-04-27: real cause found") {
		t.Fatalf("sticky = %+v", sticky)
	}
	long := cp.Comments[6]
	if long.ID != "168629942" || len(long.Replies) != 9 || long.Replies[0].CreatedAt == nil {
		t.Fatalf("thread with replies = %s, %d replies", long.ID, len(long.Replies))
	}
	for _, c := range cp.Comments {
		if strings.Contains(c.Body, "<") && strings.Contains(c.Body, "</") {
			t.Fatalf("markup left in body %s: %q", c.ID, c.Body)
		}
	}
}

func TestParseBugsFixture(t *testing.T) {
	r, enabled, err := parseBugs(fixture(t, "bugs_1.html"), "list", 1)
	if err != nil || !enabled || r.Pages != 38 || len(r.Bugs) != 10 {
		t.Fatalf("bugs: %v enabled=%v pages=%d n=%d", err, enabled, r.Pages, len(r.Bugs))
	}
	b := r.Bugs[0]
	if b.ID != "1062068" || b.Status != "Not a bug" || b.StatusKey == nil || *b.StatusKey != "not_a_bug" || b.Open ||
		b.Replies != 5 || *b.LastPostAt != "2026-04-03T17:15:57.000Z" {
		t.Fatalf("bug row = %+v", b)
	}
	if fixed := r.Bugs[3]; *fixed.StatusKey != "fixed" || fixed.Open {
		t.Fatalf("fixed row = %+v", fixed)
	}
	bp, err := parseBugReplies(fixture(t, "bug_0.html"))
	if err != nil || len(bp.posts) != 6 {
		t.Fatalf("bug posts: %v %d", err, len(bp.posts))
	}
	res := bp.result()
	if res.Report.ID != "1062068" || res.Report.Author != "PumaArts" || res.Report.CreatedAtLocal == nil || len(res.Replies) != 5 ||
		res.Replies[0].ID != "5237906" || !strings.Contains(res.Replies[0].Body, "\n") {
		t.Fatalf("bug = %+v", res)
	}
	del, _ := parseBugReplies(fixture(t, "bug_deleted.html"))
	if len(del.posts) != 1 || del.posts[0].ID != "" {
		t.Fatalf("deleted bug tile = %+v", del.posts)
	}
	for in, want := range map[string]string{"New issue": "new", "Being looked at": "looking", "Won't fix": "wont_fix", "Need more info": "need_info", "Known issue": "known", "odd": ""} {
		if got := bugStatusKey(in); got != want {
			t.Errorf("%q → %q", in, got)
		}
	}
}

func TestNativeSyncItems(t *testing.T) {
	p, fs := newFakeNative(t)
	ctx := context.Background()
	projects, err := p.ListProjects(ctx)
	if err != nil || len(projects) != 1 || projects[0].ExternalID != "windrose/147" ||
		projects[0].URL != "https://www.nexusmods.com/windrose/mods/147" || projects[0].CodeURL != "https://github.com/UberMorgott/ShareShip" {
		t.Fatalf("projects = %+v, %v", projects, err)
	}
	if acc, err := p.Account(ctx); err != nil || acc != "UberMorgott" {
		t.Fatalf("account = %q, %v", acc, err)
	}
	items, err := p.SyncItems(ctx, projects[0], time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	var comments, bugs int
	ids := map[string]bool{}
	for _, it := range items {
		if ids[it.ExternalID] {
			t.Fatalf("duplicate item %s (shifted pages must dedupe)", it.ExternalID)
		}
		ids[it.ExternalID] = true
		switch it.Kind {
		case store.KindComment:
			comments++
		case store.KindBug:
			bugs++
		}
	}
	// 11 threads on page 1 + 1 new on page 2 (the sticky repeats), 10 bug rows,
	// 8 of them deleted (no reply list in the fake).
	if comments != 12 || bugs != 2 {
		t.Fatalf("comments %d bugs %d", comments, bugs)
	}
	sticky := findItem(items, "comment:windrose/147/168798341")
	if sticky == nil || sticky.CreatedAt != time.Date(2026, 4, 26, 10, 10, 25, 0, time.UTC) || sticky.URL != commentURL("windrose", 147, "168798341") {
		t.Fatalf("sticky item = %+v", sticky)
	}
	bug := findItem(items, "bug:1062068")
	if bug == nil || bug.Open || bug.RawStatus != "not_a_bug" || bug.Author != "PumaArts" || len(bug.Comments) != 5 ||
		bug.URL != "https://www.nexusmods.com/windrose/mods/147?tab=bugs" {
		t.Fatalf("bug item = %+v", bug)
	}
	// The thread id is read from the mod page once.
	pages := 0
	for _, c := range fs.calls {
		if strings.HasSuffix(c.URL, "/mods/147") {
			pages++
		}
		if c.Headers["X-Requested-With"] != "XMLHttpRequest" {
			t.Fatalf("request without the XHR header: %s", c.URL)
		}
	}
	if pages != 1 {
		t.Fatalf("mod page read %d times", pages)
	}
	var st provider.PollState
	if _, err := p.DetectChanges(ctx, projects[0], &st); err != nil {
		t.Fatal(err)
	}
	if st.ETags["v2:nexus:page1"] == "" || st.ETags["nexus:page1"] != "" {
		t.Fatalf("fingerprint keys = %v", st.ETags)
	}
}

func TestNativeErrorMidListingFails(t *testing.T) {
	p, fs := newFakeNative(t)
	fs.fail = map[string]int{"page:2": 500}
	_, err := p.SyncItems(context.Background(), provider.Project{ExternalID: "windrose/147"}, time.Time{})
	if err == nil || !strings.Contains(err.Error(), "comments page 2") {
		t.Fatalf("err = %v (a failed page must fail the read: the syncer keeps its cursor)", err)
	}
	fs.fail = map[string]int{"CommentContainer": 429}
	var rl *provider.RateLimitError
	if _, err := p.SyncItems(context.Background(), provider.Project{ExternalID: "windrose/147"}, time.Time{}); !errors.As(err, &rl) {
		t.Fatalf("429: err = %v", err)
	}
}

func TestNativeAccountFromSession(t *testing.T) {
	fs := &fakeSite{t: t}
	srv, hc := gqlServer(t, func(cookie string) bool { return strings.Contains(cookie, "nexusmods_session=ok") })
	jar, _ := websession.Open(filepath.Join(t.TempDir(), "nexus.json"))
	host := strings.TrimPrefix(srv.URL, "https://")
	host, _, _ = strings.Cut(host, ":")
	spec := SignInSpec(hc, nil)
	n := &native{opts: NativeOptions{HTTP: hc, GraphQL: srv.URL + "/v2/graphql", APIRouter: srv.URL + "/graphql"}}
	n.fill()
	spec.Probe = func(ctx context.Context, j *websession.Jar) (string, error) {
		m, err := n.whoAmI(ctx, j)
		return m.name, err
	}
	_ = jar.Replace([]websession.Cookie{{Name: "nexusmods_session", Value: "ok", Domain: host, Path: "/", Secure: true}}, "UA", "", signin.SourceWindow)
	mgr := signin.New(spec, jar, nil)
	p := New(Options{Native: &NativeOptions{Browser: fs, HTTP: hc, GraphQL: srv.URL + "/v2/graphql", APIRouter: srv.URL + "/graphql", Session: mgr}})
	l, err := p.Login(context.Background())
	if err != nil || !l.LoggedIn || l.Account != "UberMorgott" {
		t.Fatalf("login = %+v, %v", l, err)
	}
	if acc, err := p.Account(context.Background()); err != nil || acc != "UberMorgott" {
		t.Fatalf("account = %q, %v", acc, err)
	}
	if _, err := n.whoAmI(context.Background(), websession.Memory(nil, "")); !errors.Is(err, provider.ErrNotSignedIn) {
		t.Fatalf("signed out: err = %v", err)
	}
	if id := memberIDFromGID("Z2lkOi8vYXBpL01lbWJlcnNQcmVmZXJlbmNlLzEyMzQ="); id != 1234 {
		t.Fatalf("gid = %d", id)
	}
}

func findItem(items []provider.Item, id string) *provider.Item {
	for i := range items {
		if items[i].ExternalID == id {
			return &items[i]
		}
	}
	return nil
}
