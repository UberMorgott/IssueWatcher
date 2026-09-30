package signin

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/browser"
	"github.com/UberMorgott/issuewatcher/internal/provider"
	"github.com/UberMorgott/issuewatcher/internal/websession"
)

// TestRealWindowSignIn opens a visible window of the installed browser on a
// local login page that signs in on load: IW_BROWSER_E2E=1.
func TestRealWindowSignIn(t *testing.T) {
	if os.Getenv("IW_BROWSER_E2E") != "1" {
		t.Skip("set IW_BROWSER_E2E=1")
	}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/login" {
			http.SetCookie(w, &http.Cookie{Name: "sid", Value: "fresh", Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode, Expires: time.Now().Add(time.Hour)})
		}
		_, _ = w.Write([]byte("<title>login</title>signed in"))
	}))
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "https://")
	host, _, _ = strings.Cut(host, ":")
	br := browser.New(browser.Options{Dir: t.TempDir(), Origins: []string{srv.URL}, Args: []string{"--ignore-certificate-errors"}})
	defer br.Close()
	jar, _ := websession.Open(filepath.Join(t.TempDir(), "p.json"))
	m := New(Spec{Platform: "p", LoginURL: srv.URL + "/login", Domains: []string{host}, Interval: 500 * time.Millisecond, Timeout: time.Minute,
		Probe: func(_ context.Context, j *websession.Jar) (string, error) {
			if j.Value("sid") == "fresh" {
				return "Tester", nil
			}
			return "", fmt.Errorf("%w: no sid", provider.ErrNotSignedIn)
		}}, jar, br)
	l, err := m.Login(context.Background())
	if err != nil || !l.InProgress {
		t.Fatalf("login = %+v, %v", l, err)
	}
	l = waitDone(t, m)
	if !l.LoggedIn || l.Account != "Tester" || br.Headed() {
		t.Fatalf("after window sign-in: %+v, headed=%v", l, br.Headed())
	}
	if err := m.Logout(context.Background()); err != nil {
		t.Fatal(err)
	}
	if cks, _ := br.Cookies(context.Background(), []string{host}); len(cks) != 0 {
		t.Fatalf("profile cookies after logout: %d", len(cks))
	}
}
