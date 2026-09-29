package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/api"
	"github.com/UberMorgott/issuewatcher/internal/config"
	"github.com/UberMorgott/issuewatcher/internal/provider"
	"github.com/UberMorgott/issuewatcher/internal/provider/curseforge"
	"github.com/UberMorgott/issuewatcher/internal/provider/mcpbridge"
	"github.com/UberMorgott/issuewatcher/internal/provider/nexus"
	"github.com/UberMorgott/issuewatcher/internal/provider/steam"
	"github.com/UberMorgott/issuewatcher/internal/store"
	"github.com/UberMorgott/issuewatcher/internal/syncer"
)

// checkTimeout bounds one «Проверить» (an MCP server may start a browser first).
const checkTimeout = 90 * time.Second

// modPlatform is one running MCP-backed platform.
type modPlatform struct {
	bridge *mcpbridge.Client
	prov   provider.Provider
	syncer *syncer.Syncer
	cfg    config.ModPlatform
}

// modPlatforms runs the MCP-backed mod platforms switched on in settings
// (providers.<id>.enabled) and reports every platform's state for Settings ›
// Платформы. A switch, a new server command or author applies live: the
// platform's syncer is added to or removed from the group, its server child
// restarted on the next call.
type modPlatforms struct {
	cfgs     *config.Store
	st       *store.Store
	log      *slog.Logger
	group    *syncer.Group
	gh       provider.Provider
	steam    *steam.Provider
	onUpdate func([]store.Event, int)

	mu     sync.Mutex
	live   map[string]*modPlatform
	checks map[string]api.PlatformStatus // last «Проверить» result per platform
}

func newModPlatforms(cfgs *config.Store, st *store.Store, log *slog.Logger, group *syncer.Group,
	gh provider.Provider, stm *steam.Provider, onUpdate func([]store.Event, int),
) *modPlatforms {
	m := &modPlatforms{cfgs: cfgs, st: st, log: log, group: group, gh: gh, steam: stm, onUpdate: onUpdate,
		live: map[string]*modPlatform{}, checks: map[string]api.PlatformStatus{}}
	m.apply(cfgs.Get())
	return m
}

// Close stops the MCP server children.
func (m *modPlatforms) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, p := range m.live {
		p.bridge.Close()
	}
}

func modConfig(p config.Providers, id string) config.ModPlatform {
	if id == nexus.Platform {
		return p.Nexus
	}
	return p.CurseForge
}

// apply brings the running platforms in line with the settings.
func (m *modPlatforms) apply(cfg config.Settings) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, id := range []string{nexus.Platform, curseforge.Platform} {
		want := modConfig(cfg.Providers, id)
		cur := m.live[id]
		switch {
		case want.Enabled && cur == nil:
			p := m.start(id, cfg)
			m.live[id] = p
			m.group.Add(p.syncer)
			m.log.Info("mod platform on", "platform", id)
		case !want.Enabled && cur != nil:
			m.group.Remove(id)
			cur.bridge.Close()
			delete(m.live, id)
			delete(m.checks, id)
			m.log.Info("mod platform off", "platform", id)
		case cur != nil && (want.MCP.Command != cur.cfg.MCP.Command || !slices.Equal(want.MCP.Args, cur.cfg.MCP.Args)):
			cur.bridge.Close() // the next call starts the new command
			cur.cfg = want
			delete(m.checks, id)
			cur.syncer.Trigger()
		case cur != nil && want.Author != cur.cfg.Author:
			cur.cfg = want
			delete(m.checks, id)
			cur.syncer.Trigger()
		}
	}
}

// start builds a platform's bridge, provider and syncer. m.mu must be held.
func (m *modPlatforms) start(id string, cfg config.Settings) *modPlatform {
	pick := func() config.ModPlatform { return modConfig(m.cfgs.Get().Providers, id) }
	b := mcpbridge.New(mcpbridge.Options{Name: id, Log: m.log, Command: func() (string, []string) {
		s := pick().MCP
		return os.ExpandEnv(s.Command), s.Args
	}})
	author := func() string { return pick().Author }
	var p provider.Provider
	if id == nexus.Platform {
		p = nexus.New(nexus.Options{Bridge: b, Log: m.log, Author: author})
	} else {
		p = curseforge.New(curseforge.Options{Bridge: b, Log: m.log, Author: author})
	}
	s := syncer.New(syncer.Options{Store: m.st, Provider: p, Plan: syncPlan(cfg.Sync), Log: m.log, OnUpdate: m.onUpdate})
	return &modPlatform{bridge: b, prov: p, syncer: s, cfg: modConfig(cfg.Providers, id)}
}

var platformNames = map[string]string{
	"github": "GitHub", nexus.Platform: "Nexus Mods", curseforge.Platform: "CurseForge", steam.Platform: "Steam Workshop",
}

// Platforms implements api.Platforms: every platform, GitHub first.
func (m *modPlatforms) Platforms(ctx context.Context) []api.PlatformStatus {
	sources := map[string]syncer.SourceStatus{}
	projects := map[string]int{}
	for _, s := range m.group.Status().Sources {
		sources[s.Platform] = s
	}
	if repos, err := m.st.Repos(ctx); err == nil {
		for _, r := range repos {
			projects[r.Platform]++
		}
	}
	cfg := m.cfgs.Get().Providers
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]api.PlatformStatus, 0, 4)
	for _, id := range []string{"github", nexus.Platform, curseforge.Platform, steam.Platform} {
		ps := api.PlatformStatus{ID: id, Name: platformNames[id], Enabled: true, Projects: projects[id]}
		switch id {
		case "github":
			ps.Capabilities = m.gh.Capabilities()
		case steam.Platform:
			ps.Capabilities = m.steam.Capabilities()
			st, err := m.steam.Status()
			ps.Enabled = err == nil && st.Configured
		default:
			ps.Enabled = modConfig(cfg, id).Enabled
			if p := m.live[id]; p != nil {
				ps.Capabilities = p.prov.Capabilities()
				ps.Running = p.bridge.Running()
			} else if id == nexus.Platform {
				ps.Capabilities = nexus.New(nexus.Options{}).Capabilities()
			} else {
				ps.Capabilities = curseforge.New(curseforge.Options{}).Capabilities()
			}
		}
		if ps.Capabilities.Kinds == nil {
			ps.Capabilities.Kinds = []string{}
		}
		src, synced := sources[id]
		switch {
		case !ps.Enabled:
			ps.State = api.PlatformDisabled
		case m.checks[id].State != "":
			c := m.checks[id]
			ps.State, ps.Account, ps.Error, ps.CheckedAt = c.State, c.Account, c.Error, c.CheckedAt
		case synced && src.Account != "" && src.LastError == "":
			ps.State, ps.Account = api.PlatformConnected, src.Account
		case synced && src.LastError != "":
			ps.State, ps.Error = api.PlatformError, src.LastError
		default:
			ps.State = api.PlatformUnknown
		}
		if synced {
			ps.LastSync = src.LastSync
		}
		out = append(out, ps)
	}
	return out
}

// Check implements api.Platforms: a live account check of one platform.
func (m *modPlatforms) Check(ctx context.Context, id string) (api.PlatformStatus, error) {
	var p provider.Provider
	m.mu.Lock()
	switch id {
	case steam.Platform:
		p = m.steam
	case nexus.Platform, curseforge.Platform:
		if mp := m.live[id]; mp != nil {
			p = mp.prov
		}
	}
	m.mu.Unlock()
	if id != steam.Platform && id != nexus.Platform && id != curseforge.Platform {
		return api.PlatformStatus{}, api.ErrUnknownPlatform
	}
	res := api.PlatformStatus{ID: id, CheckedAt: time.Now().UTC().Format(time.RFC3339)}
	if p == nil {
		res.State = api.PlatformDisabled
		return m.finish(ctx, id, res), nil
	}
	ctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	var err error
	if id == steam.Platform {
		// Steam's account is just the saved SteamID: read the Workshop list instead.
		if res.Account, err = p.Account(ctx); err == nil {
			_, err = p.ListProjects(ctx)
		}
	} else {
		res.Account, err = p.Account(ctx)
	}
	res.State, res.Error = checkState(err)
	if err == nil {
		m.group.Trigger() // read what the account now reaches
	}
	return m.finish(ctx, id, res), nil
}

func (m *modPlatforms) finish(ctx context.Context, id string, res api.PlatformStatus) api.PlatformStatus {
	m.mu.Lock()
	m.checks[id] = res
	m.mu.Unlock()
	for _, ps := range m.Platforms(ctx) {
		if ps.ID == id {
			return ps
		}
	}
	return res
}

// checkState maps an Account error to a platform state and its reason.
func checkState(err error) (string, string) {
	switch {
	case err == nil:
		return api.PlatformConnected, ""
	case errors.Is(err, mcpbridge.ErrUnavailable):
		return api.PlatformUnavailable, err.Error()
	case errors.Is(err, curseforge.ErrRelogin), errors.Is(err, steam.ErrSessionExpired):
		return api.PlatformRelogin, err.Error()
	case errors.Is(err, provider.ErrNotSignedIn):
		return api.PlatformSignedOut, err.Error()
	default:
		return api.PlatformError, err.Error()
	}
}
