package runner

import (
	"strings"
	"testing"

	"github.com/UberMorgott/issuewatcher/internal/config"
	"github.com/UberMorgott/issuewatcher/internal/store"
)

// The autopilot variant of the direct fix prompt asks for a neutral "Refs #N"
// (the release closes the issue after the reply); the ordinary one keeps
// "Fixes #N"; a mod-page report keeps its report URL in both.
func TestAutopilotFixPrompt(t *testing.T) {
	cfg := config.Defaults().Agents
	in := store.JobInput{Number: 7, Title: "Crash", ProjectName: "o/r", ProjectKey: "github:o/r", CodeKey: "github:o/r"}
	_, plain := prompts(cfg, flowFixDirect, promptInput{in: in})
	_, auto := prompts(cfg, flowFixDirect, promptInput{in: in, autopilot: true})
	if !strings.Contains(plain, `"Fixes #7"`) || strings.Contains(plain, "Refs #7") {
		t.Fatalf("plain prompt:\n%s", plain)
	}
	if strings.Contains(auto, `"Fixes #7"`) || !strings.Contains(auto, `"Refs #7"`) || !strings.Contains(auto, "Never use a closing keyword") {
		t.Fatalf("autopilot prompt:\n%s", auto)
	}
	mod := in
	mod.Mod, mod.URL, mod.Platform, mod.CodeProject = true, "https://www.nexusmods.com/x/mods/1?tab=bugs", "nexus", "o/r"
	if _, task := prompts(cfg, flowFixDirect, promptInput{in: mod, autopilot: true}); strings.Contains(task, "Refs #7") || !strings.Contains(task, "Reported on "+mod.URL) {
		t.Fatalf("mod autopilot prompt:\n%s", task)
	}
}

func TestRefsAndClosingRefs(t *testing.T) {
	refs, closes := refsRe(12), closesRe(12)
	for msg, want := range map[string][2]bool{
		"fix: crash\n\nRefs #12":      {true, false},
		"ref: o/r#12":                 {true, false},
		"fix: crash\n\nFixes #12":     {false, true},
		"Refs #123":                   {false, false},
		"closes #12 and refs #12 too": {true, true},
	} {
		if refs.MatchString(msg) != want[0] || closes.MatchString(msg) != want[1] {
			t.Errorf("%q: refs %v closes %v, want %v", msg, refs.MatchString(msg), closes.MatchString(msg), want)
		}
	}
}

// Fix rules leave a project to autopilot when its autopilot fixes (enabled + autoFix).
func TestAutopilotFixesAndRule(t *testing.T) {
	cfg := config.Defaults().Agents
	key := "github:o/r"
	if AutopilotFixes(cfg, key) {
		t.Fatal("default config: autopilot fixes")
	}
	ap := config.DefaultProjectAutopilot()
	ap.Enabled, ap.AutoFix = true, true
	cfg.Projects = map[string]config.ProjectAgent{key: {Autopilot: ap}}
	if !AutopilotFixes(cfg, key) {
		t.Fatal("enabled + autoFix: not autopilot")
	}
	rules := []config.Rule{
		{ID: "label", Enabled: true, Project: key, Event: "new_issue", Flow: config.FlowLabel},
		{ID: "crash", Enabled: true, Project: key, Event: "new_issue", Flow: config.FlowFix, LabelsAny: []string{"crash"}},
	}
	ev := store.Event{Kind: store.EventNewIssue, ItemKind: store.KindIssue, Project: key}
	if _, ok := fixRule(rules, ev, []string{"bug"}); ok {
		t.Fatal("fix rule matched without its label")
	}
	if ru, ok := fixRule(rules, ev, []string{"Crash"}); !ok || ru.ID != "crash" {
		t.Fatalf("fix rule %+v %v", ru, ok)
	}
}
