// Package instance keeps one running copy per data directory and publishes how
// to reach it (data\runtime.json) for a second launch and, later, the MCP bridge.
package instance

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// RuntimeFile is the name of the runtime descriptor inside the data dir.
const RuntimeFile = "runtime.json"

// Runtime describes the running instance. Token authorizes loopback API calls.
type Runtime struct {
	PID       int       `json:"pid"`
	Port      int       `json:"port"`
	URL       string    `json:"url"`
	Token     string    `json:"token"`
	Version   string    `json:"version"`
	StartedAt time.Time `json:"startedAt"`
}

// ErrAlreadyRunning is returned by Acquire when another instance owns the data dir.
var ErrAlreadyRunning = errors.New("instance: already running")

// WriteRuntime atomically writes data\runtime.json (owner-only permissions).
func WriteRuntime(dataDir string, rt Runtime) error {
	body, err := json.MarshalIndent(rt, "", "  ")
	if err != nil {
		return fmt.Errorf("instance: encode runtime: %w", err)
	}
	path := filepath.Join(dataDir, RuntimeFile)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return fmt.Errorf("instance: write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("instance: replace %s: %w", path, err)
	}
	return nil
}

// ReadRuntime reads data\runtime.json.
func ReadRuntime(dataDir string) (Runtime, error) {
	var rt Runtime
	body, err := os.ReadFile(filepath.Join(dataDir, RuntimeFile)) //nolint:gosec // G304: fixed name inside our own data dir
	if err != nil {
		return rt, fmt.Errorf("instance: read runtime: %w", err)
	}
	if err := json.Unmarshal(body, &rt); err != nil {
		return rt, fmt.Errorf("instance: decode runtime: %w", err)
	}
	return rt, nil
}

// RemoveRuntime deletes data\runtime.json if it still belongs to pid.
func RemoveRuntime(dataDir string, pid int) error {
	rt, err := ReadRuntime(dataDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if rt.PID != pid {
		return nil // a newer instance owns it
	}
	if err := os.Remove(filepath.Join(dataDir, RuntimeFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("instance: remove runtime: %w", err)
	}
	return nil
}

// PortFile keeps the loopback port of the last run (runtime.json goes away
// on exit): a restart prefers it, and the restart after a self-update
// requires it, so open dashboard tabs reconnect to the same origin.
const PortFile = "port"

// WritePort records the bound port.
func WritePort(dataDir string, port int) error {
	if err := os.WriteFile(filepath.Join(dataDir, PortFile), []byte(strconv.Itoa(port)), 0o600); err != nil {
		return fmt.Errorf("instance: write port: %w", err)
	}
	return nil
}

// ReadPort returns the last recorded port, 0 when none.
func ReadPort(dataDir string) int {
	b, err := os.ReadFile(filepath.Join(dataDir, PortFile)) //nolint:gosec // G304: fixed name inside our own data dir
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || n <= 0 || n > 65535 {
		return 0
	}
	return n
}