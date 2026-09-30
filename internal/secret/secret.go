// Package secret stores credentials (GitHub App keys, user tokens) as JSON
// files readable only by the current user. Contents are never logged.
package secret

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ErrNotFound is returned by ReadJSON when the file does not exist.
var ErrNotFound = errors.New("secret: not found")

// WriteJSON atomically writes v to path and restricts it to the current user.
func WriteJSON(path string, v any) error {
	body, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("secret: encode %s: %w", filepath.Base(path), err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("secret: create dir: %w", err)
	}
	if err := restrict(dir, true); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return fmt.Errorf("secret: write %s: %w", filepath.Base(tmp), err)
	}
	if err := restrict(tmp, false); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("secret: replace %s: %w", filepath.Base(path), err)
	}
	return nil
}

// ReadJSON decodes path into v; ErrNotFound when missing.
func ReadJSON(path string, v any) error {
	body, err := os.ReadFile(path) //nolint:gosec // G304: caller passes a fixed name inside our own data dir
	if errors.Is(err, os.ErrNotExist) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("secret: read %s: %w", filepath.Base(path), err)
	}
	if err := json.Unmarshal(body, v); err != nil {
		return fmt.Errorf("secret: decode %s: %w", filepath.Base(path), err)
	}
	return nil
}

// Remove deletes path; a missing file is not an error.
func Remove(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("secret: remove %s: %w", filepath.Base(path), err)
	}
	return nil
}

// RestrictDir creates dir (and parents) and restricts it to the current user,
// inherited by everything created inside (a browser profile).
func RestrictDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("secret: create dir: %w", err)
	}
	return restrict(dir, true)
}
