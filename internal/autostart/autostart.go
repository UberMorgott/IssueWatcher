// Package autostart manages the "start with Windows" entry: a value under
// HKCU\Software\Microsoft\Windows\CurrentVersion\Run that launches this exe with
// --minimized. It is the one registry write of the portable app: present only
// while the setting is on, removed when it is turned off.
package autostart

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows/registry"
)

// RunKey is the per-user autostart key (relative to HKCU).
const RunKey = `Software\Microsoft\Windows\CurrentVersion\Run`

// ValueName is the Run value this app owns.
const ValueName = "IssueWatcher"

// MinimizedFlag starts the app in the tray without opening the dashboard.
const MinimizedFlag = "--minimized"

// Entry is one Run value: Key is relative to HKCU (tests use their own key).
type Entry struct {
	Key  string
	Name string
}

// Default is the real autostart entry.
var Default = Entry{Key: RunKey, Name: ValueName}

// Command is the Run value for exe: quoted path plus --minimized.
func Command(exe string) string { return `"` + exe + `" ` + MinimizedFlag }

// ExePath extracts the executable from a Run command line ("" if unparsable).
func ExePath(cmd string) string {
	cmd = strings.TrimSpace(cmd)
	if strings.HasPrefix(cmd, `"`) {
		if end := strings.Index(cmd[1:], `"`); end >= 0 {
			return cmd[1 : end+1]
		}
		return ""
	}
	if i := strings.IndexByte(cmd, ' '); i >= 0 {
		return cmd[:i]
	}
	return cmd
}

// SamePath compares Windows paths (case-insensitive, cleaned).
func SamePath(a, b string) bool {
	return a != "" && b != "" && strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
}

// Get returns the stored command ("" when the value does not exist).
func (e Entry) Get() (string, error) {
	k, err := registry.OpenKey(registry.CURRENT_USER, e.Key, registry.QUERY_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("autostart: open: %w", err)
	}
	defer func() { _ = k.Close() }()
	v, _, err := k.GetStringValue(e.Name)
	if errors.Is(err, registry.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("autostart: read: %w", err)
	}
	return v, nil
}

// Enabled reports whether the entry exists and starts exe.
func (e Entry) Enabled(exe string) (bool, error) {
	v, err := e.Get()
	return err == nil && SamePath(ExePath(v), exe), err
}

// Set writes (on) or deletes (off) the entry for exe.
func (e Entry) Set(on bool, exe string) error {
	if !on {
		k, err := registry.OpenKey(registry.CURRENT_USER, e.Key, registry.SET_VALUE)
		if errors.Is(err, registry.ErrNotExist) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("autostart: open: %w", err)
		}
		defer func() { _ = k.Close() }()
		if err := k.DeleteValue(e.Name); err != nil && !errors.Is(err, registry.ErrNotExist) {
			return fmt.Errorf("autostart: delete: %w", err)
		}
		return nil
	}
	k, _, err := registry.CreateKey(registry.CURRENT_USER, e.Key, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("autostart: create: %w", err)
	}
	defer func() { _ = k.Close() }()
	if err := k.SetStringValue(e.Name, Command(exe)); err != nil {
		return fmt.Errorf("autostart: write: %w", err)
	}
	return nil
}

// Refresh keeps an enabled entry pointing at exe after the portable folder was
// moved: when the setting is on and the stored path differs, it is rewritten.
// It reports whether it wrote.
func (e Entry) Refresh(on bool, exe string) (bool, error) {
	if !on {
		return false, nil
	}
	v, err := e.Get()
	if err != nil {
		return false, err
	}
	if v == Command(exe) {
		return false, nil
	}
	return true, e.Set(true, exe)
}
