package factorio

import (
	"context"
	"crypto/tls"
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

func TestParseUserAndList(t *testing.T) {
	mods, last, err := parseUserMods(fixture(t, "user.html"))
	if err != nil || last != 1 || len(mods) != 2 || mods[0] != (modRef{"auto-build-and-deconstruct", "Lazy Builder"}) || mods[1].Name != "Turrets-need-energy-source" {
		t.Fatalf("mods = %+v, last %d, %v", mods, last, err)
	}
	rows, last, err := parseList(fixture(t, "list_1.html"))
	if err != nil || last != 1 || len(rows) != 27 {
		t.Fatalf("rows = %d, last %d, %v", len(rows), last, err)
	}
	r := rows[0]
	if r != (row{ID: "6ab8e16ca3e387a150232548", Title: "Wires", Category: "Bugs", Author: "dzbanek", Posted: "2026-09-27T09:27:08.720000",
		Replies: 1, LastBy: "Morgott", Last: "2026-09-28T21:08:52.319000"}) {
		t.Fatalf("row = %+v", r)
	}
	cats := map[string]int{}
	for _, r := range rows {
		cats[r.Category]++
	}
	if cats["Bugs"] != 15 || cats["General"] != 8 || cats["Ideas & suggestions"] != 4 {
		t.Fatalf("categories = %v", cats)
	}
	paged, last, err := parseList(fixture(t, "list_paged.html"))
	if err != nil || len(paged) != 50 || last != 10 {
		t.Fatalf("paged: %d rows, last %d, %v", len(paged), last, err)
	}
}

func TestParseThread(t *testing.T) {
	msgs, form, err := parseThread(fixture(t, "thread.html"))
	if err != nil || len(msgs) != 2 {
		t.Fatalf("msgs = %+v, %v", msgs, err)
	}
	if msgs[0].Author != "dzbanek" || msgs[0].At != "2026-09-27T09:27:08.720000" || msgs[0].Owner ||
		msgs[0].Body != "Place 2 poles and connect them with a wire.\n\nCtrl+c\n\nCrlt+v\n\nThere are just poles, not wires between them." {
		t.Fatalf("first = %+v", msgs[0])
	}
	if msgs[1].Author != "Morgott" || !msgs[1].Owner || msgs[1].Body != "hi!\n\nFixed" || msgs[1].At != "2026-09-28T21:08:52.316000" {
		t.Fatalf("owner reply = %+v", msgs[1])
	}
	if !form.Disabled || form.Method != "post" {
		t.Fatalf("signed-out form = %+v", form)
	}
	it := threadItem("auto-build-and-deconstruct", row{ID: "6ab8e16ca3e387a150232548", Title: "Wires", Category: "Bugs", Author: "dzbanek",
		Posted: "2026-09-27T09:27:08.720000", Last: "2026-09-28T21:08:52.319000"}, msgs)
	if it.ExternalID != "thread:6ab8e16ca3e387a150232548" || it.Kind != store.KindBug || !it.Open || it.RawStatus != "Bugs" ||
		it.CreatedAt != time.Date(2026, 9, 27, 9, 27, 8, 720000000, time.UTC) || len(it.Comments) != 1 ||
		it.Comments[0].ExternalID != "6ab8e16ca3e387a150232548/2026-09-28T21:08:52.316000" ||
		it.URL != "https://mods.factorio.com/mod/auto-build-and-deconstruct/discussion/6ab8e16ca3e387a150232548" {
		t.Fatalf("item = %+v", it)
	}
}

func TestIdenticalTimestamps(t *testing.T) {
	msgs := []message{{Author: "a", At: "2026-09-01T10:00:00.000000", Body: "q"},
		{Author: "b", At: "2026-09-01T10:05:00.000000", Body: "x"}, {Author: "c", At: "2026-09-01T10:05:00.000000", Body: "y"},
		{Author: "d", At: "2026-09-01T10:05:00.000000", Body: "z"}}
	it := threadItem("m", row{ID: "aaaaaaaaaaaaaaaaaaaaaaaa", Category: "General"}, msgs)
	var ids []string
	for _, c := range it.Comments {
		ids = append(ids, c.ExternalID)
	}
	want := "aaaaaaaaaaaaaaaaaaaaaaaa/2026-09-01T10:05:00.000000,aaaaaaaaaaaaaaaaaaaaaaaa/2026-09-01T10:05:00.000000#2,aaaaaaaaaaaaaaaaaaaaaaaa/2026-09-01T10:05:00.000000#3"
	if strings.Join(ids, ",") != want || it.Kind != store.KindComment {
		t.Fatalf("ids = %v", ids)
	}
}

// portal is a fake mods.factorio.com.
type portal struct {
	t   *testing.T
	srv *httptest.Server
	hc  *http.Client

	mu       sync.Mutex
	list     map[int]string    // page → list html
	threads  map[string]string // id → thread html
	hits     map[string]int
	posts    []url.Values
	onPost   func(w http.ResponseWriter, form url.Values)
	signedIn bool
}

func newPortal(t *testing.T) *portal {
	p := &portal{t: t, list: map[int]string{1: fixture(t, "list_1.html")}, threads: map[string]string{}, hits: map[string]int{}}
	p.srv = httptest.NewTLSServer(http.HandlerFunc(p.serve))
	t.Cleanup(p.srv.Close)
	p.hc = p.srv.Client()
	if tr, ok := p.hc.Transport.(*http.Transport); ok {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // G402: httptest certificate
	}
	return p
}

func (p *portal) serve(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	p.hits[r.URL.Path]++
	p.mu.Unlock()
	path := r.URL.Path
	switch {
	case path == "/user/Morgott":
		_, _ = w.Write([]byte(fixture(p.t, "user.html")))
	case path == "/api/mods/auto-build-and-deconstruct/full":
		_, _ = w.Write([]byte(`{"name":"auto-build-and-deconstruct","title":"Lazy Builder","source_url":"https://github.com/UberMorgott/Factorio-Mod-Lazy_Builder"}`))
	case strings.HasPrefix(path, "/api/mods/"):
		http.NotFound(w, r)
	case path == "/":
		body := `<div class="header"><a href="/login">Log in</a></div><div class="container"></div>`
		if p.signedIn && strings.Contains(r.Header.Get("Cookie"), "session=ok") {
			body = `<div class="header"><a href="/user/Morgott">Morgott</a> <a href="/logout">Log out</a></div><div class="container"></div>`
		}
		_, _ = w.Write([]byte(body))
	case strings.HasSuffix(path, "/discussion") || strings.Contains(path, "/discussion/page/"):
		page := 1
		if _, n, ok := strings.CutLast(path, "/page/"); ok {
			_, _ = fmt.Sscanf(n, "%d", &page)
		}
		p.mu.Lock()
		body, ok := p.list[page]
		p.mu.Unlock()
		if !ok {
			body = `<table></table>`
		}
		_, _ = w.Write([]byte(body))
	case strings.Contains(path, "/discussion/") && r.Method == http.MethodPost:
		_ = r.ParseForm()
		p.mu.Lock()
		p.posts = append(p.posts, r.PostForm)
		on := p.onPost
		p.mu.Unlock()
		on(w, r.PostForm)
	case strings.Contains(path, "/discussion/"):
		id := path[strings.LastIndex(path, "/")+1:]
		p.mu.Lock()
		body, ok := p.threads[id]
		p.mu.Unlock()
		if !ok {
			body = fixture(p.t, "thread.html")
		}
		_, _ = w.Write([]byte(body))
	default:
		http.NotFound(w, r)
	}
}

func (p *portal) provider(opts Options) *Provider {
	opts.HTTP, opts.Site = p.hc, p.srv.URL
	if opts.Author == nil {
		opts.Author = func() string { return "Morgott" }
	}
	return New(opts)
}

func TestListProjectsAndSync(t *testing.T) {
	pt := newPortal(t)
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	clock := now
	p := pt.provider(Options{Now: func() time.Time { return clock }})
	ctx := context.Background()
	projects, err := p.ListProjects(ctx)
	if err != nil || len(projects) != 2 {
		t.Fatalf("projects = %+v, %v", projects, err)
	}
	lb := projects[0]
	if lb.ExternalID != "auto-build-and-deconstruct" || lb.Name != "Lazy Builder" || lb.Game != "factorio" ||
		lb.CodeURL != "https://github.com/UberMorgott/Factorio-Mod-Lazy_Builder" || lb.URL != "https://mods.factorio.com/mod/auto-build-and-deconstruct" {
		t.Fatalf("project = %+v", lb)
	}
	if projects[1].CodeURL != "" || projects[1].Name != "Turrets need energy source" && projects[1].Name == "" {
		t.Fatalf("second project = %+v", projects[1])
	}
	items, err := p.SyncItems(ctx, lb, time.Time{})
	if err != nil || len(items) != 27 {
		t.Fatalf("items = %d, %v", len(items), err)
	}
	// Delta: only threads whose last message is at or after the cursor
	// (the list's newest is 2026-09-28T21:08:52.319).
	pt.mu.Lock()
	pt.hits = map[string]int{}
	pt.mu.Unlock()
	clock = now.Add(10 * time.Minute) // within FullEvery: a real delta
	cursor := time.Date(2026, 9, 28, 21, 8, 52, 316000000, time.UTC)
	items, err = p.SyncItems(ctx, lb, cursor)
	if err != nil {
		t.Fatal(err)
	}
	threads := 0
	for path, n := range pt.hits {
		if threadHrefRe.MatchString(path) {
			threads += n
		}
	}
	if len(items) != 1 || items[0].ExternalID != "thread:6ab8e16ca3e387a150232548" || threads != 1 {
		t.Fatalf("delta items = %d (thread reads %d)", len(items), threads)
	}
	// The hourly sweep reads everything again.
	clock = now.Add(2 * time.Hour)
	if items, _ = p.SyncItems(ctx, lb, cursor); len(items) != 27 {
		t.Fatalf("sweep items = %d", len(items))
	}
	var st provider.PollState
	if _, err := p.DetectChanges(ctx, lb, &st); err != nil || st.ETags[sigKey] == "" {
		t.Fatalf("fingerprint %v %v", st.ETags, err)
	}
}

func TestShiftedPagesAndErrors(t *testing.T) {
	pt := newPortal(t)
	pt.list[2] = fixture(t, "list_1.html") // page 2 repeats page 1 (a shift): deduped
	pt.list[1] = strings.Replace(pt.list[1], `<a href="/mod/auto-build-and-deconstruct/discussion/g">`, `<a href="/mod/x/discussion/page/2"></a><a href="/mod/auto-build-and-deconstruct/discussion/g">`, 1)
	p := pt.provider(Options{})
	items, err := p.SyncItems(context.Background(), provider.Project{ExternalID: "auto-build-and-deconstruct"}, time.Time{})
	if err != nil || len(items) != 27 {
		t.Fatalf("items = %d, %v", len(items), err)
	}
	pt.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTooManyRequests) })
	var rl *provider.RateLimitError
	if _, err := p.SyncItems(context.Background(), provider.Project{ExternalID: "auto-build-and-deconstruct"}, time.Time{}); !errors.As(err, &rl) {
		t.Fatalf("429: %v", err)
	}
}

func TestAccountAndSignIn(t *testing.T) {
	pt := newPortal(t)
	p := pt.provider(Options{Author: func() string { return "" }})
	if _, err := p.Account(context.Background()); !errors.Is(err, provider.ErrNotSignedIn) {
		t.Fatalf("no author, no session: %v", err)
	}
	pt.signedIn = true
	host, _, _ := strings.Cut(strings.TrimPrefix(pt.srv.URL, "https://"), ":")
	jar := websession.Memory([]websession.Cookie{{Name: "session", Value: "ok", Domain: host, Path: "/"}}, "UA")
	if u, err := p.whoAmI(context.Background(), jar); err != nil || u != "Morgott" {
		t.Fatalf("whoAmI = %q, %v", u, err)
	}
	if _, err := p.whoAmI(context.Background(), websession.Memory(nil, "")); !errors.Is(err, provider.ErrNotSignedIn) {
		t.Fatalf("signed out: %v", err)
	}
	if p.Capabilities().Reply {
		t.Fatal("reply must stay off until the live check")
	}
}

// replyThread is a signed-in thread page with a reply form (the recorded
// shape is unverified until the owner's first sign-in: TASKS.md step 11).
func replyThread(extra string) string {
	return `<div class="header"><a href="/user/Morgott">Morgott</a><a href="/logout">Log out</a></div><div class="container">` +
		`<div class="discussion-message flex"><div class="discussion-message-content"><div class="discussion-message-header"><div class="discussion-message-author"><a href="/user/dzbanek">dzbanek</a></div><div title="2026-09-27T09:27:08.720000">x</div></div><div class="discussion-message-body">Wires</div></div></div>` +
		extra +
		`<form class="discussion-message-editor" method="post"><input type="hidden" name="csrf_token" value="tok-1"><textarea name="message"></textarea><button type="submit">Reply</button></form></div>`
}

func ownMessage(body string, at time.Time) string {
	return `<div class="discussion-message flex"><div class="discussion-message-content"><div class="discussion-message-header discussion-message-header-owner"><div class="discussion-message-author"><a href="/user/Morgott">Morgott</a></div><div title="` +
		at.UTC().Format("2006-01-02T15:04:05.000000") + `">now</div></div><div class="discussion-message-body">` + body + `</div></div></div>`
}

func TestReplyFormPostedOnceWithReadBack(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		save   bool
		want   error
	}{{"ok", 200, true, nil}, {"500 but saved", 500, true, nil}, {"lost", 502, false, ErrUnknownOutcome}} {
		pt := newPortal(t)
		id := "6ab8e16ca3e387a150232548"
		pt.threads[id] = replyThread("")
		pt.onPost = func(w http.ResponseWriter, form url.Values) {
			if tc.save {
				pt.mu.Lock()
				pt.threads[id] = replyThread(ownMessage(form.Get("message"), time.Now()))
				pt.mu.Unlock()
			}
			w.WriteHeader(tc.status)
		}
		host, _, _ := strings.Cut(strings.TrimPrefix(pt.srv.URL, "https://"), ":")
		jar, _ := websession.Open(filepath.Join(t.TempDir(), "factorio.json"))
		_ = jar.Replace([]websession.Cookie{{Name: "session", Value: "ok", Domain: host, Path: "/"}}, "UA", "Morgott", signin.SourceWindow)
		p := pt.provider(Options{Session: signin.New(signin.Spec{Platform: Platform}, jar, nil), ReadBackWaits: []time.Duration{time.Millisecond}})
		c, err := p.Reply(context.Background(), "thread:"+id, "Fixed in 0.3")
		switch {
		case tc.want == nil && (err != nil || c.Author != "Morgott" || !strings.HasPrefix(c.ExternalID, id+"/")):
			t.Fatalf("%s: %+v, %v", tc.name, c, err)
		case tc.want != nil && !errors.Is(err, tc.want):
			t.Fatalf("%s: err = %v", tc.name, err)
		}
		if len(pt.posts) != 1 || pt.posts[0].Get("csrf_token") != "tok-1" || pt.posts[0].Get("message") != "Fixed in 0.3" {
			t.Fatalf("%s: posts = %v", tc.name, pt.posts)
		}
	}
}

func TestReplyLoginPageIsNotSuccess(t *testing.T) {
	pt := newPortal(t)
	id := "6ab8e16ca3e387a150232548"
	pt.threads[id] = replyThread("")
	pt.onPost = func(w http.ResponseWriter, _ url.Values) {
		_, _ = w.Write([]byte(`<div class="header"><a href="/login">Log in</a></div><div class="container"><form action="/login"></form></div>`))
	}
	host, _, _ := strings.Cut(strings.TrimPrefix(pt.srv.URL, "https://"), ":")
	jar := websession.Memory([]websession.Cookie{{Name: "session", Value: "ok", Domain: host, Path: "/"}}, "UA")
	_ = jar.SetAccount("Morgott")
	p := pt.provider(Options{Session: signin.New(signin.Spec{Platform: Platform}, jar, nil)})
	if _, err := p.Reply(context.Background(), "thread:"+id, "x"); !errors.Is(err, provider.ErrRelogin) || len(pt.posts) != 1 {
		t.Fatalf("err = %v, posts %d", err, len(pt.posts))
	}
}

// TestLiveRead is the read-only live check: IW_FACTORIO_LIVE=1.
func TestLiveRead(t *testing.T) {
	if os.Getenv("IW_FACTORIO_LIVE") != "1" {
		t.Skip("set IW_FACTORIO_LIVE=1")
	}
	p := New(Options{Author: func() string { return "Morgott" }})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	start := time.Now()
	projects, err := p.ListProjects(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, pr := range projects {
		items, err := p.SyncItems(ctx, pr, time.Time{})
		if err != nil {
			t.Fatal(err)
		}
		comments, bugs := 0, 0
		for _, it := range items {
			comments += len(it.Comments)
			if it.Kind == store.KindBug {
				bugs++
			}
		}
		t.Logf("%s (%s) code=%s: %d threads (%d bugs), %d comments", pr.ExternalID, pr.Name, pr.CodeURL, len(items), bugs, comments)
	}
	t.Logf("total %v", time.Since(start))
}
