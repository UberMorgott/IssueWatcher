package config

import (
	"maps"
	"path/filepath"
	"strings"
	"time"
)

// Settings is config.json. Every field has a default (Defaults), so a partial
// or older file always decodes to a complete document.
type Settings struct {
	SchemaVersion int           `json:"schemaVersion"`
	Revision      int           `json:"revision"` // bumped by every write; guards concurrent tabs
	General       General       `json:"general"`
	Appearance    Appearance    `json:"appearance"`
	Notifications Notifications `json:"notifications"`
	Sync          Sync          `json:"sync"`
	Projects      Projects      `json:"projects"`
	Updates       Updates       `json:"updates"`
	Agents        Agents        `json:"agents"`
	Providers     Providers     `json:"providers"`
}

// Updates: self-update from GitHub releases (internal/selfupdate). Checks
// only; installing is always the user's click.
type Updates struct {
	Channel       string `json:"channel"`       // stable | preview
	AutoCheck     bool   `json:"autoCheck"`     // check every IntervalHours (with jitter)
	IntervalHours int    `json:"intervalHours"` // 1–168
}

// General: language and startup.
type General struct {
	Language string `json:"language"` // ru | en
	// StartWithWindows keeps the HKCU Run entry (internal/autostart) pointing at
	// this exe; the entry itself is the source of truth shown in the UI.
	StartWithWindows bool `json:"startWithWindows"`
	// StartMinimized starts in the tray without opening the dashboard (the
	// --minimized flag forces it either way).
	StartMinimized bool `json:"startMinimized"`
}

// Appearance: theme mode, palette preset, custom colours per mode, typography.
type Appearance struct {
	Mode      string `json:"mode"`      // dark | light | system
	PaletteID string `json:"paletteId"` // a Palettes() id
	// Custom overrides palette colours per mode; empty fields use the palette.
	Custom     CustomColors `json:"custom"`
	FontFamily string       `json:"fontFamily"` // inter | segoe | system | mono
	FontScale  float64      `json:"fontScale"`  // 0.85–1.30
	Density    string       `json:"density"`    // compact | comfortable | spacious
}

// CustomColors are the per-mode overrides of the custom editor.
type CustomColors struct {
	Dark  Colors `json:"dark"`
	Light Colors `json:"light"`
}

// Notifications: which sync events pop up.
type Notifications struct {
	Enabled       bool       `json:"enabled"`
	NewIssue      bool       `json:"newIssue"`
	NewComment    bool       `json:"newComment"`
	Closed        bool       `json:"closed"`
	MutedProjects []string   `json:"mutedProjects"` // project names (owner/repo): no popups, still unread
	Quiet         QuietHours `json:"quiet"`
	// Group collapses several events of one item from one sync into one popup.
	Group bool `json:"group"`
	// AutoHideSeconds: how long a popup stays.
	AutoHideSeconds int `json:"autoHideSeconds"`
	// RespectDnd skips popups while Windows is busy (full screen, presentation).
	RespectDnd bool `json:"respectDnd"`
}

// QuietHours mutes popups between From and To (local time, may wrap past midnight).
type QuietHours struct {
	Enabled bool   `json:"enabled"`
	From    string `json:"from"` // HH:MM
	To      string `json:"to"`   // HH:MM
}

// Sync modes.
const (
	SyncBalanced = "balanced"
	SyncFast     = "fast"
	SyncCustom   = "custom"
)

// Sync: how often each provider is polled (see internal/syncer).
type Sync struct {
	Mode string `json:"mode"` // balanced | fast | custom
	// ActiveDays: a project with activity in the last N days is polled on the
	// active interval, the rest on the idle one.
	ActiveDays int                     `json:"activeDays"`
	Providers  map[string]ProviderSync `json:"providers"` // custom values per provider id
}

// ProviderSync is the polling plan of one provider.
type ProviderSync struct {
	ActiveMinutes    int `json:"activeMinutes"`    // cheap change check, active projects
	IdleMinutes      int `json:"idleMinutes"`      // cheap change check, quiet projects
	ReconcileMinutes int `json:"reconcileMinutes"` // full re-read of everything
	HourlyBudget     int `json:"hourlyBudget"`     // max requests per hour the poller may spend
	Concurrency      int `json:"concurrency"`      // projects checked in parallel
}

// Projects: local folder discovery.
type Projects struct {
	Roots     []string `json:"roots"`     // folders scanned for git clones
	Exclude   []string `json:"exclude"`   // folder names skipped while scanning
	ScanDepth int      `json:"scanDepth"` // levels below a root
}

func defaultGitHub() ProviderSync {
	return ProviderSync{ActiveMinutes: 5, IdleMinutes: 30, ReconcileMinutes: 60, HourlyBudget: 1500, Concurrency: 4}
}

// presets are the fixed plans behind the balanced and fast modes.
var presets = map[string]ProviderSync{
	SyncBalanced: defaultGitHub(),
	SyncFast:     {ActiveMinutes: 2, IdleMinutes: 15, ReconcileMinutes: 30, HourlyBudget: 3000, Concurrency: 6},
}

// Presets are the fixed plans of the balanced and fast modes (shown in the UI).
func Presets() map[string]ProviderSync { return maps.Clone(presets) }

// Defaults is a fresh install.
func Defaults() Settings {
	return Settings{
		SchemaVersion: SchemaVersion,
		General:       General{Language: "ru"},
		Appearance:    Appearance{Mode: "dark", PaletteID: "indigo", FontFamily: "inter", FontScale: 1, Density: "comfortable"},
		Notifications: Notifications{
			Enabled: true, NewIssue: true, NewComment: true, Closed: true, MutedProjects: []string{},
			Quiet: QuietHours{From: "22:00", To: "08:00"}, Group: true, AutoHideSeconds: 8,
		},
		Sync:      Sync{Mode: SyncBalanced, ActiveDays: 14, Providers: map[string]ProviderSync{"github": defaultGitHub()}},
		Updates:   Updates{Channel: "stable", AutoCheck: true, IntervalHours: 24},
		Agents:    defaultAgents(),
		Providers: defaultProviders(),
		Projects: Projects{
			Roots: []string{}, ScanDepth: 3,
			Exclude: []string{"node_modules", ".git", "vendor", "bin", "obj", "build", "dist", "Library", "Temp"},
		},
	}
}

// Plan is the effective polling plan of provider id (presets win outside custom mode).
func (s Sync) Plan(id string) ProviderSync {
	if p, ok := presets[s.Mode]; ok {
		return p
	}
	if p, ok := s.Providers[id]; ok {
		return p
	}
	return defaultGitHub()
}

// Duration helpers for a plan.
func (p ProviderSync) Active() time.Duration { return time.Duration(p.ActiveMinutes) * time.Minute }
func (p ProviderSync) Idle() time.Duration   { return time.Duration(p.IdleMinutes) * time.Minute }
func (p ProviderSync) Reconcile() time.Duration {
	return time.Duration(p.ReconcileMinutes) * time.Minute
}

func (s *Settings) normalize() {
	s.Agents.normalize()
	s.Providers.normalize()
	if s.Notifications.MutedProjects == nil {
		s.Notifications.MutedProjects = []string{}
	}
	if s.Projects.Roots == nil {
		s.Projects.Roots = []string{}
	}
	if s.Projects.Exclude == nil {
		s.Projects.Exclude = []string{}
	}
	if s.Sync.Providers == nil {
		s.Sync.Providers = map[string]ProviderSync{}
	}
	if _, ok := s.Sync.Providers["github"]; !ok {
		s.Sync.Providers["github"] = defaultGitHub()
	}
	// A partial provider object decodes with zero fields: fill them from the defaults.
	d := defaultGitHub()
	for id, p := range s.Sync.Providers {
		for _, f := range []struct{ v, def *int }{
			{&p.ActiveMinutes, &d.ActiveMinutes}, {&p.IdleMinutes, &d.IdleMinutes},
			{&p.ReconcileMinutes, &d.ReconcileMinutes}, {&p.HourlyBudget, &d.HourlyBudget}, {&p.Concurrency, &d.Concurrency},
		} {
			if *f.v == 0 {
				*f.v = *f.def
			}
		}
		s.Sync.Providers[id] = p
	}
	for i, r := range s.Projects.Roots {
		s.Projects.Roots[i] = filepath.Clean(strings.TrimSpace(r))
	}
}
