package curseforge

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/browser"
	"github.com/UberMorgott/issuewatcher/internal/provider"
	"github.com/UberMorgott/issuewatcher/internal/signin"
	"github.com/UberMorgott/issuewatcher/internal/websession"
)

// cfSite is a fake www.curseforge.com + api.cfwidget.com on one TLS server.
type cfSite struct {
	t   *testing.T
	srv *httptest.Server
	hc  *http.Client

	mu        sync.Mutex
	pages     map[string]string // "mod/page" → JSON
	profileOK bool
	posts     []*http.Request
	postBody  []string
	postReply func(w http.ResponseWriter, body map[string]any)
}

func readFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name)) //nolint:gosec // G304: fixed test fixtures
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func newCFSite(t *testing.T) *cfSite {
	s := &cfSite{t: t, pages: map[string]string{
		"1437738/0": readFixture(t, "comments_0.json"), "1437738/1": readFixture(t, "comments_1.json"),
	}}
	s.srv = httptest.NewTLSServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.srv.Close)
	s.hc = s.srv.Client()
	if tr, ok := s.hc.Transport.(*http.Transport); ok {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // G402: httptest certificate
	}
	return s
}

func (s *cfSite) serve(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/author/search/Morgott":
		_, _ = w.Write([]byte(`{"id":136845864,"username":"Morgott","projects":[{"id":1437738,"name":"Nick Name Changer"}]}`))
	case r.URL.Path == "/1437738":
		_, _ = w.Write([]byte(`{"summary":"Source: https://github.com/UberMorgott/NickNameChanger","urls":{"curseforge":"https://www.curseforge.com/hytale/mods/nick-name-changer"}}`))
	case r.URL.Path == "/api/v1/users/profile":
		s.mu.Lock()
		ok := s.profileOK
		s.mu.Unlock()
		if !ok || !strings.Contains(r.Header.Get("Cookie"), "CobaltSession=good") {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"userId":136845864,"displayName":"Morgott","userName":"user_7onp0c5v0pzp9m6j"}`))
	case strings.HasPrefix(r.URL.Path, "/api/v1/mods/") && strings.HasSuffix(r.URL.Path, "/comments"):
		if r.Header.Get("Cookie") != "" {
			http.Error(w, "reads must not send cookies", 400)
			return
		}
		mod := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/mods/"), "/comments")
		s.mu.Lock()
		body, ok := s.pages[mod+"/"+r.URL.Query().Get("page")]
		s.mu.Unlock()
		if !ok {
			body = `{"data":[],"pagination":{"index":9,"totalCount":82,"pageSize":20}}`
		}
		_, _ = w.Write([]byte(body))
	case r.URL.Path == "/api/v1/comments" && r.Method == http.MethodPost:
		var body map[string]any
		raw, _ := json.Marshal(nil)
		_ = raw
		dec := json.NewDecoder(r.Body)
		_ = dec.Decode(&body)
		b, _ := json.Marshal(body)
		s.mu.Lock()
		s.posts = append(s.posts, r)
		s.postBody = append(s.postBody, string(b))
		reply := s.postReply
		s.mu.Unlock()
		reply(w, body)
	default:
		http.NotFound(w, r)
	}
}

func (s *cfSite) provider(t *testing.T, cookies []websession.Cookie, br Fetcher) *Provider {
	t.Helper()
	jar, _ := websession.Open(filepath.Join(t.TempDir(), "curseforge.json"))
	if cookies != nil {
		_ = jar.Replace(cookies, "UA/1", "Morgott", signin.SourceWindow)
	}
	mgr := signin.New(signin.Spec{Platform: Platform}, jar, nil)
	return New(Options{Native: &NativeOptions{HTTP: s.hc, Site: s.srv.URL, Widget: s.srv.URL, Session: mgr, Browser: br},
		ReadBackWaits: []time.Duration{time.Millisecond}})
}

func (s *cfSite) cookies() []websession.Cookie {
	h := strings.TrimPrefix(s.srv.URL, "https://")
	h, _, _ = strings.Cut(h, ":")
	return []websession.Cookie{{Name: "CobaltSession", Value: "good", Domain: h, Path: "/", Secure: true},
		{Name: "XSRF-TOKEN", Value: "xsrf-123", Domain: h, Path: "/", Secure: true}}
}

func TestNativeCommentsFixture(t *testing.T) {
	s := newCFSite(t)
	p := s.provider(t, nil, nil)
	r, err := p.b.page(context.Background(), 1437738, 1)
	if err != nil || len(r.Comments) != 8 || r.Total == nil || *r.Total != 82 || *r.Pages != 5 {
		t.Fatalf("page = %+v, %v", r, err)
	}
	replies := 0
	for _, c := range r.Comments {
		replies += len(c.Replies)
		if c.Author == "" || c.Author == "?" || c.CreatedAt == nil || !strings.HasSuffix(*c.CreatedAt, "Z") || c.Body == "" {
			t.Fatalf("thread = %+v", c)
		}
	}
	if replies < 12 {
		t.Fatalf("replies = %d (every depth is flattened)", replies)
	}
}

func TestNativeSyncSignedOut(t *testing.T) {
	s := newCFSite(t)
	p := s.provider(t, nil, nil)
	p.opts.Author = func() string { return "Morgott" }
	projects, err := p.ListProjects(context.Background())
	if err != nil || len(projects) != 1 || projects[0].URL != "https://www.curseforge.com/hytale/mods/nick-name-changer" ||
		projects[0].CodeURL != "https://github.com/UberMorgott/NickNameChanger" {
		t.Fatalf("projects = %+v, %v", projects, err)
	}
	items, err := p.SyncItems(context.Background(), projects[0], time.Time{})
	if err != nil || len(items) != 15 {
		t.Fatalf("items = %d, %v", len(items), err)
	}
	for _, it := range items {
		if !strings.HasPrefix(it.ExternalID, "comment:1437738/") || it.URL != "https://www.curseforge.com/hytale/mods/nick-name-changer/comments" {
			t.Fatalf("item = %+v", it)
		}
	}
	var st provider.PollState
	if _, err := p.DetectChanges(context.Background(), projects[0], &st); err != nil || st.ETags["v2:curseforge:page1"] == "" {
		t.Fatalf("fingerprint: %v %v", st.ETags, err)
	}
	// Signed out, the configured author is the account (keyless reads), but a
	// reply still needs the session.
	if acc, err := p.Account(context.Background()); err != nil || acc != "Morgott" {
		t.Fatalf("account without a session = %q, %v", acc, err)
	}
	s.postReply = func(http.ResponseWriter, map[string]any) { t.Fatal("posted without a session") }
	if _, err := p.Reply(context.Background(), "comment:1437738/"+firstRoot(t, s), "x"); !errors.Is(err, provider.ErrNotSignedIn) {
		t.Fatalf("reply without a session: %v", err)
	}
	p.opts.Author = func() string { return "" }
	if _, err := p.Account(context.Background()); !errors.Is(err, provider.ErrNotSignedIn) {
		t.Fatalf("account without a session or author: %v", err)
	}
	if p.Scheduling().PollMinInterval != 5*time.Minute {
		t.Fatal("native change check every 5 min")
	}
}

func TestNativeAccount(t *testing.T) {
	s := newCFSite(t)
	s.profileOK = true
	p := s.provider(t, s.cookies(), nil)
	if acc, err := p.Account(context.Background()); err != nil || acc != "Morgott" {
		t.Fatalf("account = %q, %v", acc, err)
	}
	s.mu.Lock()
	s.profileOK = false
	s.mu.Unlock()
	if _, err := p.Account(context.Background()); !errors.Is(err, ErrRelogin) {
		t.Fatalf("expired session: err = %v", err)
	}
}

func TestStripHTML(t *testing.T) {
	for in, want := range map[string]string{
		"<p>Hello&nbsp;<b>world</b></p><p>two &amp; three</p>": "Hello world\n\ntwo & three",
		"a<br/>b<br>c":            "a\nb\nc",
		"&#65;&#x42; &lt;tag&gt;": "AB <tag>",
		"  <div> x </div>  ":      "x",
	} {
		if got := StripHTML(in); got != want {
			t.Errorf("%q → %q, want %q", in, got, want)
		}
	}
}

// ── replies ──────────────────────────────────────────────────────

func (s *cfSite) addReply(root, id, author, text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	body := s.pages["1437738/0"]
	var d map[string]any
	dec := json.NewDecoder(strings.NewReader(body))
	dec.UseNumber()
	_ = dec.Decode(&d)
	for _, c := range d["data"].([]any) { //nolint:forcetypeassert // test fixture shape
		m := c.(map[string]any) //nolint:forcetypeassert // test fixture shape
		if fmt.Sprint(m["id"]) == root {
			reps, _ := m["replies"].([]any)
			m["replies"] = append(reps, map[string]any{"id": id, "text": "In reply to x: " + text, "author": map[string]any{"displayName": author},
				"datePosted": time.Now().UnixMilli()})
		}
	}
	b, _ := json.Marshal(d)
	s.pages["1437738/0"] = string(b)
}

func firstRoot(t *testing.T, s *cfSite) string {
	var d struct {
		Data []struct {
			ID json.Number `json:"id"`
		} `json:"data"`
	}
	_ = json.Unmarshal([]byte(s.pages["1437738/0"]), &d)
	return d.Data[0].ID.String()
}

func TestNativeReplyExactBodyAndHeaders(t *testing.T) {
	s := newCFSite(t)
	s.profileOK = true
	s.postReply = func(w http.ResponseWriter, _ map[string]any) { _, _ = w.Write([]byte(`{"id":9990001}`)) }
	p := s.provider(t, s.cookies(), nil)
	root := firstRoot(t, s)
	c, err := p.Reply(context.Background(), "comment:1437738/"+root, "Fixed <now>\nthanks")
	if err != nil || c.ExternalID != "9990001" || c.Author != "Morgott" {
		t.Fatalf("reply = %+v, %v", c, err)
	}
	if len(s.posts) != 1 {
		t.Fatalf("posts = %d", len(s.posts))
	}
	req := s.posts[0]
	if req.Header.Get("X-XSRF-TOKEN") != "xsrf-123" || !strings.Contains(req.Header.Get("Cookie"), "CobaltSession=good") ||
		req.Header.Get("Content-Type") != "application/json" || req.UserAgent() != "UA/1" {
		t.Fatalf("headers = %v", req.Header)
	}
	var got map[string]any
	dec := json.NewDecoder(strings.NewReader(s.postBody[0]))
	dec.UseNumber()
	_ = dec.Decode(&got)
	if got["body"] != "Fixed &lt;now&gt;<br>thanks" || got["bodyType"] != "RawHtml" || fmt.Sprint(got["entityId"]) != "1437738" ||
		fmt.Sprint(got["parentId"]) != root || len(got) != 4 {
		t.Fatalf("body = %s", s.postBody[0])
	}
}

func TestNativeReplyRelogin(t *testing.T) {
	s := newCFSite(t)
	s.profileOK = true
	s.postReply = func(w http.ResponseWriter, _ map[string]any) { http.Error(w, "no", http.StatusUnauthorized) }
	p := s.provider(t, s.cookies(), nil)
	if _, err := p.Reply(context.Background(), "comment:1437738/"+firstRoot(t, s), "x"); !errors.Is(err, ErrRelogin) {
		t.Fatalf("err = %v", err)
	}
	if len(s.posts) != 1 {
		t.Fatalf("posts = %d", len(s.posts))
	}
}

func TestNativeReplyChallengeReadBack(t *testing.T) {
	for _, saved := range []bool{true, false} {
		s := newCFSite(t)
		s.profileOK = true
		root := firstRoot(t, s)
		s.postReply = func(w http.ResponseWriter, body map[string]any) {
			if saved {
				go s.addReply(root, "9990002", "Morgott", "hello")
				time.Sleep(20 * time.Millisecond)
			}
			w.Header().Set("cf-mitigated", "challenge")
			w.WriteHeader(http.StatusForbidden)
		}
		p := s.provider(t, s.cookies(), nil)
		c, err := p.Reply(context.Background(), "comment:1437738/"+root, "hello")
		switch {
		case saved && (err != nil || c.ExternalID != "9990002"):
			t.Fatalf("saved: %+v, %v", c, err)
		case !saved && !errors.Is(err, ErrUnknownOutcome):
			t.Fatalf("not saved: err = %v", err)
		}
		if len(s.posts) != 1 {
			t.Fatalf("posts = %d (never a second POST)", len(s.posts))
		}
	}
}

func TestNativeReplyLostAnswerNoID(t *testing.T) {
	s := newCFSite(t)
	s.profileOK = true
	root := firstRoot(t, s)
	s.postReply = func(w http.ResponseWriter, _ map[string]any) {
		s.addReply(root, "9990003", "Morgott", "hello")
		w.WriteHeader(http.StatusBadGateway)
	}
	p := s.provider(t, s.cookies(), nil)
	c, err := p.Reply(context.Background(), "comment:1437738/"+root, "hello")
	if err != nil || c.ExternalID != "9990003" || len(s.posts) != 1 {
		t.Fatalf("reply = %+v, %v, posts %d", c, err, len(s.posts))
	}
}

type fakeBrowser struct {
	mu   sync.Mutex
	reqs []browser.Request
}

func (f *fakeBrowser) Fetch(_ context.Context, req browser.Request) (browser.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, req)
	if strings.HasSuffix(req.URL, "/users/profile") {
		return browser.Response{Status: 200, URL: req.URL, Body: `{"userId":136845864,"displayName":"Morgott"}`}, nil
	}
	return browser.Response{Status: 200, URL: req.URL, Body: `{"data":{"id":9990004}}`}, nil
}

type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// A challenged plain users/profile falls back to a read-only browser probe, so
// a signed-in user is recognized instead of waiting out the sign-in timeout.
func TestSignInProbeBrowserOnChallenge(t *testing.T) {
	hc := &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusForbidden, Header: http.Header{"Cf-Mitigated": {"challenge"}}, Request: r,
			Body: io.NopCloser(strings.NewReader("<title>Just a moment...</title>"))}, nil
	})}
	jar := websession.Memory([]websession.Cookie{{Name: "sid", Value: "x", Domain: "curseforge.com", Path: "/", Secure: true}}, "")
	fb := &fakeBrowser{}
	acc, err := SignInSpecBrowser(hc, nil, fb).Probe(t.Context(), jar)
	if err != nil || acc != "Morgott" {
		t.Fatalf("probe = %q, %v", acc, err)
	}
	if len(fb.reqs) != 1 || fb.reqs[0].Method != "" || !strings.HasSuffix(fb.reqs[0].URL, "/api/v1/users/profile") {
		t.Fatalf("browser probe requests: %+v", fb.reqs)
	}
	// Without a browser the challenge stays "not signed in".
	if _, err := SignInSpec(hc, nil).Probe(t.Context(), jar); !errors.Is(err, provider.ErrNotSignedIn) {
		t.Fatalf("no browser: err = %v", err)
	}
}

func TestNativeReplyBrowserWhenPreflightChallenged(t *testing.T) {
	s := newCFSite(t)
	s.profileOK = true
	s.postReply = func(w http.ResponseWriter, _ map[string]any) { t.Fatal("plain POST after a failed preflight") }
	fb := &fakeBrowser{}
	p := s.provider(t, s.cookies(), fb)
	// The profile answers a challenge: the write goes through the browser.
	orig := s.srv.Config.Handler
	s.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/users/profile" {
			w.Header().Set("cf-mitigated", "challenge")
			w.WriteHeader(http.StatusForbidden)
			return
		}
		orig.ServeHTTP(w, r)
	})
	c, err := p.Reply(context.Background(), "comment:1437738/"+firstRoot(t, s), "via browser")
	if err != nil || c.ExternalID != "9990004" || len(fb.reqs) < 2 || fb.reqs[len(fb.reqs)-1].Headers["X-XSRF-TOKEN"] != "xsrf-123" ||
		!strings.Contains(fb.reqs[len(fb.reqs)-1].Body, `"entityId":1437738`) {
		t.Fatalf("reply = %+v, %v, browser reqs %+v", c, err, fb.reqs)
	}
}

func TestNativeReplyNeedsSession(t *testing.T) {
	s := newCFSite(t)
	s.postReply = func(w http.ResponseWriter, _ map[string]any) { t.Fatal("posted without a session") }
	p := s.provider(t, nil, nil)
	if _, err := p.Reply(context.Background(), "comment:1437738/1", "x"); !errors.Is(err, provider.ErrNotSignedIn) {
		t.Fatalf("err = %v", err)
	}
}
