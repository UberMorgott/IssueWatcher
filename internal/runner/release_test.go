package runner

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/UberMorgott/issuewatcher/internal/config"
)

// The release run's folder lock: one holder per folder (case-insensitive),
// a running job's folder is busy, queued jobs of a held folder wait.
func TestAcquireFolder(t *testing.T) {
	r := New(Options{Settings: config.Defaults, DataDir: t.TempDir()})
	dir := filepath.Join(t.TempDir(), "Mod")
	release, err := r.AcquireFolder(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.AcquireFolder(filepath.Join(filepath.Dir(dir), "mod")); !errors.Is(err, ErrFolderBusy) {
		t.Fatalf("second holder: %v", err)
	}
	release()
	release() // idempotent
	again, err := r.AcquireFolder(dir)
	if err != nil {
		t.Fatalf("after release: %v", err)
	}
	again()

	r.mu.Lock()
	r.running[1] = &activeRun{folder: folderKey(dir)}
	r.mu.Unlock()
	if _, err := r.AcquireFolder(dir); !errors.Is(err, ErrFolderBusy) {
		t.Fatalf("job running there: %v", err)
	}
}
