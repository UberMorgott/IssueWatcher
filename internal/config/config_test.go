package config

import (
	"os"
	"path/filepath"
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
