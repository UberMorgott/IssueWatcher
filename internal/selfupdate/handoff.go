package selfupdate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Hand-off files in the data dir between the old and the new process.
const (
	readyFile  = "update-ready"       // new process: "<pid>" once it runs
	resultFile = "update-result.json" // outcome of the last update, for the next start
)

// SignalReady tells the old process that the new executable started (pid).
func SignalReady(dataDir string, pid int) error {
	path := filepath.Join(dataDir, readyFile)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(strconv.Itoa(pid)), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ClearReady removes the ready signal.
func ClearReady(dataDir string) { _ = os.Remove(filepath.Join(dataDir, readyFile)) }

// waitReady waits until the ready file names pid, the process exits (exited
// closes) or ctx ends.
func waitReady(ctx context.Context, dataDir string, pid int, exited <-chan struct{}) error {
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		if b, err := os.ReadFile(filepath.Join(dataDir, readyFile)); err == nil && strings.TrimSpace(string(b)) == strconv.Itoa(pid) { //nolint:gosec // G304: fixed name in our data dir
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("the new version did not start in time: %w", ctx.Err())
		case <-exited:
			return errors.New("the new version exited right after starting")
		case <-tick.C:
		}
	}
}

// Result is the outcome of the last update, shown in Settings › Updates.
type Result struct {
	OK    bool      `json:"ok"`
	From  string    `json:"from,omitempty"`
	To    string    `json:"to,omitempty"`
	Error string    `json:"error,omitempty"`
	At    time.Time `json:"at"`
}

// WriteResult stores r for the process that starts next (a rolled-back old
// version reads why the update failed).
func WriteResult(dataDir string, r Result) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dataDir, resultFile), b, 0o600)
}

// TakeResult reads and removes the stored result; nil when there is none.
func TakeResult(dataDir string) *Result {
	path := filepath.Join(dataDir, resultFile)
	b, err := os.ReadFile(path) //nolint:gosec // G304: fixed name in our data dir
	if err != nil {
		return nil
	}
	_ = os.Remove(path)
	var r Result
	if json.Unmarshal(b, &r) != nil {
		return nil
	}
	return &r
}
