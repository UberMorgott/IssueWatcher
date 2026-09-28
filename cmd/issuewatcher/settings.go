package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"

	"github.com/UberMorgott/issuewatcher/internal/api"
	"github.com/UberMorgott/issuewatcher/internal/config"
)

// startMinimized decides whether this launch stays in the tray: the
// --minimized flag (also --minimized=true|false) wins over the setting.
func startMinimized(args []string, setting bool) bool {
	for _, a := range args {
		switch strings.ToLower(strings.TrimLeft(a, "-/")) {
		case "minimized", "minimized=true":
			return true
		case "minimized=false":
			return false
		}
	}
	return setting
}

// appSettings implements api.SettingsStore over config.Store plus the side
// effects that live outside config.json (the autostart Run entry).
type appSettings struct {
	mu      sync.Mutex // one change at a time (entry.Set + write)
	store   *config.Store
	dataDir string
	exe     string
	version string
	entry   runEntry
}

func (a *appSettings) doc(s config.Settings) api.SettingsDoc {
	// The Run entry is the truth for "start with Windows" (the user may have
	// removed it in Task Manager).
	if on, err := a.entry.Enabled(a.exe); err == nil {
		s.General.StartWithWindows = on
	}
	return api.SettingsDoc{
		Revision: s.Revision,
		Settings: s,
		Info: api.SettingsInfo{
			DataDir: a.dataDir, ConfigFile: filepath.Join(a.dataDir, config.File), Version: a.version, Exe: a.exe,
			SyncPresets: config.Presets(), Palettes: config.Palettes(),
		},
	}
}

func (a *appSettings) Settings() (api.SettingsDoc, error) {
	return a.doc(a.store.Get()), nil
}

// PatchSettings writes the Run entry first when the patch names
// general.startWithWindows, so a failed registry write saves nothing.
func (a *appSettings) PatchSettings(rev int, patch json.RawMessage) (api.SettingsDoc, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	var p struct {
		General *struct {
			StartWithWindows *bool `json:"startWithWindows"`
		} `json:"general"`
	}
	_ = json.Unmarshal(patch, &p) // shape errors are reported by Patch
	var before func(old, next config.Settings) error
	if p.General != nil && p.General.StartWithWindows != nil {
		before = func(_, next config.Settings) error { return a.entry.Set(next.General.StartWithWindows, a.exe) }
	}
	s, err := a.store.Patch(rev, patch, before)
	if err != nil {
		return api.SettingsDoc{}, err
	}
	return a.doc(s), nil
}

func (a *appSettings) ResetSettings(rev int, section string) (api.SettingsDoc, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	var before func(old, next config.Settings) error
	if section == "general" {
		before = func(_, next config.Settings) error { return a.entry.Set(next.General.StartWithWindows, a.exe) }
	}
	s, err := a.store.Reset(rev, section, before)
	if err != nil {
		return api.SettingsDoc{}, err
	}
	return a.doc(s), nil
}
