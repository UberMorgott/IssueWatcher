package github

import (
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/provider"
	"github.com/UberMorgott/issuewatcher/internal/provider/github/githubtest"
)

func newAuth(t *testing.T, gh *githubtest.Server) *Auth {
	t.Helper()
	a := NewAuth(filepath.Join(t.TempDir(), "secrets"))
	a.WebURL, a.APIURL, a.HTTP = gh.URL, gh.URL, gh.Client()
	a.PollUnit = time.Millisecond
	return a
}

// signIn runs manifest conversion + web flow against the fake.
func signIn(t *testing.T, gh *githubtest.Server, a *Auth) Token {
	t.Helper()
	if _, err := a.ConvertManifest(t.Context(), githubtest.ManifestCode, 4567); err != nil {
		t.Fatal(err)
	}
	verifier, challenge := PKCE()
	tok, err := a.Exchange(t.Context(), gh.IssueCode(challenge), verifier, "http://127.0.0.1:4567"+CallbackPath)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func TestManifestShape(t *testing.T) {
	body, err := Manifest("IssueWatcher-abc", 4567)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	perms, _ := m["default_permissions"].(map[string]any)
	want := map[string]string{"issues": "write", "pull_requests": "write", "contents": "write", "metadata": "read"}
	for k, v := range want {
		if perms[k] != v {
			t.Errorf("permission %s = %v, want %s", k, perms[k], v)
		}
	}
	if m["public"] != false || m["redirect_url"] != "http://127.0.0.1:4567"+AppCreatedPath {
		t.Errorf("public/redirect_url: %v %v", m["public"], m["redirect_url"])
	}
	cb, _ := m["callback_urls"].([]any)
	if len(cb) != 2 || cb[0] != "http://127.0.0.1:4567"+CallbackPath || cb[1] != "http://127.0.0.1"+CallbackPath {
		t.Errorf("callback_urls = %v", cb)
	}
	hook, _ := m["hook_attributes"].(map[string]any)
	if hook["active"] != false {
		t.Errorf("webhook must be inactive: %v", hook)
	}
	if n := AppName(); !strings.HasPrefix(n, "IssueWatcher-") || len(n) > 34 {
		t.Errorf("app name %q", n)
	}
}

func TestConvertManifestStoresCredentials(t *testing.T) {
	gh := githubtest.New(t)
	a := newAuth(t, gh)
	if _, err := a.App(); !errors.Is(err, ErrNoApp) {
		t.Fatalf("before conversion: %v", err)
	}
	if _, err := a.ConvertManifest(t.Context(), "wrong-code", 1); err == nil {
		t.Fatal("bad code accepted")
	}
	app, err := a.ConvertManifest(t.Context(), githubtest.ManifestCode, 4567)
	if err != nil {
		t.Fatal(err)
	}
	got, err := a.App()
	if err != nil || got.ClientID != githubtest.ClientID || got.ClientSecret != githubtest.ClientSecret ||
		got.PEM == "" || got.Slug != "issuewatcher-test" || got.RegisteredPort != 4567 || got.Owner != githubtest.Login {
		t.Fatalf("stored app %+v err %v", got, err)
	}
	if u := a.InstallURL(app); u != gh.URL+"/apps/issuewatcher-test/installations/new" {
		t.Errorf("install url %s", u)
	}
}

func TestAuthorizeURLCarriesPKCEAndState(t *testing.T) {
	a := NewAuth(t.TempDir())
	verifier, challenge := PKCE()
	if len(verifier) != 43 || len(challenge) != 43 {
		t.Fatalf("verifier/challenge length %d/%d, want 43", len(verifier), len(challenge))
	}
	u, err := url.Parse(a.AuthorizeURL(App{ClientID: "cid"}, "http://127.0.0.1:9/cb", "st", challenge))
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if u.Host != "github.com" || u.Path != "/login/oauth/authorize" || q.Get("client_id") != "cid" ||
		q.Get("state") != "st" || q.Get("code_challenge") != challenge || q.Get("code_challenge_method") != "S256" ||
		q.Get("redirect_uri") != "http://127.0.0.1:9/cb" {
		t.Fatalf("authorize url %s", u)
	}
}

func TestExchangeRequiresMatchingVerifier(t *testing.T) {
	gh := githubtest.New(t)
	a := newAuth(t, gh)
	if _, err := a.ConvertManifest(t.Context(), githubtest.ManifestCode, 1); err != nil {
		t.Fatal(err)
	}
	_, challenge := PKCE()
	other, _ := PKCE()
	_, err := a.Exchange(t.Context(), gh.IssueCode(challenge), other, "r")
	var oe *OAuthError
	if !errors.As(err, &oe) || oe.Code != "invalid_grant" {
		t.Fatalf("wrong verifier: err %v", err)
	}
}

func TestExchangeStoresTokenAndLogin(t *testing.T) {
	gh := githubtest.New(t)
	a := newAuth(t, gh)
	tok := signIn(t, gh, a)
	if tok.Login != githubtest.Login || tok.RefreshToken == "" || tok.Expiry.IsZero() {
		t.Fatalf("token %+v", tok)
	}
	// A fresh Auth over the same dir reads it back from disk.
	b := newAuth(t, gh)
	b.Dir = a.Dir
	if b.Login() != githubtest.Login {
		t.Fatalf("login after reload = %q", b.Login())
	}
	raw, err := os.ReadFile(filepath.Join(a.Dir, tokenFile))
	if err != nil || !strings.Contains(string(raw), tok.AccessToken) {
		t.Fatalf("token file: %v", err)
	}
}

func TestAccessTokenRefreshesNearExpiry(t *testing.T) {
	gh := githubtest.New(t)
	a := newAuth(t, gh)
	tok := signIn(t, gh, a)

	got, err := a.AccessToken(t.Context())
	if r, _ := gh.Counts(); err != nil || got != tok.AccessToken || r != 0 {
		t.Fatalf("fresh token: %q err %v refreshes %d", got, err, r)
	}
	a.Now = func() time.Time { return time.Now().Add(8 * time.Hour) } // past expiry
	got, err = a.AccessToken(t.Context())
	if r, _ := gh.Counts(); err != nil || got == tok.AccessToken || r != 1 {
		t.Fatalf("refreshed token: %q err %v refreshes %d", got, err, r)
	}
	if a.Login() != githubtest.Login {
		t.Fatal("login lost on refresh")
	}
}

func TestDeadRefreshTokenSignsOut(t *testing.T) {
	gh := githubtest.New(t)
	a := newAuth(t, gh)
	signIn(t, gh, a)
	a.mu.Lock()
	a.token.RefreshToken = "refresh-unknown"
	a.token.Expiry = time.Now().Add(-time.Minute)
	a.mu.Unlock()
	if _, err := a.AccessToken(t.Context()); !errors.Is(err, provider.ErrNotSignedIn) {
		t.Fatalf("dead refresh: err %v", err)
	}
	if a.Login() != "" {
		t.Fatal("token kept after failed refresh")
	}
}

func TestNonExpiringTokenIsUsedAsIs(t *testing.T) {
	gh := githubtest.New(t)
	gh.Mu.Lock()
	gh.ExpiresIn = 0
	gh.Mu.Unlock()
	a := newAuth(t, gh)
	tok := signIn(t, gh, a)
	a.Now = func() time.Time { return time.Now().Add(1000 * time.Hour) }
	if got, err := a.AccessToken(t.Context()); err != nil || got != tok.AccessToken {
		t.Fatalf("non-expiring: %q %v", got, err)
	}
}

func TestDeviceFlow(t *testing.T) {
	gh := githubtest.New(t)
	a := newAuth(t, gh)
	if _, err := a.ConvertManifest(t.Context(), githubtest.ManifestCode, 1); err != nil {
		t.Fatal(err)
	}
	dc, err := a.DeviceStart(t.Context())
	if err != nil || dc.UserCode != "ABCD-1234" {
		t.Fatalf("device start %+v %v", dc, err)
	}
	tok, err := a.DevicePoll(t.Context(), dc)
	if _, hits := gh.Counts(); err != nil || tok.Login != githubtest.Login || hits != 2 {
		t.Fatalf("device poll %+v err %v hits %d", tok, err, hits)
	}
}

func TestLogoutKeepsApp(t *testing.T) {
	gh := githubtest.New(t)
	a := newAuth(t, gh)
	signIn(t, gh, a)
	if err := a.Logout(); err != nil {
		t.Fatal(err)
	}
	if _, err := a.AccessToken(t.Context()); !errors.Is(err, provider.ErrNotSignedIn) {
		t.Fatalf("after logout: %v", err)
	}
	if _, err := a.App(); err != nil {
		t.Fatalf("app lost on logout: %v", err)
	}
}
