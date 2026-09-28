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
	if read(t, dir)["schemaVersion"] != float64(3) {
		t.Fatal("v2 file not marked v3")
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
		got.Agents.Projects["octo/app"].Verify != "go test ./..." {
		t.Fatalf("patched: %+v", got.Agents)
	}
	// Default prompt texts are not frozen into the file: a newer build's defaults apply.
	ag, _ := read(t, dir)["agents"].(map[string]any)
	pr, _ := ag["prompts"].(map[string]any)
	if pr["fix"] != "" || pr["system"] != "" {
		t.Fatalf("default prompts written to the file: %v", pr)
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
