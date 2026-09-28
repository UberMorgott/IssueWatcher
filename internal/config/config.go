// Package config reads the user-editable settings file data\config.json.
// A missing file means defaults; unknown keys are ignored.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"time"
)

// File is the settings file name inside the data dir.
const File = "config.json"

// MinPollInterval protects the GitHub quota from a typo.
const MinPollInterval = time.Minute

// Config holds user settings.
type Config struct {
	// PollIntervalMinutes is the GitHub sync period (default 5, minimum 1).
	PollIntervalMinutes int `json:"pollIntervalMinutes"`
	// StartWithWindows keeps the HKCU Run entry (internal/autostart) pointing at
	// this exe; the entry itself is the source of truth for the UI.
	StartWithWindows bool `json:"startWithWindows"`
	// StartMinimized starts in the tray without opening the dashboard (the
	// --minimized flag forces it either way).
	StartMinimized bool `json:"startMinimized"`
}

// Update applies patch (JSON field name → value) to dataDir\config.json,
// keeping keys it does not know, and writes it atomically.
func Update(dataDir string, patch map[string]any) error {
	path := filepath.Join(dataDir, File)
	raw := map[string]any{}
	body, err := os.ReadFile(path) //nolint:gosec // G304: fixed name inside our own data dir
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return fmt.Errorf("config: read: %w", err)
	default:
		if err := json.Unmarshal(body, &raw); err != nil {
			return fmt.Errorf("config: %s: %w", File, err)
		}
	}
	maps.Copy(raw, patch)
	out, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return fmt.Errorf("config: encode: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(out, '\n'), 0o600); err != nil {
		return fmt.Errorf("config: write: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("config: replace: %w", err)
	}
	return nil
}

// PollInterval is the effective sync period.
func (c Config) PollInterval() time.Duration {
	if c.PollIntervalMinutes <= 0 {
		return 5 * time.Minute
	}
	return max(time.Duration(c.PollIntervalMinutes)*time.Minute, MinPollInterval)
}

// Load reads dataDir\config.json.
func Load(dataDir string) (Config, error) {
	var c Config
	body, err := os.ReadFile(filepath.Join(dataDir, File)) //nolint:gosec // G304: fixed name inside our own data dir
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, fmt.Errorf("config: read: %w", err)
	}
	if err := json.Unmarshal(body, &c); err != nil {
		return c, fmt.Errorf("config: %s: %w", File, err)
	}
	return c, nil
}
