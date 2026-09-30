package main

import (
	"context"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/UberMorgott/issuewatcher/internal/api"
	"github.com/UberMorgott/issuewatcher/internal/config"
	"github.com/UberMorgott/issuewatcher/internal/provider/github"
	"github.com/UberMorgott/issuewatcher/internal/provider/mcpbridge/mcptest"
	"github.com/UberMorgott/issuewatcher/internal/provider/steam"
	"github.com/UberMorgott/issuewatcher/internal/store"
	"github.com/UberMorgott/issuewatcher/internal/syncer"
)

// A mod platform switched on or off in settings joins or leaves the running
// sync group without a restart; its card reports the state.
func TestModPlatformsApplyLive(t *testing.T) {
	m, cfgs, group, dir := newTestPlatforms(t, nil)
	state := func(id string) api.PlatformStatus {
		for _, p := range m.Platforms(t.Context()) {
			if p.ID == id {
				return p
			}
		}
		t.Fatalf("no %s card", id)
		return api.PlatformStatus{}
	}
	testApplyLive(t, m, cfgs, group, dir, state)
}

func newTestPlatforms(t *testing.T, fakes map[string]*mcptest.Server) (*modPlatforms, *config.Store, *syncer.Group, string) {
	t.Helper()
	dir := t.TempDir()
	cfgs, err := config.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	// The MCP engine tests keep the engine they test (native is the default
	// since the owner-data switch; the MCP engine goes in Phase 6 step 14).
	if _, err := cfgs.Patch(cfgs.Get().Revision, []byte(`{"providers":{"nexus":{"engine":"mcp"},"curseforge":{"engine":"mcp"}}}`), nil); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(t.Context(), filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := store.New(db)
	log := slog.New(slog.DiscardHandler)
	gh := github.NewProvider(newAuth(log, dir))
	stm := steam.New(steam.Options{Dir: filepath.Join(dir, "secrets"), Log: log})
	group := syncer.NewGroup(syncer.New(syncer.Options{Store: st, Provider: gh, Log: log}))
	m := newModPlatforms(cfgs, st, log, group, gh, stm, nil, dir)
	if fakes != nil {
		m.dial = func(id string) func(ctx context.Context) (mcp.Transport, error) { return fakes[id].Dial }
	}
	t.Cleanup(m.Close)
	return m, cfgs, group, dir
}

func testApplyLive(t *testing.T, m *modPlatforms, cfgs *config.Store, group *syncer.Group, dir string, state func(string) api.PlatformStatus) {
	t.Helper()
	if n := len(group.Syncers()); n != 1 {
		t.Fatalf("mod platforms are off by default, got %d syncers", n)
	}
	if p := state("nexus"); p.Enabled || p.State != api.PlatformDisabled || len(p.Capabilities.Kinds) != 2 {
		t.Fatalf("nexus off: %+v", p)
	}
	if p := state("steam"); p.Enabled || p.State != api.PlatformDisabled {
		t.Fatalf("steam without a SteamID: %+v", p)
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { group.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

	on, err := cfgs.Patch(cfgs.Get().Revision, []byte(`{"providers":{"nexus":{"enabled":true,"mcp":{"command":"`+
		filepath.ToSlash(filepath.Join(dir, "missing.exe"))+`","args":[]}}}}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	m.apply(on)
	if n := len(group.Syncers()); n != 2 {
		t.Fatalf("nexus on: %d syncers, want 2", n)
	}
	if p := state("nexus"); !p.Enabled {
		t.Fatalf("nexus on: %+v", p)
	}
	// No author set → the account comes from the server's session: no server → unavailable.
	if p, err := m.Check(t.Context(), "nexus"); err != nil || p.State != api.PlatformUnavailable || p.Error == "" {
		t.Fatalf("check without author: %+v %v", p, err)
	}
	withAuthor, err := cfgs.Patch(on.Revision, []byte(`{"providers":{"nexus":{"author":"Someone"}}}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	m.apply(withAuthor)
	if p, err := m.Check(t.Context(), "nexus"); err != nil || p.State != api.PlatformUnavailable {
		t.Fatalf("check with a missing server: %+v %v", p, err)
	}
	if _, err := m.Check(t.Context(), "github"); err == nil {
		t.Fatal("github has no platform check")
	}

	off, err := cfgs.Patch(withAuthor.Revision, []byte(`{"providers":{"nexus":{"enabled":false}}}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	m.apply(off)
	if n := len(group.Syncers()); n != 1 {
		t.Fatalf("nexus off again: %d syncers, want 1", n)
	}
	if p := state("nexus"); p.State != api.PlatformDisabled {
		t.Fatalf("nexus off again: %+v", p)
	}
}

func platformState(t *testing.T, m *modPlatforms, id string) api.PlatformStatus {
	t.Helper()
	for _, p := range m.Platforms(t.Context()) {
		if p.ID == id {
			return p
		}
	}
	t.Fatalf("no %s card", id)
	return api.PlatformStatus{}
}

// «Подключить» Nexus: the platform is switched on, the server's sign-in window
// opens, and once the user signed in there the member becomes the account
// (stored as providers.nexus.author) and the card turns connected.
func TestLoginNexusZeroSetup(t *testing.T) {
	fake := mcptest.New()
	var mu sync.Mutex
	window, signedIn := false, false
	fake.Handle("web_login", func(map[string]any) (any, error) {
		mu.Lock()
		defer mu.Unlock()
		window = true
		return map[string]any{"loggedIn": false, "loginWindowOpened": true, "loginInProgress": true, "detail": "window"}, nil
	})
	fake.Handle("web_status", func(map[string]any) (any, error) {
		mu.Lock()
		defer mu.Unlock()
		if !signedIn {
			signedIn = window // signs in inside the window before the next poll
			return map[string]any{"loggedIn": false, "loginInProgress": window, "cookiesStored": false, "detail": "UNAUTHORIZED", "account": nil}, nil
		}
		return map[string]any{"loggedIn": true, "loginInProgress": false, "cookiesStored": true, "detail": "session valid",
			"account": map[string]any{"memberId": 6541781, "name": "UberMorgott"}}, nil
	})
	fake.Handle("search_mods", func(args map[string]any) (any, error) {
		return map[string]any{"total": 1, "mods": []any{map[string]any{"game": "windrose", "modId": 147, "name": "ShareShip",
			"url": "https://www.nexusmods.com/windrose/mods/147", "uploader": map[string]any{"name": "UberMorgott", "memberId": 6541781}}}}, nil
	})
	m, cfgs, _, _ := newTestPlatforms(t, map[string]*mcptest.Server{"nexus": fake})

	l, err := m.Login(t.Context(), "nexus")
	if err != nil || l.State != api.LoginWindow {
		t.Fatalf("login %+v %v", l, err)
	}
	if !cfgs.Get().Providers.Nexus.Enabled {
		t.Fatal("login did not switch nexus on")
	}
	if l, err = m.LoginStatus(t.Context(), "nexus"); err != nil || l.State != api.LoginWindow {
		t.Fatalf("while signing in %+v %v", l, err)
	}
	if l, err = m.LoginStatus(t.Context(), "nexus"); err != nil || l.State != api.LoginConnected || l.Account != "UberMorgott" {
		t.Fatalf("after %+v %v", l, err)
	}
	if a := cfgs.Get().Providers.Nexus.Author; a != "6541781" {
		t.Fatalf("detected account stored as %q", a)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if p := platformState(t, m, "nexus"); p.State == api.PlatformConnected && p.Account == "UberMorgott" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("card %+v", platformState(t, m, "nexus"))
}

// The card says where the session came from; «Выйти» drops it (read-only,
// signed out), «Отключить» also forgets the account and switches the platform
// off — through the server's cf_logout, even when the platform is already off.
func TestLogoutCurseForge(t *testing.T) {
	fake := mcptest.New()
	var mu sync.Mutex
	loggedIn, logouts := true, 0
	fake.Handle("cf_session_status", func(map[string]any) (any, error) {
		mu.Lock()
		defer mu.Unlock()
		if !loggedIn {
			return map[string]any{"loggedIn": false, "cookiesStored": false, "detail": "no cookies", "user": nil,
				"loginInProgress": false, "sessionSource": nil, "sessionBrowser": nil}, nil
		}
		return map[string]any{"loggedIn": true, "cookiesStored": true, "detail": "ok", "loginInProgress": false,
			"user": map[string]any{"id": 1, "displayName": "Morgott"}, "sessionSource": "browser", "sessionBrowser": "Chrome"}, nil
	})
	fake.Handle("cf_logout", func(map[string]any) (any, error) {
		mu.Lock()
		defer mu.Unlock()
		loggedIn = false
		logouts++
		return map[string]any{"loggedOut": true, "cookiesStored": false}, nil
	})
	m, cfgs, _, _ := newTestPlatforms(t, map[string]*mcptest.Server{"curseforge": fake})
	if err := m.enable("curseforge"); err != nil {
		t.Fatal(err)
	}
	p, err := m.Check(t.Context(), "curseforge")
	if err != nil || p.State != api.PlatformConnected || p.Session != api.SessionBrowser || p.Browser != "Chrome" {
		t.Fatalf("check %+v %v", p, err)
	}
	if p, err = m.Logout(t.Context(), "curseforge", false); err != nil || p.State != api.PlatformSignedOut {
		t.Fatalf("logout %+v %v", p, err)
	}
	if !cfgs.Get().Providers.CurseForge.Enabled {
		t.Fatal("«Выйти» switched the platform off")
	}
	if p, err = m.Logout(t.Context(), "curseforge", true); err != nil || p.State != api.PlatformDisabled {
		t.Fatalf("forget %+v %v", p, err)
	}
	if c := cfgs.Get().Providers.CurseForge; c.Enabled || c.Author != "" {
		t.Fatalf("forget left %+v", c)
	}
	if _, err = m.Logout(t.Context(), "curseforge", true); err != nil { // off: a short-lived server runs cf_logout
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if logouts != 3 {
		t.Fatalf("cf_logout ran %d times, want 3", logouts)
	}
}

// No browser session: the server opens the login page in the default browser;
// the card says which, and «Отмена» stops the server's polling.
func TestLoginDefaultBrowserCancel(t *testing.T) {
	fake := mcptest.New()
	var mu sync.Mutex
	cancels := 0
	fake.Handle("cf_session_status", func(map[string]any) (any, error) {
		return map[string]any{"loggedIn": false, "cookiesStored": false, "detail": "no cookies", "user": nil,
			"loginInProgress": true, "loginVia": "default-browser", "loginBrowser": "firefox"}, nil
	})
	fake.Handle("cf_auto_extract_cookies", func(map[string]any) (any, error) {
		return map[string]any{"result": "opened", "loggedIn": false, "loginInProgress": true, "loginWindowOpened": true,
			"loginVia": "default-browser", "loginBrowser": "firefox"}, nil
	})
	fake.Handle("cf_login_cancel", func(map[string]any) (any, error) {
		mu.Lock()
		defer mu.Unlock()
		cancels++
		return map[string]any{"cancelled": true}, nil
	})
	m, _, _, _ := newTestPlatforms(t, map[string]*mcptest.Server{"curseforge": fake})
	l, err := m.Login(t.Context(), "curseforge")
	if err != nil || l.State != api.LoginWindow || l.Via != "default-browser" || l.Browser != "firefox" {
		t.Fatalf("login %+v %v", l, err)
	}
	if err := m.CancelLogin("curseforge"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if cancels != 1 {
		t.Fatalf("cf_login_cancel ran %d times", cancels)
	}
}

// An expired session shows «войдите снова» once, and again only after the
// platform was connected in between.
func TestReloginCardOnce(t *testing.T) {
	fake := mcptest.New()
	var mu sync.Mutex
	loggedIn := false
	fake.Handle("cf_session_status", func(map[string]any) (any, error) {
		mu.Lock()
		defer mu.Unlock()
		out := map[string]any{"loggedIn": loggedIn, "cookiesStored": true, "detail": "expired", "user": nil, "loginInProgress": false}
		if loggedIn {
			out["user"] = map[string]any{"id": 1, "displayName": "Morgott", "username": "user_x"}
		}
		return out, nil
	})
	fake.Handle("search_author", func(map[string]any) (any, error) {
		return map[string]any{"author": map[string]any{"id": 1, "username": "Morgott"}, "projects": []any{}}, nil
	})
	m, _, _, _ := newTestPlatforms(t, map[string]*mcptest.Server{"curseforge": fake})
	var cards []string
	m.onRelogin = func(id, name string) { mu.Lock(); cards = append(cards, id+"|"+name); mu.Unlock() }
	if err := m.enable("curseforge"); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if p, err := m.Check(t.Context(), "curseforge"); err != nil || p.State != api.PlatformRelogin {
			t.Fatalf("check %+v %v", p, err)
		}
	}
	mu.Lock()
	loggedIn = true
	mu.Unlock()
	if p, _ := m.Check(t.Context(), "curseforge"); p.State != api.PlatformConnected {
		t.Fatalf("connected %+v", p)
	}
	mu.Lock()
	loggedIn = false
	mu.Unlock()
	_, _ = m.Check(t.Context(), "curseforge")
	mu.Lock()
	defer mu.Unlock()
	if len(cards) != 2 || cards[0] != "curseforge|CurseForge" {
		t.Fatalf("cards %v", cards)
	}
}
