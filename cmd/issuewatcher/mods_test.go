package main

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/UberMorgott/issuewatcher/internal/api"
	"github.com/UberMorgott/issuewatcher/internal/config"
	"github.com/UberMorgott/issuewatcher/internal/provider/github"
	"github.com/UberMorgott/issuewatcher/internal/provider/steam"
	"github.com/UberMorgott/issuewatcher/internal/store"
	"github.com/UberMorgott/issuewatcher/internal/syncer"
)

// A mod platform switched on or off in settings joins or leaves the running
// sync group without a restart; its card reports the state.
func TestModPlatformsApplyLive(t *testing.T) {
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
	m := newModPlatforms(cfgs, st, log, group, gh, stm, nil)
	t.Cleanup(m.Close)

	state := func(id string) api.PlatformStatus {
		for _, p := range m.Platforms(t.Context()) {
			if p.ID == id {
				return p
			}
		}
		t.Fatalf("no %s card", id)
		return api.PlatformStatus{}
	}
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
	// No author set → signed out before the server is even started.
	if p, err := m.Check(t.Context(), "nexus"); err != nil || p.State != api.PlatformSignedOut || p.Error == "" {
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
