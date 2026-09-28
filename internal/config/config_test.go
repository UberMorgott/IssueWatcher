package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, File), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, dir string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, File)) //nolint:gosec // G304: test temp dir
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]any{}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestDefaultsWithoutFile(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := s.Get()
	if got.Revision != 0 || got.General.Language != "ru" || got.Sync.Mode != SyncBalanced || got.Sync.Plan("github").ActiveMinutes != 5 {
		t.Fatalf("defaults: %+v", got)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("defaults invalid: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, File)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("no change must not create the file")
	}
}

func TestMigrateV1(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, `{"pollIntervalMinutes": 15, "startWithWindows": true, "startMinimized": true, "future": {"x": 1}}`)
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := s.Get()
	if !got.General.StartWithWindows || !got.General.StartMinimized {
		t.Fatalf("general not migrated: %+v", got.General)
	}
	if got.Sync.Mode != SyncCustom || got.Sync.Plan("github").ActiveMinutes != 15 {
		t.Fatalf("poll interval not migrated: %+v", got.Sync)
	}
	raw := read(t, dir)
	if raw["schemaVersion"] != float64(SchemaVersion) || raw["pollIntervalMinutes"] != nil || raw["future"] == nil {
		t.Fatalf("file after migration: %v", raw)
	}
	if _, err := os.Stat(filepath.Join(dir, File+".bak")); err != nil {
		t.Fatalf("migration must keep a backup: %v", err)
	}

	// The default interval is today's balanced mode.
	dir2 := t.TempDir()
	write(t, dir2, `{"pollIntervalMinutes": 5}`)
	s2, err := Open(dir2)
	if err != nil || s2.Get().Sync.Mode != SyncBalanced {
		t.Fatalf("5 min → balanced: %+v %v", s2.Get().Sync, err)
	}
}

func TestPatchRevisionUnknownKeysAndBackup(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, `{"schemaVersion": 2, "revision": 3, "future": "keep", "general": {"language": "en", "nested": 7}}`)
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Patch(3, json.RawMessage(`{"general": {"startMinimized": true}, "revision": 99}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Revision != 4 || !got.General.StartMinimized || got.General.Language != "en" {
		t.Fatalf("patched: %+v", got)
	}
	raw := read(t, dir)
	general, _ := raw["general"].(map[string]any)
	if raw["future"] != "keep" || general["nested"] != float64(7) || raw["revision"] != float64(4) {
		t.Fatalf("unknown keys or revision lost: %v", raw)
	}
	bak, err := os.ReadFile(filepath.Join(dir, File+".bak")) //nolint:gosec // G304: test temp dir
	if err != nil || !strings.Contains(string(bak), `"revision": 3`) {
		t.Fatalf("backup: %s %v", bak, err)
	}
	if _, err := s.Patch(3, json.RawMessage(`{"general": {"startMinimized": false}}`), nil); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale revision: %v", err)
	}
	// Reopen reads the same document.
	s2, err := Open(dir)
	if err != nil || s2.Get().Revision != 4 || !s2.Get().General.StartMinimized {
		t.Fatalf("reopen: %+v %v", s2.Get(), err)
	}
}

func TestPatchValidation(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		`{"general": {"language": "de"}}`:                                  "general.language",
		`{"appearance": {"mode": "blue"}}`:                                 "appearance.mode",
		`{"notifications": {"quiet": {"enabled": true, "from": "25:00"}}}`: "notifications.quiet.from",
		`{"sync": {"mode": "turbo"}}`:                                      "sync.mode",
		`{"sync": {"providers": {"github": {"activeMinutes": 0.5}}}}`:      "",
		`{"sync": {"providers": {"github": {"idleMinutes": 2}}}}`:          "sync.providers.github.idleMinutes",
		`{"projects": {"roots": ["relative\\path"]}}`:                      "projects.roots.0",
		`[1]`: "",
	}
	for patch, field := range cases {
		_, err := s.Patch(0, json.RawMessage(patch), nil)
		if err == nil {
			t.Errorf("%s accepted", patch)
			continue
		}
		var ve *ValidationError
		if field != "" && (!errors.As(err, &ve) || ve.Field != field) {
			t.Errorf("%s: %v, want field %s", patch, err, field)
		}
	}
	if s.Get().Revision != 0 {
		t.Fatal("rejected patches must not bump the revision")
	}
	// before can veto; nothing is written.
	veto := errors.New("registry denied")
	if _, err := s.Patch(0, json.RawMessage(`{"general": {"startWithWindows": true}}`), func(_, _ Settings) error { return veto }); !errors.Is(err, veto) {
		t.Fatalf("veto: %v", err)
	}
	if s.Get().General.StartWithWindows {
		t.Fatal("vetoed change applied")
	}
}

func TestResetAndSubscribe(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var seen []int
	s.Subscribe(func(_, cur Settings) { seen = append(seen, cur.Revision) })
	if _, err := s.Patch(0, json.RawMessage(`{"sync": {"mode": "custom", "providers": {"github": {"activeMinutes": 3}}}}`), nil); err != nil {
		t.Fatal(err)
	}
	if p := s.Get().Sync.Plan("github"); p.ActiveMinutes != 3 || p.IdleMinutes != 30 {
		t.Fatalf("partial provider: %+v", p)
	}
	got, err := s.Reset(1, "sync", nil)
	if err != nil || got.Sync.Mode != SyncBalanced || got.Sync.Plan("github").ActiveMinutes != 5 {
		t.Fatalf("reset: %+v %v", got.Sync, err)
	}
	if _, err := s.Reset(2, "nope", nil); err == nil {
		t.Fatal("unknown section accepted")
	}
	if len(seen) != 2 || seen[1] != 2 {
		t.Fatalf("subscribers: %v", seen)
	}
}
