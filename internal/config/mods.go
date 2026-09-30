package config

import "strings"

// Providers: per-platform connection settings of the mod platforms
// (docs/ARCHITECTURE.md → Native mod platforms).
type Providers struct {
	Nexus      ModPlatform    `json:"nexus"`
	CurseForge ModPlatform    `json:"curseforge"`
	Factorio   NativePlatform `json:"factorio"`
}

// ModPlatform is one Nexus / CurseForge platform. Every field applies live:
// Enabled adds or removes the platform's syncer.
type ModPlatform struct {
	Enabled bool `json:"enabled"`
	// Author is whose projects are listed: Nexus = the exact uploader account
	// name or member id (not the mod's free-text author field, which anyone can
	// set; default the signed-in member); CurseForge = the CFWidget author,
	// default the session's display name.
	Author string `json:"author,omitempty"`
}

// NativePlatform is a platform with only the native engine (Factorio).
type NativePlatform struct {
	Enabled bool `json:"enabled"`
	// Author is the portal username whose mods are listed (default: the
	// signed-in account).
	Author string `json:"author,omitempty"`
}

func (p *Providers) normalize() {
	p.Nexus.Author = strings.TrimSpace(p.Nexus.Author)
	p.CurseForge.Author = strings.TrimSpace(p.CurseForge.Author)
	p.Factorio.Author = strings.TrimSpace(p.Factorio.Author)
}

func (p Providers) validate() error {
	for f, a := range map[string]string{"nexus": p.Nexus.Author, "curseforge": p.CurseForge.Author, "factorio": p.Factorio.Author} {
		if len(a) > 100 {
			return invalid("providers."+f+".author", "tooLong", map[string]any{"max": 100}, "at most 100 characters")
		}
	}
	return nil
}
