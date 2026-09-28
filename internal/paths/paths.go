// Package paths resolves the portable data directory.
//
// All runtime state (database, logs, WebView2 profile, later worktrees) lives
// in one folder so the app can be moved or deleted as a unit: nothing goes to
// the registry or AppData.
package paths

import (
	"fmt"
	"os"
	"path/filepath"
)

// EnvDataDir overrides the data directory (dev runs via `go run`, where the
// exe sits in a temp build folder).
const EnvDataDir = "IW_DATA_DIR"

// DataDir returns the data directory and creates it if missing:
// $IW_DATA_DIR when set, else `data` next to the executable.
func DataDir() (string, error) {
	dir := os.Getenv(EnvDataDir)
	if dir == "" {
		exe, err := os.Executable()
		if err != nil {
			return "", fmt.Errorf("paths: locate executable: %w", err)
		}
		if resolved, err := filepath.EvalSymlinks(exe); err == nil {
			exe = resolved
		}
		dir = filepath.Join(filepath.Dir(exe), "data")
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("paths: resolve %s: %w", dir, err)
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", fmt.Errorf("paths: create %s: %w", dir, err)
	}
	return dir, nil
}
