package main

import (
	"log/slog"
	"os"

	"github.com/UberMorgott/issuewatcher/internal/config"
	"github.com/UberMorgott/issuewatcher/internal/provider/mcpbridge"
	"github.com/UberMorgott/issuewatcher/internal/provider/nexus"
	"github.com/UberMorgott/issuewatcher/internal/store"
	"github.com/UberMorgott/issuewatcher/internal/syncer"
)

// modPlatforms are the syncers of the MCP-backed mod platforms enabled in
// settings (providers.<id>.enabled, read at startup) and their server bridges.
type modPlatforms struct {
	syncers []*syncer.Syncer
	bridges []*mcpbridge.Client
}

// Close stops the MCP server children.
func (m *modPlatforms) Close() {
	for _, b := range m.bridges {
		b.Close()
	}
}

func newModPlatforms(cfgs *config.Store, st *store.Store, log *slog.Logger, plan syncer.Plan,
	onUpdate func([]store.Event, int),
) *modPlatforms {
	m := &modPlatforms{}
	cfg := cfgs.Get().Providers
	// The server command is re-read on every server start.
	command := func(pick func(config.Providers) config.MCPServer) func() (string, []string) {
		return func() (string, []string) {
			s := pick(cfgs.Get().Providers)
			return os.ExpandEnv(s.Command), s.Args
		}
	}
	if cfg.Nexus.Enabled {
		b := mcpbridge.New(mcpbridge.Options{Name: nexus.Platform, Log: log,
			Command: command(func(p config.Providers) config.MCPServer { return p.Nexus.MCP })})
		p := nexus.New(nexus.Options{Bridge: b, Log: log, Author: func() string { return cfgs.Get().Providers.Nexus.Author }})
		m.bridges = append(m.bridges, b)
		m.syncers = append(m.syncers, syncer.New(syncer.Options{Store: st, Provider: p, Plan: plan, Log: log, OnUpdate: onUpdate}))
	}
	return m
}
