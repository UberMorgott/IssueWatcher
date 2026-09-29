//go:build e2e

// End-to-end automation test: the real executable, headless, against the
// GitHub fake (IW_GITHUB_API) with both agent profiles pointing at fakecli.
//
//	go test -tags e2e -run TestAutomationE2E -v -timeout 10m ./cmd/issuewatcher
//
// A new issue synced from the fake reaches the rules engine through the
// syncer's OnUpdate (main.go): rule label job → fakecli picks → labels added on
// the fake (autoApplyLabels) → job done; decision log rows for queued,
// auto_fix_off, total_cap and day_cap.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/instance"
	"github.com/UberMorgott/issuewatcher/internal/provider/github"
	"github.com/UberMorgott/issuewatcher/internal/provider/github/githubtest"
	"github.com/UberMorgott/issuewatcher/internal/secret"
	"github.com/UberMorgott/issuewatcher/internal/store"
)

const e2eRepo = "octo/demo"

func TestAutomationE2E(t *testing.T) {
	root := t.TempDir()
	exe := filepath.Join(root, "issuewatcher.exe")
	build(t, exe, "v0.0.0-e2e", "")
	fakecli := filepath.Join(root, "fakecli.exe")
	if b, err := exec.CommandContext(t.Context(), "go", "build", "-o", fakecli, "../../internal/runner/testdata/fakecli").CombinedOutput(); err != nil {
		t.Fatalf("build fakecli: %v\n%s", err, b)
	}

	gh := githubtest.New(t)
	gh.ExpiresIn = 0
	gh.Repos = []string{e2eRepo}
	gh.Labels = map[string][]string{e2eRepo: {"bug", "enhancement", "question"}}
	gh.Mu.Lock()
	gh.Issues = append(gh.Issues, fakeIssue(1, "Old issue", nil)) // baseline: never an event
	gh.Mu.Unlock()

	a := &app{dir: root, exe: exe, data: filepath.Join(root, "data")}
	secrets := filepath.Join(a.data, "secrets")
	if err := secret.WriteJSON(filepath.Join(secrets, "github-app.json"), github.App{ID: 42, Slug: "issuewatcher-test",
		ClientID: githubtest.ClientID, ClientSecret: githubtest.ClientSecret, PEM: "fake-pem", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := secret.WriteJSON(filepath.Join(secrets, "github-token.json"), github.Token{AccessToken: gh.Grant(), Login: githubtest.Login}); err != nil {
		t.Fatal(err)
	}

	e := &e2e{t: t}
	t.Cleanup(e.killAll)
	cmd := exec.Command(exe) //nolint:gosec,noctx // our test build
	cmd.Env = append(os.Environ(), "IW_HEADLESS=1", "IW_DATA_DIR="+a.data, "IW_PORT=", "IW_GITHUB_API="+gh.URL,
		"FAKECLI_MODE=ok", "FAKECLI_LABELS=bug,not-a-label")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	e.track(cmd.Process.Pid)
	go func() { _ = cmd.Wait() }()
	e.waitRuntime(a, func(rt instance.Runtime) bool { return rt.PID == cmd.Process.Pid })

	waitFor(t, "baseline sync", func() bool { return len(items(t, a)) == 1 })

	profile := func(id, cli string) map[string]any {
		return map[string]any{"id": id, "name": id, "cli": cli, "path": fakecli, "model": "", "args": []string{},
			"timeoutMinutes": 5, "maxParallel": 1, "maxBudgetUsd": 0}
	}
	patchSettings(t, a, map[string]any{"agents": map[string]any{
		"profiles": []any{profile("claude", "claude"), profile("codex", "codex")},
		"automation": map[string]any{"enabled": true, "maxPerDay": 2, "maxAttempts": 2, "allowAutoFix": false, "autoApplyLabels": true,
			"rules": []any{
				map[string]any{"id": "crash-fix", "enabled": true, "project": e2eRepo, "event": "new_issue", "labelsAny": []string{"crash"}, "flow": "fix", "profileId": "", "maxPerDay": 0},
				map[string]any{"id": "triage", "enabled": true, "project": e2eRepo, "event": "new_issue", "labelsAny": []string{}, "flow": "label", "profileId": "", "maxPerDay": 0},
			}},
	}})

	// #2 matches the fix rule (auto-fix off → skipped), #3 the label rule.
	addIssues(gh, fakeIssue(2, "Crash on start", []string{"crash"}), fakeIssue(3, "Button misaligned", nil))
	syncNow(t, a)
	waitFor(t, "log rows for #2 and #3", func() bool { return len(automationLog(t, a)) == 2 })
	log := byNumber(automationLog(t, a))
	expect(t, log[2], "crash-fix", store.DecisionSkipped, "auto_fix_off")
	expect(t, log[3], "triage", store.DecisionQueued, "")

	j3 := waitJobDone(t, a, *log[3].JobID)
	var res struct{ Labels, DroppedLabels, AppliedLabels []string }
	_ = json.Unmarshal(j3.Result, &res)
	if !slices.Equal(res.AppliedLabels, []string{"bug"}) || !slices.Equal(res.DroppedLabels, []string{"not-a-label"}) || j3.Origin != "rule" {
		t.Fatalf("job #3: origin %s result %s", j3.Origin, j3.Result)
	}
	if got := fakeLabels(gh, 3); !slices.Equal(got, []string{"bug"}) {
		t.Fatalf("fake GitHub #3 labels %v, want [bug]", got)
	}
	if !slices.Contains(gh.Calls(), githubtest.Call{Method: http.MethodPost, Path: "/repos/octo/demo/issues/3/labels"}) {
		t.Fatal("no label add call recorded")
	}

	// #4 takes the last slot of the total cap (2), #5 is over it.
	addIssues(gh, fakeIssue(4, "Typo in docs", nil))
	syncNow(t, a)
	waitFor(t, "log row for #4", func() bool { return len(automationLog(t, a)) == 3 })
	addIssues(gh, fakeIssue(5, "Slow search", nil))
	syncNow(t, a)
	waitFor(t, "log row for #5", func() bool { return len(automationLog(t, a)) == 4 })
	log = byNumber(automationLog(t, a))
	expect(t, log[4], "triage", store.DecisionQueued, "")
	expect(t, log[5], "triage", store.DecisionSkipped, "total_cap")
	waitJobDone(t, a, *log[4].JobID)

	// Global cap raised, project cap 2 (already used): day_cap.
	patchSettings(t, a, map[string]any{"agents": map[string]any{
		"automation": map[string]any{"maxPerDay": 50},
		"projects":   map[string]any{e2eRepo: map[string]any{"automation": map[string]any{"maxPerDay": 2}}},
	}})
	addIssues(gh, fakeIssue(6, "Dark mode", nil))
	syncNow(t, a)
	waitFor(t, "log row for #6", func() bool { return len(automationLog(t, a)) == 5 })
	expect(t, byNumber(automationLog(t, a))[6], "triage", store.DecisionSkipped, "day_cap")

	// The label adds bumped updated_at on the fake: later syncs saw #3/#4
	// change, and that must not be a new decision.
	syncNow(t, a)
	time.Sleep(2 * time.Second)
	if n := len(automationLog(t, a)); n != 5 {
		t.Fatalf("decision log has %d rows after a quiet sync, want 5", n)
	}
	t.Logf("fake GitHub: %d calls, %d label adds", len(gh.Calls()), gh.LabelAdds)
}

func fakeIssue(n int, title string, labels []string) *githubtest.Issue {
	now := time.Now().UTC().Truncate(time.Second)
	return &githubtest.Issue{ID: fmt.Sprintf("I_%d", n), Repo: e2eRepo, Number: n, Title: title, Author: "reporter", Open: true,
		Labels: labels, CreatedAt: now, UpdatedAt: now}
}

func addIssues(gh *githubtest.Server, is ...*githubtest.Issue) {
	gh.Mu.Lock()
	defer gh.Mu.Unlock()
	gh.Issues = append(gh.Issues, is...)
}

func fakeLabels(gh *githubtest.Server, n int) []string {
	gh.Mu.Lock()
	defer gh.Mu.Unlock()
	for _, is := range gh.Issues {
		if is.Number == n {
			return append([]string(nil), is.Labels...)
		}
	}
	return nil
}

func (a *app) send(t *testing.T, method, p string, body any, out any) int {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequestWithContext(t.Context(), method, a.url(p), r)
	req.Header.Set("Authorization", "Bearer "+a.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, p, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		t.Fatalf("%s %s: %d %s", method, p, resp.StatusCode, b)
	}
	if out != nil {
		if err := json.Unmarshal(b, out); err != nil {
			t.Fatalf("%s %s: %v: %s", method, p, err, b)
		}
	}
	return resp.StatusCode
}

func patchSettings(t *testing.T, a *app, patch any) {
	t.Helper()
	var doc struct{ Revision int }
	a.send(t, http.MethodGet, "/api/settings", nil, &doc)
	a.send(t, http.MethodPatch, "/api/settings", map[string]any{"revision": doc.Revision, "patch": patch}, nil)
}

func syncNow(t *testing.T, a *app) { t.Helper(); a.send(t, http.MethodPost, "/api/sync", nil, nil) }

func items(t *testing.T, a *app) []json.RawMessage {
	t.Helper()
	var c struct{ Items []json.RawMessage }
	a.send(t, http.MethodGet, "/api/items", nil, &c)
	return c.Items
}

func automationLog(t *testing.T, a *app) []store.AutomationEntry {
	t.Helper()
	var c store.AutomationChunk
	a.send(t, http.MethodGet, "/api/automation/log", nil, &c)
	return c.Items
}

func byNumber(es []store.AutomationEntry) map[int]store.AutomationEntry {
	m := map[int]store.AutomationEntry{}
	for _, e := range es {
		if _, dup := m[e.Number]; dup {
			m[-e.Number] = e // surfaced by expect as a duplicate
		}
		m[e.Number] = e
	}
	return m
}

func expect(t *testing.T, e store.AutomationEntry, rule, decision, reason string) {
	t.Helper()
	if e.RuleID != rule || e.Decision != decision || e.Reason != reason || (decision == store.DecisionQueued) != (e.JobID != nil) {
		t.Fatalf("decision for #%d: %+v, want rule %s %s %q", e.Number, e, rule, decision, reason)
	}
}

func waitJobDone(t *testing.T, a *app, id int64) store.Job {
	t.Helper()
	var j store.Job
	waitFor(t, fmt.Sprintf("job %d done", id), func() bool {
		a.send(t, http.MethodGet, fmt.Sprintf("/api/jobs/%d", id), nil, &j)
		if strings.Contains("failed cancelled needs_review", j.State) && j.State != "" {
			t.Fatalf("job %d ended %s: %s %s", id, j.State, j.Error, j.Result)
		}
		return j.State == store.JobDone
	})
	return j
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
