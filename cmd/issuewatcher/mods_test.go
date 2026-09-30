package main

import (
	"context"
	"log/slog"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/api"
	"github.com/UberMorgott/issuewatcher/internal/config"
	"github.com/UberMorgott/issuewatcher/internal/provider/curseforge"
	"github.com/UberMorgott/issuewatcher/internal/provider/factorio"
	"github.com/UberMorgott/issuewatcher/internal/provider/github"
	"github.com/UberMorgott/issuewatcher/internal/provider/steam"
	"github.com/UberMorgott/issuewatcher/internal/store"
	"github.com/UberMorgott/issuewatcher/internal/syncer"
)

// refuseSessions makes the fake sites refuse every stored session (expired).
var refuseSessions atomic.Bool

// A mod platform switched on or off in settings joins or leaves the running
// sync group without a restart; its card reports the state.
func TestModPlatformsApplyLive(t *testing.T) {
	m, cfgs, group, dir := newTestPlatforms(t)
	withNative(t, m, dir)
	if n := len(group.Syncers()); n != 1 {
		t.Fatalf("mod platforms are off by default, got %d syncers", n)
	}
	if p := platformState(t, m, "nexus"); p.Enabled || p.State != api.PlatformDisabled || len(p.Capabilities.Kinds) != 2 {
		t.Fatalf("nexus off: %+v", p)
	}
	if p := platformState(t, m, "steam"); p.Enabled || p.State != api.PlatformDisabled {
		t.Fatalf("steam without a SteamID: %+v", p)
	}
	if p := platformState(t, m, factorio.Platform); p.Enabled || p.State != api.PlatformDisabled || p.Name != "Factorio Mod Portal" || p.Capabilities.Reply {
		t.Fatalf("factorio card %+v", p)
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { group.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

	on, err := cfgs.Patch(cfgs.Get().Revision, []byte(`{"providers":{"nexus":{"enabled":true}}}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	m.apply(on)
	if n := len(group.Syncers()); n != 2 {
		t.Fatalf("nexus on: %d syncers, want 2", n)
	}
	if p := platformState(t, m, "nexus"); !p.Enabled {
		t.Fatalf("nexus on: %+v", p)
	}
	// No author, no session → signed out.
	if p, err := m.Check(t.Context(), "nexus"); err != nil || p.State != api.PlatformSignedOut || p.Error == "" {
		t.Fatalf("check without author: %+v %v", p, err)
	}
	withAuthor, err := cfgs.Patch(cfgs.Get().Revision, []byte(`{"providers":{"nexus":{"author":"UberMorgott"}}}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	m.apply(withAuthor)
	if p, err := m.Check(t.Context(), "nexus"); err != nil || p.State != api.PlatformConnected || p.Account != "UberMorgott" {
		t.Fatalf("check with an author: %+v %v", p, err)
	}
	if _, err := m.Check(t.Context(), "github"); err == nil {
		t.Fatal("github has no platform check")
	}

	off, err := cfgs.Patch(cfgs.Get().Revision, []byte(`{"providers":{"nexus":{"enabled":false}}}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	m.apply(off)
	if n := len(group.Syncers()); n != 1 {
		t.Fatalf("nexus off again: %d syncers, want 1", n)
	}
	if p := platformState(t, m, "nexus"); p.State != api.PlatformDisabled {
		t.Fatalf("nexus off again: %+v", p)
	}
}

func newTestPlatforms(t *testing.T) (*modPlatforms, *config.Store, *syncer.Group, string) {
	t.Helper()
	dir := t.TempDir()
	cfgs, err := config.Open(dir)
	if err != nil {
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
	t.Cleanup(m.Close)
	return m, cfgs, group, dir
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

// signIn runs «Подключить» until the platform is connected.
func signIn(t *testing.T, m *modPlatforms, id string) {
	t.Helper()
	if _, err := m.Login(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if l, err := m.LoginStatus(t.Context(), id); err == nil && l.State == api.LoginConnected {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: not connected", id)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// «Выйти» drops the session (the platform stays on), «Отключить» also forgets
// the account and switches the platform off — and still clears the stored
// session when the platform is already off.
func TestLogoutCurseForge(t *testing.T) {
	m, cfgs, _, dir := newTestPlatforms(t)
	windows := withNative(t, m, dir)
	signIn(t, m, curseforge.Platform)
	if p, err := m.Logout(t.Context(), curseforge.Platform, false); err != nil || p.State == api.PlatformConnected && p.Session != api.SessionNone {
		t.Fatalf("logout %+v %v", p, err)
	}
	if !cfgs.Get().Providers.CurseForge.Enabled {
		t.Fatal("«Выйти» switched the platform off")
	}
	if p, err := m.Logout(t.Context(), curseforge.Platform, true); err != nil || p.State != api.PlatformDisabled {
		t.Fatalf("forget %+v %v", p, err)
	}
	if c := cfgs.Get().Providers.CurseForge; c.Enabled || c.Author != "" {
		t.Fatalf("forget left %+v", c)
	}
	if _, err := m.Logout(t.Context(), curseforge.Platform, true); err != nil { // off: a short-lived provider clears it
		t.Fatal(err)
	}
	if n := windows[curseforge.Platform].cleared; n != 3 {
		t.Fatalf("session cleared %d times, want 3", n)
	}
}

// An expired session shows «войдите снова» once, and again only after the
// platform was connected in between.
func TestReloginCardOnce(t *testing.T) {
	m, _, _, dir := newTestPlatforms(t)
	withNative(t, m, dir)
	var mu sync.Mutex
	var cards []string
	m.onRelogin = func(id, name string) { mu.Lock(); cards = append(cards, id+"|"+name); mu.Unlock() }
	signIn(t, m, curseforge.Platform)
	t.Cleanup(func() { refuseSessions.Store(false) })
	refuseSessions.Store(true)
	for range 2 {
		if p, err := m.Check(t.Context(), curseforge.Platform); err != nil || p.State != api.PlatformRelogin {
			t.Fatalf("check %+v %v", p, err)
		}
	}
	refuseSessions.Store(false)
	if p, _ := m.Check(t.Context(), curseforge.Platform); p.State != api.PlatformConnected {
		t.Fatalf("connected %+v", p)
	}
	refuseSessions.Store(true)
	_, _ = m.Check(t.Context(), curseforge.Platform)
	mu.Lock()
	defer mu.Unlock()
	if len(cards) != 2 || cards[0] != "curseforge|CurseForge" {
		t.Fatalf("cards %v", cards)
	}
}
