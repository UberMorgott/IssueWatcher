package signin

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/browser"
	"github.com/UberMorgott/issuewatcher/internal/provider"
	"github.com/UberMorgott/issuewatcher/internal/websession"
)

type fakeBrowser struct {
	mu      sync.Mutex
	cookies []browser.Cookie
	window  bool
	opened  int
	stops   int
	cleared int
}

func (f *fakeBrowser) Cookies(context.Context, []string) ([]browser.Cookie, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]browser.Cookie(nil), f.cookies...), nil
}

func (f *fakeBrowser) OpenWindow(context.Context, string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.window = true
	f.opened++
	return nil
}

func (f *fakeBrowser) WindowOpen(context.Context) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.window
}

func (f *fakeBrowser) ClearSite(context.Context, []string, []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cookies = nil
	f.cleared++
	return nil
}

func (f *fakeBrowser) UserAgent() string { return "UA" }
func (f *fakeBrowser) Name() string      { return "Fake" }
func (f *fakeBrowser) Stop() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.window = false
	f.stops++
}

func (f *fakeBrowser) signIn(v string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cookies = []browser.Cookie{{Name: "sid", Value: v, Domain: ".example.com", Path: "/", Session: true}}
}

// probe: "good" and "fresh" are signed in, anything else is not.
func probe(_ context.Context, jar *websession.Jar) (string, error) {
	switch jar.Value("sid") {
	case "good", "fresh":
		return "Morgott", nil
	}
	return "", fmt.Errorf("%w: test", provider.ErrNotSignedIn)
}

func newManager(t *testing.T) (*Manager, *fakeBrowser) {
	jar, err := websession.Open(filepath.Join(t.TempDir(), "p.json"))
	if err != nil {
		t.Fatal(err)
	}
	fb := &fakeBrowser{}
	m := New(Spec{Platform: "p", LoginURL: "https://www.example.com/login", Domains: []string{"example.com"},
		Probe: probe, Interval: 5 * time.Millisecond, Timeout: time.Second}, jar, fb)
	return m, fb
}

func waitDone(t *testing.T, m *Manager) provider.Login {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if l := m.Status(); !l.InProgress {
			return l
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("sign-in still waiting")
	return provider.Login{}
}

func TestStoredSessionIsSilent(t *testing.T) {
	m, fb := newManager(t)
	_ = m.Jar().Replace([]websession.Cookie{{Name: "sid", Value: "good", Domain: ".example.com", Path: "/"}}, "UA", "", SourceWindow)
	l, err := m.Login(context.Background())
	if err != nil || !l.LoggedIn || l.Account != "Morgott" || l.Source != SourceWindow || fb.opened != 0 {
		t.Fatalf("login = %+v, %v, opened %d", l, err, fb.opened)
	}
}

func TestProfileCapture(t *testing.T) {
	m, fb := newManager(t)
	_ = m.Jar().Replace([]websession.Cookie{{Name: "sid", Value: "expired", Domain: ".example.com", Path: "/"}}, "UA", "", SourceWindow)
	fb.signIn("good")
	l, err := m.Login(context.Background())
	if err != nil || !l.LoggedIn || l.Source != SourceProfile || fb.opened != 0 {
		t.Fatalf("login = %+v, %v", l, err)
	}
	if m.Jar().Value("sid") != "good" || m.Jar().Account() != "Morgott" {
		t.Fatal("profile session not stored")
	}
}

func TestWindowCapture(t *testing.T) {
	m, fb := newManager(t)
	l, err := m.Login(context.Background())
	if err != nil || !l.InProgress || !l.Window || fb.opened != 1 {
		t.Fatalf("login = %+v, %v", l, err)
	}
	if again, _ := m.Login(context.Background()); !again.InProgress || fb.opened != 1 {
		t.Fatal("a second «Подключить» opened another window")
	}
	time.Sleep(20 * time.Millisecond)
	fb.signIn("fresh")
	l = waitDone(t, m)
	if !l.LoggedIn || l.Account != "Morgott" || l.Source != SourceWindow || fb.WindowOpen(context.Background()) {
		t.Fatalf("after sign-in: %+v (window must close)", l)
	}
}

func TestWindowClosedByUser(t *testing.T) {
	m, fb := newManager(t)
	if _, err := m.Login(context.Background()); err != nil {
		t.Fatal(err)
	}
	fb.Stop() // the user closes the window
	if l := waitDone(t, m); l.LoggedIn || l.Detail != "окно входа закрыто" {
		t.Fatalf("status = %+v", l)
	}
}

func TestCancel(t *testing.T) {
	m, _ := newManager(t)
	if _, err := m.Login(context.Background()); err != nil {
		t.Fatal(err)
	}
	m.Cancel()
	if l := m.Status(); l.InProgress || l.LoggedIn || l.Detail != "вход отменён" {
		t.Fatalf("status = %+v", l)
	}
}

func TestLogoutThenNoSilentCapture(t *testing.T) {
	m, fb := newManager(t)
	fb.signIn("good")
	if l, _ := m.Login(context.Background()); !l.LoggedIn {
		t.Fatal("not signed in")
	}
	if err := m.Logout(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !m.Jar().Empty() || fb.cleared != 1 {
		t.Fatal("logout kept the session")
	}
	l, err := m.Login(context.Background())
	if err != nil || l.LoggedIn || !l.InProgress {
		t.Fatalf("login after logout = %+v, %v (must open the window, not capture silently)", l, err)
	}
	m.Cancel()
}
