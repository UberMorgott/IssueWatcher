package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	c, err := Load(dir)
	if err != nil || c.PollInterval() != 5*time.Minute {
		t.Fatalf("defaults: %v %v", c.PollInterval(), err)
	}
	if err := os.WriteFile(filepath.Join(dir, File), []byte(`{"pollIntervalMinutes": 15}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if c, err = Load(dir); err != nil || c.PollInterval() != 15*time.Minute {
		t.Fatalf("15m: %v %v", c.PollInterval(), err)
	}
	if err := os.WriteFile(filepath.Join(dir, File), []byte(`{`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err = Load(dir); err == nil {
		t.Fatal("bad json accepted")
	}
}

func TestUpdateKeepsUnknownKeys(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, File), []byte(`{"pollIntervalMinutes": 7, "future": "x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Update(dir, map[string]any{"startMinimized": true}); err != nil {
		t.Fatal(err)
	}
	c, err := Load(dir)
	if err != nil || !c.StartMinimized || c.PollIntervalMinutes != 7 {
		t.Fatalf("%+v %v", c, err)
	}
	body, _ := os.ReadFile(filepath.Join(dir, File)) //nolint:gosec // G304: test temp dir
	if !strings.Contains(string(body), `"future": "x"`) {
		t.Fatalf("unknown key lost: %s", body)
	}
	if err := Update(t.TempDir(), map[string]any{"startWithWindows": true}); err != nil {
		t.Fatalf("missing file: %v", err)
	}
}
