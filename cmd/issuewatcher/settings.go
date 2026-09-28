package main

import (
	"errors"
	"strings"
	"sync"

	"github.com/UberMorgott/issuewatcher/internal/api"
	"github.com/UberMorgott/issuewatcher/internal/autostart"
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

// appSettings implements api.SettingsStore over config.json and the Run entry.
type appSettings struct {
	mu      sync.Mutex
	dataDir string
	exe     string
	entry   autostart.Entry
}

func (a *appSettings) Settings() (api.Settings, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.read()
}

func (a *appSettings) read() (api.Settings, error) {
	cfg, err := config.Load(a.dataDir)
	on, rerr := a.entry.Enabled(a.exe)
	return api.Settings{
		StartWithWindows:    on,
		StartMinimized:      cfg.StartMinimized,
		PollIntervalMinutes: int(cfg.PollInterval().Minutes()),
	}, errors.Join(err, rerr)
}

func (a *appSettings) UpdateSettings(p api.SettingsPatch) (api.Settings, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	patch := map[string]any{}
	if p.StartWithWindows != nil {
		if err := a.entry.Set(*p.StartWithWindows, a.exe); err != nil {
			return api.Settings{}, err
		}
		patch["startWithWindows"] = *p.StartWithWindows
	}
	if p.StartMinimized != nil {
		patch["startMinimized"] = *p.StartMinimized
	}
	if len(patch) > 0 {
		if err := config.Update(a.dataDir, patch); err != nil {
			return api.Settings{}, err
		}
	}
	return a.read()
}
