package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/api"
	"github.com/UberMorgott/issuewatcher/internal/browser"
	"github.com/UberMorgott/issuewatcher/internal/provider"
	"github.com/UberMorgott/issuewatcher/internal/provider/curseforge"
	"github.com/UberMorgott/issuewatcher/internal/provider/factorio"
	"github.com/UberMorgott/issuewatcher/internal/provider/nexus"
	"github.com/UberMorgott/issuewatcher/internal/signin"
	"github.com/UberMorgott/issuewatcher/internal/websession"
)

// fakeWindow is the installed browser: the user signs in while the window is
// open (the next cookie read carries the session).
type fakeWindow struct {
	host    string
	mu      sync.Mutex
	window  bool
	signed  bool
	cleared int
}

func (f *fakeWindow) Cookies(context.Context, []string) ([]browser.Cookie, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.window {
		f.signed = true
	}
	if !f.signed {
		return nil, nil
	}
	return []browser.Cookie{{Name: "session", Value: "ok", Domain: f.host, Path: "/", Session: true}}, nil
}

func (f *fakeWindow) OpenWindow(context.Context, string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.window = true
	return nil
}

func (f *fakeWindow) WindowOpen(context.Context) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.window
}

func (f *fakeWindow) ClearSite(context.Context, []string, []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.signed = false
	f.cleared++
	return nil
}

func (f *fakeWindow) UserAgent() string { return "UA" }
func (f *fakeWindow) Name() string      { return "Cent Browser" }
func (f *fakeWindow) Stop() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.window = false
}

func (f *fakeWindow) Fetch(_ context.Context, req browser.Request) (browser.Response, error) {
	return browser.Response{Status: 404, URL: req.URL}, nil
}

// fakeSites answers the native engines' plain requests (GraphQL, CF profile,
// the Factorio portal home) on one TLS server.
func fakeSites(t *testing.T) (*httptest.Server, *http.Client) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		signed := strings.Contains(r.Header.Get("Cookie"), "session=ok")
		switch {
		case r.URL.Path == "/v2/graphql":
			_, _ = w.Write([]byte(`{"data":{"mods":{"totalCount":1,"nodes":[{"modId":147,"name":"ShareShip","description":"","uploader":{"name":"UberMorgott","memberId":6541781},"game":{"domainName":"windrose"}}]}}}`))
		case r.URL.Path == "/api/v1/users/profile" && signed:
			_, _ = w.Write([]byte(`{"userId":1,"displayName":"Morgott","userName":"user_x"}`))
		case r.URL.Path == "/api/v1/users/profile":
			http.Error(w, "{}", http.StatusUnauthorized)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	hc := srv.Client()
	if tr, ok := hc.Transport.(*http.Transport); ok {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // G402: httptest certificate
	}
	return srv, hc
}

// withNative makes every mod platform native over fakes.
func withNative(t *testing.T, m *modPlatforms, dir string) map[string]*fakeWindow {
	srv, hc := fakeSites(t)
	host, _, _ := strings.Cut(strings.TrimPrefix(srv.URL, "https://"), ":")
	windows := map[string]*fakeWindow{}
	accounts := map[string]string{nexus.Platform: "UberMorgott", curseforge.Platform: "Morgott", factorio.Platform: "Morgott"}
	m.native = func(id string, author func() string) (provider.Provider, *signin.Manager) {
		fw := windows[id]
		if fw == nil {
			fw = &fakeWindow{host: host}
			windows[id] = fw
		}
		jar, _ := websession.Open(filepath.Join(dir, "secrets", id+".json"))
		spec := signin.Spec{Platform: id, LoginURL: "https://example.invalid/login", Interval: 5 * time.Millisecond, Timeout: 5 * time.Second,
			Probe: func(_ context.Context, j *websession.Jar) (string, error) {
				if j.Value("session") == "ok" {
					return accounts[id], nil
				}
				return "", fmt.Errorf("%w: test", provider.ErrNotSignedIn)
			}}
		mgr := signin.New(spec, jar, fw)
		switch id {
		case nexus.Platform:
			return nexus.New(nexus.Options{Native: &nexus.NativeOptions{Browser: fw, Session: mgr, HTTP: hc, GraphQL: srv.URL + "/v2/graphql", APIRouter: srv.URL + "/graphql"}, Author: author}), mgr
		case curseforge.Platform:
			return curseforge.New(curseforge.Options{Native: &curseforge.NativeOptions{Browser: fw, Session: mgr, HTTP: hc, Site: srv.URL, Widget: srv.URL}, Author: author}), mgr
		}
		return factorio.New(factorio.Options{Session: mgr, HTTP: hc, Site: srv.URL, Author: author}), mgr
	}
	return windows
}

// «Подключить» on each native platform: switched on, the sign-in window opens,
// the session is captured once the user signed in, the card says «Подключено
// как X» with the session source.
func TestLoginNativeThreePlatforms(t *testing.T) {
	m, cfgs, _, dir := newTestPlatforms(t, nil)
	windows := withNative(t, m, dir)
	for _, id := range []string{nexus.Platform, curseforge.Platform} {
		if _, err := cfgs.Patch(cfgs.Get().Revision, []byte(`{"providers":{"`+id+`":{"engine":"native"}}}`), nil); err != nil {
			t.Fatal(err)
		}
	}
	want := map[string]string{nexus.Platform: "UberMorgott", curseforge.Platform: "Morgott", factorio.Platform: "Morgott"}
	for _, id := range modIDs {
		l, err := m.Login(t.Context(), id)
		if err != nil || l.State != api.LoginWindow || l.Via != "window" || l.Browser != "Cent Browser" {
			t.Fatalf("%s: login %+v %v", id, l, err)
		}
		if !modConfig(cfgs.Get().Providers, id).Enabled || !m.isNative(id) {
			t.Fatalf("%s: not switched on natively", id)
		}
		deadline := time.Now().Add(5 * time.Second)
		for {
			l, err = m.LoginStatus(t.Context(), id)
			if err == nil && l.State == api.LoginConnected {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: status %+v %v", id, l, err)
			}
			time.Sleep(10 * time.Millisecond)
		}
		if l.Account != want[id] {
			t.Fatalf("%s: account %q", id, l.Account)
		}
		var card api.PlatformStatus
		for time.Now().Before(deadline) {
			if card = platformState(t, m, id); card.State == api.PlatformConnected && card.Account == want[id] {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if card.State != api.PlatformConnected || card.Account != want[id] || card.Session != api.SessionWindow {
			t.Fatalf("%s: card %+v", id, card)
		}
	}
	if a := cfgs.Get().Providers.Nexus.Author; a != "UberMorgott" {
		t.Fatalf("nexus account stored as %q", a)
	}
	// «Выйти» clears the jar and the profile: the card turns read-only / signed out.
	p, err := m.Logout(t.Context(), curseforge.Platform, false)
	if err != nil || windows[curseforge.Platform].cleared != 1 || p.State == api.PlatformConnected && p.Session != api.SessionNone {
		t.Fatalf("logout %+v %v (cleared %d)", p, err, windows[curseforge.Platform].cleared)
	}
	// A second «Подключить» on a stored session connects silently.
	if l, err := m.Login(t.Context(), factorio.Platform); err != nil || l.State != api.LoginConnected {
		t.Fatalf("stored session: %+v %v", l, err)
	}
}

// Switching providers.nexus.engine swaps the provider live (same platform id).
func TestEngineSwitchLive(t *testing.T) {
	m, cfgs, group, dir := newTestPlatforms(t, nil)
	withNative(t, m, dir)
	on, err := cfgs.Patch(cfgs.Get().Revision, []byte(`{"providers":{"nexus":{"enabled":true,"mcp":{"command":"`+
		filepath.ToSlash(filepath.Join(dir, "missing.exe"))+`","args":[]}}}}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	m.apply(on)
	if m.isNative(nexus.Platform) {
		t.Fatal("engine mcp set by newTestPlatforms")
	}
	nat, err := cfgs.Patch(on.Revision, []byte(`{"providers":{"nexus":{"engine":"native"}}}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	m.apply(nat)
	if !m.isNative(nexus.Platform) || len(group.Syncers()) != 2 {
		t.Fatalf("native %v, syncers %d", m.isNative(nexus.Platform), len(group.Syncers()))
	}
	if p := platformState(t, m, factorio.Platform); p.Enabled || p.State != api.PlatformDisabled || p.Name != "Factorio Mod Portal" || p.Capabilities.Reply {
		t.Fatalf("factorio card %+v", p)
	}
}

// Owner-data switch: a native platform switched on without an author adopts
// the account of its stored source (synced by the MCP engine), so a signed-out
// start keeps reading keylessly as that account; an author already set wins.
func TestNativeAdoptsStoredSourceAccount(t *testing.T) {
	m, cfgs, _, dir := newTestPlatforms(t, nil)
	withNative(t, m, dir)
	for platform, account := range map[string]string{nexus.Platform: "UberMorgott", curseforge.Platform: "Morgott", factorio.Platform: "Morgott"} {
		if _, err := m.st.UpsertSource(t.Context(), platform, account); err != nil {
			t.Fatal(err)
		}
	}
	on, err := cfgs.Patch(cfgs.Get().Revision, []byte(`{"providers":{"nexus":{"enabled":true,"engine":"native","author":""},`+
		`"curseforge":{"enabled":true,"engine":"native","author":""},"factorio":{"enabled":true,"author":"Other"}}}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	m.apply(on)
	got := cfgs.Get().Providers
	if got.Nexus.Author != "UberMorgott" || got.CurseForge.Author != "Morgott" || got.Factorio.Author != "Other" {
		t.Fatalf("authors: nexus %q, curseforge %q, factorio %q", got.Nexus.Author, got.CurseForge.Author, got.Factorio.Author)
	}
	for id, want := range map[string]string{nexus.Platform: "UberMorgott", curseforge.Platform: "Morgott"} {
		if p, err := m.Check(t.Context(), id); err != nil || p.State != api.PlatformConnected || p.Account != want || p.Session != api.SessionNone {
			t.Fatalf("%s signed out: %+v %v", id, p, err)
		}
	}
}

// F9b: the portal user found by a one-click «Подключить» is kept in settings
// (providers.factorio.author): «Выйти» drops only the session, so public reads
// keep their account (also after a restart); «Отключить» clears it.
func TestFactorioAuthorSurvivesLogout(t *testing.T) {
	m, cfgs, _, dir := newTestPlatforms(t, nil)
	withNative(t, m, dir)
	if _, err := m.Login(t.Context(), factorio.Platform); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for cfgs.Get().Providers.Factorio.Author != "Morgott" {
		if time.Now().After(deadline) {
			t.Fatalf("factorio author = %q, want the signed-in user", cfgs.Get().Providers.Factorio.Author)
		}
		_, _ = m.LoginStatus(t.Context(), factorio.Platform)
		time.Sleep(10 * time.Millisecond)
	}
	p, err := m.Logout(t.Context(), factorio.Platform, false)
	if err != nil || cfgs.Get().Providers.Factorio.Author != "Morgott" || p.Account != "Morgott" || p.State != api.PlatformConnected {
		t.Fatalf("after «Выйти»: %+v %v, author %q", p, err, cfgs.Get().Providers.Factorio.Author)
	}
	if _, err := m.Logout(t.Context(), factorio.Platform, true); err != nil || cfgs.Get().Providers.Factorio.Author != "" {
		t.Fatalf("after «Отключить»: %v, author %q", err, cfgs.Get().Providers.Factorio.Author)
	}
}
