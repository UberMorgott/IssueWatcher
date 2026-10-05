// Package config owns data\config.json: the typed, validated, versioned app
// settings. Unknown keys survive every write (a newer build's settings are not
// lost when an older one saves), writes are atomic with a .bak of the previous
// file, and a revision counter lets two dashboard tabs detect conflicting edits.
// Secrets and the runtime token never live here (data\secrets, runtime.json).
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
)

// File is the settings file name inside the data dir.
const File = "config.json"

// SchemaVersion is the current layout of config.json.
//
//	0/1: flat {pollIntervalMinutes, startWithWindows, startMinimized}
//	2:   sections (general, appearance, notifications, sync, projects, updates)
//	3:   + agents (profiles, roles, prompts, per-project prompt/verify); nothing moves
//	4:   + agents.projects.*.mode (direct | worktree-pr); existing entries → direct
//	5:   + agents.prompts.label, agents.automation (off, caps, rules), agents.projects.*.automation
//	     overrides; only defaults are added
//	6:   project keys source-qualified: agents.projects keys and rule projects owner/repo → github:owner/repo
//	7:   providers.nexus/curseforge lose mcp + engine (the MCP engine is gone; native only)
//	8:   + agents.autopilot (paused, caps), agents.projects.*.autopilot and .publishProfile;
//	     only defaults are added (docs/AUTOPILOT.md → Config schema)
const SchemaVersion = 8

// ErrConflict means the caller edited an older revision.
var ErrConflict = errors.New("config: settings changed elsewhere")

// ValidationError names the offending field (JSON path) and why: Code and
// Params let the UI show a localised message, Msg is the English text.
type ValidationError struct {
	Field  string         `json:"field"`
	Code   string         `json:"code"` // json | enum | range | time | differ | absPath | tooMany | section
	Params map[string]any `json:"params,omitempty"`
	Msg    string         `json:"error"`
}

func (e *ValidationError) Error() string { return "config: " + e.Field + ": " + e.Msg }

func invalid(field, code string, params map[string]any, format string, a ...any) error {
	return &ValidationError{Field: field, Code: code, Params: params, Msg: fmt.Sprintf(format, a...)}
}

func outOfRange(field string, lo, hi int) error {
	return invalid(field, "range", map[string]any{"min": lo, "max": hi}, "must be %d–%d", lo, hi)
}

func notOneOf(field string, values ...string) error {
	return invalid(field, "enum", map[string]any{"values": strings.Join(values, ", ")}, "must be one of %s", strings.Join(values, ", "))
}

// Store is the live settings document of one data dir.
type Store struct {
	mu   sync.Mutex
	dir  string
	cur  Settings
	raw  map[string]any // file content incl. unknown keys
	subs []func(old, cur Settings)
}

// Open loads (and migrates) dataDir\config.json. A missing file means defaults.
func Open(dataDir string) (*Store, error) {
	s := &Store{dir: dataDir}
	raw, err := readRaw(filepath.Join(dataDir, File))
	if err != nil {
		return nil, err
	}
	migrated := migrate(raw)
	cur, err := decode(raw)
	if err != nil {
		return nil, err
	}
	s.cur, s.raw = cur, raw
	if migrated {
		if err := s.write(); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// Get returns the current settings.
func (s *Store) Get() Settings {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cur
}

// Subscribe registers f, called after every successful change (outside the lock).
func (s *Store) Subscribe(f func(old, cur Settings)) {
	s.mu.Lock()
	s.subs = append(s.subs, f)
	s.mu.Unlock()
}

// Patch applies a JSON merge patch (RFC 7396) to revision rev. before runs with
// the validated result ahead of the write and may veto it (side effects that can
// fail, such as the autostart entry). The revision increases by one.
func (s *Store) Patch(rev int, patch json.RawMessage, before func(old, next Settings) error) (Settings, error) {
	var p map[string]any
	if err := json.Unmarshal(patch, &p); err != nil || p == nil {
		return Settings{}, invalid("", "json", nil, "patch must be a JSON object")
	}
	for _, k := range []string{"schemaVersion", "revision"} {
		delete(p, k)
	}
	return s.change(rev, func(raw map[string]any) { mergePatch(raw, p) }, before)
}

// Reset restores the defaults of one top-level section at revision rev.
func (s *Store) Reset(rev int, section string, before func(old, next Settings) error) (Settings, error) {
	def, err := toMap(Defaults())
	if err != nil {
		return Settings{}, err
	}
	d, ok := def[section]
	if !ok || section == "schemaVersion" || section == "revision" {
		return Settings{}, invalid("section", "section", map[string]any{"section": section}, "unknown section %q", section)
	}
	return s.change(rev, func(raw map[string]any) { raw[section] = d }, before)
}

func (s *Store) change(rev int, edit func(map[string]any), before func(old, next Settings) error) (Settings, error) {
	s.mu.Lock()
	if rev != s.cur.Revision {
		cur := s.cur
		s.mu.Unlock()
		return cur, ErrConflict
	}
	raw := deepCopy(s.raw)
	edit(raw)
	next, err := decode(raw)
	if err != nil {
		err = invalid("", "json", nil, "%v", err)
	} else {
		err = next.Validate()
	}
	if err == nil {
		err = checkNoSecrets(raw)
	}
	if err == nil && before != nil {
		err = before(s.cur, next)
	}
	if err != nil {
		s.mu.Unlock()
		return Settings{}, err
	}
	next.Revision = s.cur.Revision + 1
	old, oldRaw := s.cur, s.raw
	s.cur, s.raw = next, raw
	if err := s.write(); err != nil {
		s.cur, s.raw = old, oldRaw
		s.mu.Unlock()
		return Settings{}, err
	}
	subs := append([]func(old, cur Settings){}, s.subs...)
	s.mu.Unlock()
	for _, f := range subs {
		f(old, next)
	}
	return next, nil
}
