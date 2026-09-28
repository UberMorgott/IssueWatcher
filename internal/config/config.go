// Package config reads the user-editable settings file data\config.json.
// A missing file means defaults; unknown keys are ignored.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
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
