package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadSessionPersists(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "secrets")
	a, err := loadSession(dir)
	if err != nil || !validSecret(a) {
		t.Fatalf("first: %q %v", a, err)
	}
	b, err := loadSession(dir)
	if err != nil || b != a {
		t.Fatalf("second run got a new secret: %q vs %q (%v)", b, a, err)
	}
	// A damaged file is replaced, not fatal.
	if err := os.WriteFile(filepath.Join(dir, sessionFile), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := loadSession(dir)
	if err != nil || !validSecret(c) || c == a {
		t.Fatalf("damaged: %q %v", c, err)
	}
}
