package config

import "strings"

// Providers: per-platform connection settings of the mod platforms reached
// through the owner's MCP servers (docs/ARCHITECTURE.md → Mod platforms).
type Providers struct {
	Nexus      ModPlatform `json:"nexus"`
	CurseForge ModPlatform `json:"curseforge"`
}

// ModPlatform is one MCP-backed platform. Every field applies live: Enabled
// adds or removes the platform's syncer, a new command restarts the server.
type ModPlatform struct {
	Enabled bool      `json:"enabled"`
	MCP     MCPServer `json:"mcp"`
	// Author is whose projects are listed: Nexus = the exact uploader account
	// name or member id (required; not the mod's free-text author field, which
	// anyone can set); CurseForge = the CFWidget author, default the session's
	// display name.
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
		Nexus:      ModPlatform{MCP: MCPServer{Command: "node", Args: []string{`E:\DEV\nexusmods-mcp-server\build\index.js`}}},
		CurseForge: ModPlatform{MCP: MCPServer{Command: "node", Args: []string{`E:\DEV\curseforge\build\index.js`}}},
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
	}
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
		}
	}
	return nil
}
