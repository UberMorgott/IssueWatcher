package selfupdate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// checkFile keeps the last successful check across restarts (and across the
// restart into a new version), so Settings never says "not checked yet" right
// after a check or an update.
const checkFile = "update-check.json"

type lastCheck struct {
	Channel   string    `json:"channel"`
	CheckedAt time.Time `json:"checkedAt"`
	Release   *Release  `json:"release,omitempty"`
}

// saveCheck stores the outcome of a successful check; errors only lose the
// cache.
func saveCheck(dataDir string, c lastCheck) error {
	if dataDir == "" {
		return nil
	}
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	path := filepath.Join(dataDir, checkFile)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// loadCheck returns the stored check of channel; ok is false when there is
// none or it belongs to another channel.
func loadCheck(dataDir, channel string) (lastCheck, bool) {
	if dataDir == "" {
		return lastCheck{}, false
	}
	b, err := os.ReadFile(filepath.Join(dataDir, checkFile)) //nolint:gosec // G304: fixed name in our data dir
	if err != nil {
		return lastCheck{}, false
	}
	var c lastCheck
	if json.Unmarshal(b, &c) != nil || c.CheckedAt.IsZero() || c.Channel != channel {
		return lastCheck{}, false
	}
	return c, true
}
