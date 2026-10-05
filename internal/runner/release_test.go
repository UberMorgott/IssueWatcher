package runner

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/UberMorgott/issuewatcher/internal/config"
	"github.com/UberMorgott/issuewatcher/internal/store"
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
	// launch re-checks under the lock: a job of a held folder is not started
	// (Options.Store is nil here: reaching UpdateJob would panic).
	r.launch(t.Context(), store.Job{ID: 7, LocalPath: dir})
	r.mu.Lock()
	_, started := r.running[7]
	r.mu.Unlock()
	if started {
		t.Fatal("job launched in a held folder")
	}
	again()

	r.mu.Lock()
	r.running[1] = &activeRun{folder: folderKey(dir)}
	r.mu.Unlock()
	if _, err := r.AcquireFolder(dir); !errors.Is(err, ErrFolderBusy) {
		t.Fatalf("job running there: %v", err)
	}
}

// The release gate expands the placeholders of the project's verify command;
// an expand error fails the gate without running anything.
func TestGateExpandsVerifyCommand(t *testing.T) {
	cfg := config.Defaults()
	cfg.Agents.Projects = map[string]config.ProjectAgent{"github:o/r": {Verify: "echo v={version}"}}
	r := New(Options{Settings: func() config.Settings { return cfg }, DataDir: t.TempDir(),
		LookPath: func(string) (string, error) { return "", errors.New("no aegis") }})
	dir := t.TempDir()
	expand := func(c string) (string, error) { return strings.ReplaceAll(c, "{version}", "1.2.3"), nil }
	res, ran := r.Gate(t.Context(), "github:o/r", dir, dir, filepath.Join(dir, "logs"), expand)
	if !ran || !res.OK || res.Command != "echo v=1.2.3" || !strings.Contains(res.Output, "v=1.2.3") {
		t.Fatalf("ran %v %+v", ran, res)
	}
	res, ran = r.Gate(t.Context(), "github:o/r", dir, dir, filepath.Join(dir, "logs"),
		func(string) (string, error) { return "", errors.New("unsafe value") })
	if !ran || res.OK || res.ExitCode != -1 || res.Output != "unsafe value" {
		t.Fatalf("expand error: ran %v %+v", ran, res)
	}
}
