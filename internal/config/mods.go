package config

import "strings"

// Mod platform engines (Phase 6): "mcp" = the owner's MCP servers (old),
// "native" = built-in Go providers over the installed browser.
const (
	EngineMCP    = "mcp"
	EngineNative = "native"
)

// Providers: per-platform connection settings of the mod platforms
// (docs/ARCHITECTURE.md → Mod platforms, Native mod platforms).
type Providers struct {
	Nexus      ModPlatform    `json:"nexus"`
	CurseForge ModPlatform    `json:"curseforge"`
	Factorio   NativePlatform `json:"factorio"`
}

// ModPlatform is one Nexus / CurseForge platform. Every field applies live:
// Enabled adds or removes the platform's syncer, a new command restarts the
// server, a new engine rebuilds the provider.
type ModPlatform struct {
	Enabled bool      `json:"enabled"`
	MCP     MCPServer `json:"mcp"`
	// Engine picks the implementation: EngineMCP (default until the owner-data
	// switch) or EngineNative.
	Engine string `json:"engine"`
	// Author is whose projects are listed: Nexus = the exact uploader account
	// name or member id (required; not the mod's free-text author field, which
	// anyone can set); CurseForge = the CFWidget author, default the session's
	// display name.
	Author string `json:"author,omitempty"`
}

// NativePlatform is a platform with only the native engine (Factorio).
type NativePlatform struct {
	Enabled bool `json:"enabled"`
	// Author is the portal username whose mods are listed (default: the
	// signed-in account).
	Author string `json:"author,omitempty"`
}

// MCPServer is a stdio MCP server command line.
type MCPServer struct {
	Command string   `json:"command"`
	Args    []string `json:"args"`
}

// Default server locations: the owner's repos, as in their .mcp.json.
func defaultProviders() Providers {
	return Providers{
		Nexus:      ModPlatform{MCP: MCPServer{Command: "node", Args: []string{`E:\DEV\nexusmods-mcp-server\build\index.js`}}, Engine: EngineMCP},
		CurseForge: ModPlatform{MCP: MCPServer{Command: "node", Args: []string{`E:\DEV\curseforge\build\index.js`}}, Engine: EngineMCP},
	}
}

func (p *Providers) normalize() {
	d := defaultProviders()
	for _, x := range []struct{ v, def *ModPlatform }{{&p.Nexus, &d.Nexus}, {&p.CurseForge, &d.CurseForge}} {
		x.v.MCP.Command = strings.TrimSpace(x.v.MCP.Command)
		if x.v.MCP.Command == "" {
			x.v.MCP = x.def.MCP
		}
		if x.v.MCP.Args == nil {
			x.v.MCP.Args = []string{}
		}
		x.v.Author = strings.TrimSpace(x.v.Author)
		x.v.Engine = strings.ToLower(strings.TrimSpace(x.v.Engine))
		if x.v.Engine == "" {
			x.v.Engine = x.def.Engine
		}
	}
	p.Factorio.Author = strings.TrimSpace(p.Factorio.Author)
}

func (p Providers) validate() error {
	for id, m := range map[string]ModPlatform{"nexus": p.Nexus, "curseforge": p.CurseForge} {
		f := "providers." + id
		switch {
		case len(m.MCP.Command) > 1024:
			return invalid(f+".mcp.command", "tooLong", map[string]any{"max": 1024}, "at most 1024 characters")
		case len(m.MCP.Args) > 32:
			return invalid(f+".mcp.args", "tooMany", map[string]any{"max": 32}, "at most 32 arguments")
		case len(m.Author) > 100:
			return invalid(f+".author", "tooLong", map[string]any{"max": 100}, "at most 100 characters")
		case m.Engine != EngineMCP && m.Engine != EngineNative:
			return notOneOf(f+".engine", EngineMCP, EngineNative)
		}
	}
	if len(p.Factorio.Author) > 100 {
		return invalid("providers.factorio.author", "tooLong", map[string]any{"max": 100}, "at most 100 characters")
	}
	return nil
}
