package config

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestAgentsDefaultsAndMigration(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, `{"schemaVersion": 2, "general": {"language": "en"}}`)
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	a := s.Get().Agents
	if a.MaxParallel != 2 || a.Roles.Coder != "claude" || a.Roles.Responder != "codex" || len(a.Profiles) != 2 {
		t.Fatalf("defaults: %+v", a)
	}
	if read(t, dir)["schemaVersion"] != float64(SchemaVersion) {
		t.Fatal("v2 file not marked current")
	}
	// A partial profile gets defaults; an emptied prompt falls back to the default text.
	got, err := s.Patch(0, json.RawMessage(`{"agents": {"profiles": [{"id": "fast", "name": "Fast", "cli": "claude"}],
		"roles": {"coder": "fast", "responder": "fast"}, "prompts": {"fix": ""},
		"projects": {"octo/app": {"verify": "go test ./..."}}}}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	p, ok := got.Agents.Profile("fast")
	if !ok || p.TimeoutMinutes != 30 || p.MaxParallel != 1 || p.Args == nil || got.Agents.Prompts.Fix != DefaultFixPrompt ||
		got.Agents.Prompts.FixDir != DefaultFixDirectPrompt ||
		got.Agents.Projects["octo/app"].Verify != "go test ./..." || got.Agents.ModeFor("octo/app") != ModeDirect ||
		got.Agents.ModeFor("octo/unset") != ModeDirect {
		t.Fatalf("patched: %+v", got.Agents)
	}
	// Default prompt texts are not frozen into the file: a newer build's defaults apply.
	ag, _ := read(t, dir)["agents"].(map[string]any)
	pr, _ := ag["prompts"].(map[string]any)
	if pr["fix"] != "" || pr["system"] != "" || pr["fixDirect"] != "" {
		t.Fatalf("default prompts written to the file: %v", pr)
	}
	got, err = s.Patch(got.Revision, json.RawMessage(`{"agents": {"projects": {"octo/app": {"mode": "worktree-pr"}}}}`), nil)
	if err != nil || got.Agents.ModeFor("octo/app") != ModeWorktreePR || got.Agents.Projects["octo/app"].Verify != "go test ./..." {
		t.Fatalf("mode patch: %v %+v", err, got.Agents.Projects)
	}
}

// v3 → v4: projects that already have agent settings are marked direct (the
// new default), an explicit worktree-pr stays, unknown keys survive.
func TestAgentsModeMigration(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, `{"schemaVersion": 3, "agents": {"maxParallel": 3, "projects": {
		"octo/app": {"verify": "go test ./...", "future": 1}, "octo/pr": {"mode": "worktree-pr"}}}}`)
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	a := s.Get().Agents
	if a.MaxParallel != 3 || a.Projects["octo/app"].Mode != ModeDirect || a.Projects["octo/app"].Verify != "go test ./..." ||
		a.ModeFor("octo/pr") != ModeWorktreePR || a.ModeFor("octo/none") != ModeDirect {
		t.Fatalf("migrated: %+v", a.Projects)
	}
	raw := read(t, dir)
	ag, _ := raw["agents"].(map[string]any)
	projects, _ := ag["projects"].(map[string]any)
	app, _ := projects["octo/app"].(map[string]any)
	if raw["schemaVersion"] != float64(4) || app["mode"] != ModeDirect || app["future"] != float64(1) {
		t.Fatalf("file: %v", raw)
	}
	if _, err := s.Patch(0, json.RawMessage(`{"agents": {"projects": {"octo/app": {"mode": "yolo"}}}}`), nil); err == nil {
		t.Fatal("bad mode accepted")
	} else if ve := (*ValidationError)(nil); !errors.As(err, &ve) || ve.Field != "agents.projects.octo/app.mode" || ve.Code != "enum" {
		t.Fatalf("bad mode: %v", err)
	}
}

func TestAgentsValidation(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		`{"agents": {"maxParallel": 9}}`:                                                                                  "agents.maxParallel",
		`{"agents": {"profiles": [{"id": "Bad Id", "name": "x", "cli": "claude"}]}}`:                                      "agents.profiles.0.id",
		`{"agents": {"profiles": [{"id": "a", "name": "x", "cli": "gpt"}]}}`:                                              "agents.profiles.0.cli",
		`{"agents": {"profiles": [{"id": "a", "name": "x", "cli": "claude"}, {"id": "a", "name": "y", "cli": "codex"}]}}`: "agents.profiles.1.id",
		`{"agents": {"profiles": [{"id": "a", "name": "", "cli": "claude"}]}}`:                                            "agents.profiles.0.name",
		`{"agents": {"profiles": [{"id": "a", "name": "x", "cli": "claude", "timeoutMinutes": 999}]}}`:                    "agents.profiles.0.timeoutMinutes",
		`{"agents": {"roles": {"verifier": "ghost"}}}`:                                                                    "agents.roles.verifier",
	}
	for patch, field := range cases {
		_, err := s.Patch(0, json.RawMessage(patch), nil)
		var ve *ValidationError
		if !errors.As(err, &ve) || ve.Field != field {
			t.Errorf("%s: %v, want field %s", patch, err, field)
		}
	}
}
