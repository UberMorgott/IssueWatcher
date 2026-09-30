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
		"projects": {"github:octo/app": {"verify": "go test ./..."}}}}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	p, ok := got.Agents.Profile("fast")
	if !ok || p.TimeoutMinutes != 30 || p.MaxParallel != 1 || p.Args == nil || got.Agents.Prompts.Fix != DefaultFixPrompt ||
		got.Agents.Prompts.FixDir != DefaultFixDirectPrompt ||
		got.Agents.Projects["github:octo/app"].Verify != "go test ./..." || got.Agents.ModeFor("github:octo/app") != ModeDirect ||
		got.Agents.ModeFor("github:octo/unset") != ModeDirect {
		t.Fatalf("patched: %+v", got.Agents)
	}
	// Default prompt texts are not frozen into the file: a newer build's defaults apply.
	ag, _ := read(t, dir)["agents"].(map[string]any)
	pr, _ := ag["prompts"].(map[string]any)
	if pr["fix"] != "" || pr["system"] != "" || pr["fixDirect"] != "" {
		t.Fatalf("default prompts written to the file: %v", pr)
	}
	got, err = s.Patch(got.Revision, json.RawMessage(`{"agents": {"projects": {"github:octo/app": {"mode": "worktree-pr"}}}}`), nil)
	if err != nil || got.Agents.ModeFor("github:octo/app") != ModeWorktreePR || got.Agents.Projects["github:octo/app"].Verify != "go test ./..." {
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
	if a.MaxParallel != 3 || a.Projects["github:octo/app"].Mode != ModeDirect || a.Projects["github:octo/app"].Verify != "go test ./..." ||
		a.ModeFor("github:octo/pr") != ModeWorktreePR || a.ModeFor("github:octo/none") != ModeDirect {
		t.Fatalf("migrated: %+v", a.Projects)
	}
	raw := read(t, dir)
	ag, _ := raw["agents"].(map[string]any)
	projects, _ := ag["projects"].(map[string]any)
	app, _ := projects["github:octo/app"].(map[string]any)
	if raw["schemaVersion"] != float64(SchemaVersion) || app["mode"] != ModeDirect || app["future"] != float64(1) {
		t.Fatalf("file: %v", raw)
	}
	if _, err := s.Patch(0, json.RawMessage(`{"agents": {"projects": {"github:octo/app": {"mode": "yolo"}}}}`), nil); err == nil {
		t.Fatal("bad mode accepted")
	} else if ve := (*ValidationError)(nil); !errors.As(err, &ve) || ve.Field != "agents.projects.github:octo/app.mode" || ve.Code != "enum" {
		t.Fatalf("bad mode: %v", err)
	}
}

// v4 → v5: automation defaults (off) and the label prompt are added, unknown
// keys and existing agent settings survive.
func TestAutomationMigration(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, `{"schemaVersion": 4, "future": "keep", "agents": {"maxParallel": 3, "prompts": {"reply": "mine"},
		"projects": {"octo/app": {"mode": "worktree-pr", "future": 1}}, "automation": {"nested": true}}}`)
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	a := s.Get().Agents
	au := a.Automation
	if au.Enabled || au.MaxPerDay != 10 || au.MaxAttempts != 2 || au.AllowAutoFix || au.AutoApplyLabels || au.Rules == nil || len(au.Rules) != 0 {
		t.Fatalf("automation defaults: %+v", au)
	}
	if a.MaxParallel != 3 || a.Prompts.Reply != "mine" || a.Prompts.Label != DefaultLabelPrompt || a.ModeFor("github:octo/app") != ModeWorktreePR {
		t.Fatalf("agents: %+v", a)
	}
	if p := a.AutomationFor("github:octo/app"); p != (AutomationPolicy{TotalPerDay: 10, MaxAttempts: 2}) {
		t.Fatalf("inherited policy: %+v", p)
	}
	raw := read(t, dir)
	ag, _ := raw["agents"].(map[string]any)
	auto, _ := ag["automation"].(map[string]any)
	pr, _ := ag["prompts"].(map[string]any)
	projects, _ := ag["projects"].(map[string]any)
	app, _ := projects["github:octo/app"].(map[string]any)
	if raw["schemaVersion"] != float64(SchemaVersion) || raw["future"] != "keep" || auto["nested"] != true ||
		auto["enabled"] != false || auto["maxPerDay"] != float64(10) || pr["label"] != "" || app["future"] != float64(1) {
		t.Fatalf("file: %v", raw)
	}
	if _, ok := app["automation"]; ok {
		t.Fatalf("empty project override written: %v", app)
	}
}

// v5 → v6: owner/repo project keys and rule projects become github:owner/repo;
// an existing qualified key wins over its legacy twin; unknown keys survive.
func TestProjectKeysMigration(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, `{"schemaVersion": 5, "future": "keep", "agents": {"projects": {
		"octo/app": {"mode": "worktree-pr", "future": 1}, "octo/lib": {"verify": "legacy"}, "github:octo/lib": {"verify": "new"},
		"nexus:skyrim/42": {"prompt": "mod"}},
		"automation": {"rules": [{"id": "a", "enabled": true, "project": "octo/app", "event": "new_issue", "flow": "label"},
			{"id": "b", "enabled": true, "project": "nexus:skyrim/42", "event": "new_item", "kinds": ["bug"], "flow": "reply"}]}}}`)
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	a := s.Get().Agents
	if a.ModeFor("github:octo/app") != ModeWorktreePR || a.Projects["github:octo/lib"].Verify != "new" ||
		a.Projects["nexus:skyrim/42"].Prompt != "mod" || len(a.Projects) != 3 {
		t.Fatalf("projects: %+v", a.Projects)
	}
	if r := a.Automation.Rules; len(r) != 2 || r[0].Project != "github:octo/app" || r[1].Project != "nexus:skyrim/42" ||
		!r[1].MatchesKind("bug") || r[1].MatchesKind("issue") || !r[0].MatchesKind("") {
		t.Fatalf("rules: %+v", r)
	}
	raw := read(t, dir)
	ag, _ := raw["agents"].(map[string]any)
	projects, _ := ag["projects"].(map[string]any)
	app, _ := projects["github:octo/app"].(map[string]any)
	if raw["schemaVersion"] != float64(6) || raw["future"] != "keep" || app["future"] != float64(1) {
		t.Fatalf("file: %v", raw)
	}
	if _, ok := projects["octo/app"]; ok {
		t.Fatalf("legacy key kept: %v", projects)
	}
	// Unqualified keys are refused from now on.
	var ve *ValidationError
	if _, err := s.Patch(0, json.RawMessage(`{"agents": {"projects": {"octo/new": {"verify": "x"}}}}`), nil); !errors.As(err, &ve) ||
		ve.Field != "agents.projects.octo/new" || ve.Code != "projectKey" {
		t.Fatalf("unqualified key: %v", err)
	}
}

func TestAutomationOverrides(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Patch(0, json.RawMessage(`{"agents": {"automation": {"enabled": true, "maxPerDay": 20,
		"rules": [{"id": "labels", "enabled": true, "project": "github:octo/app", "event": "new_issue", "flow": "label"}]},
		"projects": {"github:octo/app": {"automation": {"enabled": false, "allowAutoFix": true, "maxAttempts": 5}},
			"github:octo/lib": {"automation": {"autoApplyLabels": true, "maxPerDay": 3}}}}}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	a := got.Agents
	for project, want := range map[string]AutomationPolicy{
		"github:octo/app":   {Enabled: false, TotalPerDay: 20, MaxAttempts: 5, AllowAutoFix: true},
		"github:octo/lib":   {Enabled: true, TotalPerDay: 20, ProjectPerDay: 3, MaxAttempts: 2, AutoApplyLabels: true},
		"github:octo/other": {Enabled: true, TotalPerDay: 20, MaxAttempts: 2},
	} {
		if p := a.AutomationFor(project); p != want {
			t.Errorf("%s: %+v, want %+v", project, p, want)
		}
	}
	if r := a.Automation.Rules; len(r) != 1 || r[0].LabelsAny == nil || r[0].ProfileID != "" {
		t.Fatalf("rules: %+v", r)
	}
	// null removes an override: the project inherits again.
	got, err = s.Patch(got.Revision, json.RawMessage(`{"agents": {"projects": {"github:octo/app": {"automation": {"enabled": null}}}}}`), nil)
	if err != nil || !got.Agents.AutomationFor("github:octo/app").Enabled || !got.Agents.AutomationFor("github:octo/app").AllowAutoFix {
		t.Fatalf("override removed: %v %+v", err, got.Agents.AutomationFor("github:octo/app"))
	}
}

func TestAutomationValidation(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	rule := func(fields string) string {
		return `{"agents": {"automation": {"rules": [{"id": "r", "project": "github:o/r", "event": "new_issue", "flow": "label"` + fields + `}]}}}`
	}
	cases := map[string][2]string{
		`{"agents": {"automation": {"maxPerDay": 0}}}`:    {"agents.automation.maxPerDay", "range"},
		`{"agents": {"automation": {"maxAttempts": 11}}}`: {"agents.automation.maxAttempts", "range"},
		rule(`, "id": "Bad Id"`):                          {"agents.automation.rules.0.id", "id"},
		rule(`, "project": "noslash"`):                    {"agents.automation.rules.0.project", "project"},
		rule(`, "event": "closed"`):                       {"agents.automation.rules.0.event", "enum"},
		rule(`, "flow": "verify"`):                        {"agents.automation.rules.0.flow", "enum"},
		rule(`, "kinds": ["pr"]`):                         {"agents.automation.rules.0.kinds", "enum"},
		rule(`, "profileId": "ghost"`):                    {"agents.automation.rules.0.profileId", "profile"},
		rule(`, "maxPerDay": -1`):                         {"agents.automation.rules.0.maxPerDay", "range"},
		rule(`, "labelsAny": [""]`):                       {"agents.automation.rules.0.labelsAny", "required"},
		`{"agents": {"automation": {"rules": [{"id": "r", "project": "github:o/r", "event": "new_issue", "flow": "fix"},
			{"id": "r", "project": "github:o/r", "event": "new_comment", "flow": "reply"}]}}}`: {"agents.automation.rules.1.id", "duplicate"},
		`{"agents": {"projects": {"github:o/r": {"automation": {"maxPerDay": 0}}}}}`:    {"agents.projects.github:o/r.automation.maxPerDay", "range"},
		`{"agents": {"projects": {"github:o/r": {"automation": {"maxAttempts": 99}}}}}`: {"agents.projects.github:o/r.automation.maxAttempts", "range"},
	}
	for patch, want := range cases {
		_, err := s.Patch(0, json.RawMessage(patch), nil)
		var ve *ValidationError
		if !errors.As(err, &ve) || ve.Field != want[0] || ve.Code != want[1] {
			t.Errorf("%s: %v, want %s/%s", patch, err, want[0], want[1])
		}
	}
	if _, err := s.Patch(0, json.RawMessage(rule(`, "profileId": "codex", "labelsAny": ["bug"], "maxPerDay": 5`)), nil); err != nil {
		t.Fatalf("valid rule: %v", err)
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
