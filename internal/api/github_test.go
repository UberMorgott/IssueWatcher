package api

import (
	"encoding/json"
	"html"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/provider/github"
	"github.com/UberMorgott/issuewatcher/internal/provider/github/githubtest"
	"github.com/UberMorgott/issuewatcher/internal/store"
	"github.com/UberMorgott/issuewatcher/internal/syncer"
)

type env struct {
	s       *Server
	gh      *githubtest.Server
	auth    *github.Auth
	store   *store.Store
	sync    *syncer.Syncer
	browser *http.Client // signed-in session (cookie jar)
	unread  chan struct{}
	opened  chan string
}

func newEnv(t *testing.T, extra ...func(*Options)) *env {
	t.Helper()
	gh := githubtest.New(t)
	a := github.NewAuth(filepath.Join(t.TempDir(), "secrets"))
	a.WebURL, a.APIURL, a.HTTP, a.PollUnit = gh.URL, gh.URL, gh.Client(), time.Millisecond
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := store.New(db)
	sy := syncer.New(syncer.Options{Store: st, Provider: github.NewProvider(a), Log: slog.New(slog.DiscardHandler)})
	e := &env{gh: gh, auth: a, store: st, sync: sy, unread: make(chan struct{}, 8), opened: make(chan string, 8)}
	opts := Options{
		Assets: fstest.MapFS{"index.html": {Data: []byte(indexHTML)}},
		Open:   func(u string) { e.opened <- u },
		Log:    slog.New(slog.DiscardHandler),
		GitHub: a, Store: st, Sync: syncer.NewGroup(sy),
		OnUnreadChange: func() { e.unread <- struct{}{} },
	}
	for _, f := range extra {
		f(&opts)
	}
	e.s, err = New(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = e.s.Serve() }()
	t.Cleanup(func() { _ = e.s.Shutdown(t.Context()) })
	jar, _ := cookiejar.New(nil)
	e.browser = &http.Client{Jar: jar}
	if r := get(t, e.browser, e.s.LaunchURL("/"), ""); r.status != http.StatusOK {
		t.Fatalf("browser sign-in: %d", r.status)
	}
	return e
}

// call sends a request with the signed-in browser session and decodes JSON into out.
func (e *env) call(t *testing.T, method, path, body string, out any) int {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(t.Context(), method, e.s.BaseURL()+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := e.browser.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if out != nil && resp.StatusCode < 300 {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatalf("%s %s: decode: %v", method, path, err)
		}
	}
	return resp.StatusCode
}

// redirectFrom GETs rawURL like a cross-site navigation from github.com:
// no session cookie, redirects not followed. Returns status and Location.
func redirectFrom(t *testing.T, rawURL string) (int, string) {
	t.Helper()
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, rawURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode, resp.Header.Get("Location")
}

func TestGitHubSignInEndToEnd(t *testing.T) {
	e := newEnv(t)
	var st authStatus
	if code := e.call(t, http.MethodGet, "/api/auth/status", "", &st); code != http.StatusOK || st.App || st.SignedIn {
		t.Fatalf("initial status %d %+v", code, st)
	}

	// 1. Start: no app yet → local manifest page.
	var start struct{ Step, URL string }
	e.call(t, http.MethodPost, "/api/auth/github/start", "", &start)
	if start.Step != "create_app" || !strings.HasPrefix(start.URL, "/auth/github/manifest?state=") {
		t.Fatalf("start %+v", start)
	}
	state := strings.TrimPrefix(start.URL, "/auth/github/manifest?state=")

	// 2. Manifest page auto-posts to github.com with our loopback URLs.
	page := get(t, e.browser, e.s.BaseURL()+start.URL, "")
	body := html.UnescapeString(page.body)
	port := strconv.Itoa(e.s.Port())
	for _, want := range []string{
		`action="` + e.gh.URL + "/settings/apps/new?state=" + state + `"`,
		`"redirect_url":"http://127.0.0.1:` + port + github.AppCreatedPath + `"`,
		`"setup_url":"http://127.0.0.1:` + port + github.SetupPath + `"`,
		`"name":"IssueWatcher-`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("manifest page lacks %s:\n%s", want, body)
		}
	}
	if r := get(t, &http.Client{}, e.s.BaseURL()+start.URL, ""); r.status != http.StatusUnauthorized {
		t.Fatalf("manifest page without session: %d", r.status)
	}

	// 3. GitHub → app-created (no cookie): converts, stores, sends to install page.
	created := e.s.BaseURL() + github.AppCreatedPath + "?code=" + githubtest.ManifestCode + "&state=" + state
	code, loc := redirectFrom(t, created)
	if code != http.StatusSeeOther || loc != e.gh.URL+"/apps/issuewatcher-test/installations/new" {
		t.Fatalf("app-created: %d %s", code, loc)
	}
	if code, _ := redirectFrom(t, created); code != http.StatusForbidden {
		t.Fatalf("replayed manifest state: %d", code)
	}

	// 4. GitHub → setup after install: starts authorize with PKCE + state.
	code, loc = redirectFrom(t, e.s.BaseURL()+github.SetupPath+"?installation_id=1&setup_action=install")
	au, err := url.Parse(loc)
	if err != nil || code != http.StatusSeeOther || au.Path != "/login/oauth/authorize" {
		t.Fatalf("setup: %d %s", code, loc)
	}
	q := au.Query()
	if q.Get("client_id") != githubtest.ClientID || q.Get("code_challenge_method") != "S256" ||
		q.Get("redirect_uri") != e.s.BaseURL()+github.CallbackPath {
		t.Fatalf("authorize params %v", q)
	}

	// 5. Wrong state is refused; the real one signs in and relaunches the dashboard.
	cb := e.s.BaseURL() + github.CallbackPath + "?code=" + e.gh.IssueCode(q.Get("code_challenge"))
	if code, _ := redirectFrom(t, cb+"&state=forged"); code != http.StatusForbidden {
		t.Fatalf("forged state: %d", code)
	}
	code, loc = redirectFrom(t, cb+"&state="+q.Get("state"))
	if code != http.StatusSeeOther || !strings.HasPrefix(loc, e.s.BaseURL()+"/auth?t=") {
		t.Fatalf("callback: %d %s", code, loc)
	}
	e.call(t, http.MethodGet, "/api/auth/status", "", &st)
	if !st.App || !st.SignedIn || st.Login != githubtest.Login || st.AppSlug != "issuewatcher-test" {
		t.Fatalf("after sign-in %+v", st)
	}
	if p := st.Providers[0]; p.ID != "github" || p.State != connConnected || !p.Connected || p.AvatarURL == "" || p.SetupNeeded {
		t.Fatalf("github provider %+v", p)
	}

	// 6. Start again → straight to authorize; logout keeps the app.
	e.call(t, http.MethodPost, "/api/auth/github/start", "", &start)
	if start.Step != "authorize" || !strings.HasPrefix(start.URL, e.gh.URL+"/login/oauth/authorize?") {
		t.Fatalf("second start %+v", start)
	}
	e.call(t, http.MethodPost, "/api/auth/github/logout", "", &st)
	if st.SignedIn || !st.App {
		t.Fatalf("after logout %+v", st)
	}
}

func TestProviderStartOpensBrowser(t *testing.T) {
	e := newEnv(t)
	var st authStatus
	e.call(t, http.MethodGet, "/api/auth/status", "", &st)
	if len(st.Providers) != 4 || st.Providers[0].State != connNotConfigured || !st.Providers[0].SetupNeeded ||
		st.Providers[1].State != connUnavailable {
		t.Fatalf("providers %+v", st.Providers)
	}

	// First run: opens a launch URL that lands on the local manifest page.
	var start map[string]any
	if code := e.call(t, http.MethodPost, "/api/auth/github/start", "", &start); code != http.StatusOK || start["step"] != "create_app" {
		t.Fatalf("start %d %+v", code, start)
	}
	opened := <-e.opened
	fresh, _ := cookiejar.New(nil) // a new browser window: no session yet
	if r := get(t, &http.Client{Jar: fresh}, opened, ""); r.status != http.StatusOK || r.path != "/auth/github/manifest" {
		t.Fatalf("opened %s → %d %s", opened, r.status, r.path)
	}
	e.call(t, http.MethodGet, "/api/auth/status", "", &st)
	if st.Providers[0].State != connConnecting {
		t.Fatalf("state after start %q", st.Providers[0].State)
	}

	// App exists: opens the GitHub authorize URL directly.
	if _, err := e.auth.ConvertManifest(t.Context(), githubtest.ManifestCode, e.s.Port()); err != nil {
		t.Fatal(err)
	}
	e.call(t, http.MethodPost, "/api/auth/github/start", "", &start)
	if u := <-e.opened; start["step"] != "authorize" || !strings.HasPrefix(u, e.gh.URL+"/login/oauth/authorize?") {
		t.Fatalf("authorize start %+v opened %s", start, u)
	}

	for path, want := range map[string]int{
		"/api/auth/curseforge/start": http.StatusNotImplemented,
		"/api/auth/nope/start":       http.StatusNotFound,
		"/api/auth/github/logout":    http.StatusOK,
	} {
		if code := e.call(t, http.MethodPost, path, "", nil); code != want {
			t.Errorf("%s: %d, want %d", path, code, want)
		}
	}
	e.call(t, http.MethodGet, "/api/auth/status", "", &st)
	if st.Providers[0].State != connDisconnected {
		t.Fatalf("state after logout %q", st.Providers[0].State)
	}
}

func TestCallbackDeniedByUser(t *testing.T) {
	e := newEnv(t)
	if _, err := e.auth.ConvertManifest(t.Context(), githubtest.ManifestCode, e.s.Port()); err != nil {
		t.Fatal(err)
	}
	var start struct{ Step, URL string }
	e.call(t, http.MethodPost, "/api/auth/github/start", "", &start)
	au, _ := url.Parse(start.URL)
	code, _ := redirectFrom(t, e.s.BaseURL()+github.CallbackPath+"?error=access_denied&state="+au.Query().Get("state"))
	if code != http.StatusForbidden {
		t.Fatalf("denied: %d", code)
	}
	var st authStatus
	e.call(t, http.MethodGet, "/api/auth/status", "", &st)
	if p := st.Providers[0]; p.State != connError || !strings.Contains(p.Error, "access_denied") {
		t.Fatalf("after denial %+v", p)
	}
}

func TestDeviceFlowFallback(t *testing.T) {
	e := newEnv(t)
	if _, err := e.auth.ConvertManifest(t.Context(), githubtest.ManifestCode, e.s.Port()); err != nil {
		t.Fatal(err)
	}
	var dc map[string]any
	if code := e.call(t, http.MethodPost, "/api/auth/github/device", "", &dc); code != http.StatusOK || dc["userCode"] != "ABCD-1234" {
		t.Fatalf("device start %d %+v", code, dc)
	}
	if u := <-e.opened; u != e.gh.URL+"/login/device" {
		t.Fatalf("opened %s", u)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		var st authStatus
		e.call(t, http.MethodGet, "/api/auth/status", "", &st)
		if st.SignedIn && !st.Device.Pending {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("device flow did not finish: %+v", st)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
