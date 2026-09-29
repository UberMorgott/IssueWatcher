//go:build e2e

package main

import (
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/UberMorgott/issuewatcher/internal/runner"
	"github.com/UberMorgott/issuewatcher/internal/store"
)

// Project triage end to end: the real exe ranks the fake GitHub's open issues
// through fakecli (claude flavour), queues fix jobs for the top N, and the
// agent's per-run MCP server is `<exe> mcp --project <id>` reading only that
// project's issues.
//
//	go test -tags e2e -run TestTriageE2E -v -timeout 10m ./cmd/issuewatcher
func TestTriageE2E(t *testing.T) {
	c := startControlApp(t)
	c.useFakeCLI(t)
	addIssues(c.gh, fakeIssue(2, "Data loss on save", []string{"bug"}), fakeIssue(3, "Typo in README", nil), fakeIssue(4, "Add dark mode", []string{"enhancement"}))
	syncNow(t, c.app)
	waitFor(t, "4 issues", func() bool { return len(items(t, c.app)) == 4 })
	patchSettings(t, c.app, map[string]any{"agents": map[string]any{"roles": map[string]any{"responder": "claude"}, "triageTopN": 2}})
	var first struct {
		RepoID int64 `json:"repoId"`
	}
	if err := json.Unmarshal(items(t, c.app)[0], &first); err != nil || first.RepoID == 0 {
		t.Fatalf("item: %v", err)
	}
	pid := strconv.FormatInt(first.RepoID, 10)
	// Fix jobs need the project's local folder (a clone of it); the coder
	// profile's CLI is missing, so they stop at once and fakecli's record
	// stays the triage's.
	var rows []struct {
		ProjectID int64  `json:"projectId"`
		URL       string `json:"url"`
	}
	c.send(t, http.MethodGet, "/api/folders", nil, &rows)
	i := slices.IndexFunc(rows, func(r struct {
		ProjectID int64  `json:"projectId"`
		URL       string `json:"url"`
	}) bool {
		return r.ProjectID == first.RepoID
	})
	if i < 0 {
		t.Fatalf("folders %+v", rows)
	}
	clone := filepath.Join(c.dir, "clone")
	for _, args := range [][]string{{"init", "-q", clone}, {"-C", clone, "remote", "add", "origin", rows[i].URL + ".git"}} {
		if b, err := exec.CommandContext(t.Context(), "git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, b)
		}
	}
	var fr struct{ Status string }
	if code := c.send(t, http.MethodPut, "/api/projects/"+pid+"/path", map[string]any{"path": clone}, &fr); code != http.StatusOK || fr.Status != "ok" {
		t.Fatalf("map folder: %d %+v", code, fr)
	}
	patchSettings(t, c.app, map[string]any{"agents": map[string]any{"roles": map[string]any{"coder": "codex"},
		"profiles": []any{
			map[string]any{"id": "claude", "name": "claude", "cli": "claude", "path": c.fakecli, "model": "", "args": []string{}, "timeoutMinutes": 5, "maxParallel": 1, "maxBudgetUsd": 0},
			map[string]any{"id": "codex", "name": "codex", "cli": "codex", "path": filepath.Join(c.dir, "no-codex.exe"), "model": "", "args": []string{}, "timeoutMinutes": 5, "maxParallel": 1, "maxBudgetUsd": 0},
		}}})

	var tj store.Job
	if code := c.send(t, http.MethodPost, "/api/projects/"+pid+"/triage", nil, &tj); code != http.StatusCreated || tj.Flow != "triage" || tj.ItemID != 0 {
		t.Fatalf("triage: %d %+v", code, tj)
	}
	tj = waitJobDone(t, c.app, tj.ID)
	var res runner.Result
	if err := json.Unmarshal(tj.Result, &res); err != nil || res.Triage == nil {
		t.Fatalf("result %s: %v", tj.Result, err)
	}
	tr := res.Triage
	var queued []int64
	for _, p := range tr.Picks {
		if p.Queue == "queued" {
			queued = append(queued, p.JobID)
		}
	}
	if tr.Open != 4 || len(tr.Picks) != 4 || len(queued) != 2 || tr.Picks[2].Queue != "" {
		t.Fatalf("triage result %s", tj.Result)
	}
	var fixes store.JobChunk
	c.send(t, http.MethodGet, "/api/jobs?flow=fix&project="+pid, nil, &fixes)
	var ids []int64
	for _, j := range fixes.Items {
		ids = append(ids, j.ID)
	}
	slices.Sort(ids)
	slices.Sort(queued)
	if !slices.Equal(ids, queued) {
		t.Fatalf("fix jobs %v, triage queued %v", ids, queued)
	}

	// The agent's MCP server: `mcp --project <id>`, gone after the run.
	b, err := os.ReadFile(filepath.Join(c.dir, "fakecli-record.json"))
	if err != nil {
		t.Fatal(err)
	}
	var rec struct {
		Args      []string `json:"args"`
		Stdin     string   `json:"stdin"`
		MCPConfig string   `json:"mcpConfig"`
	}
	if err := json.Unmarshal(b, &rec); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rec.Stdin, "#2 Data loss on save") || !slices.Contains(rec.Args, "dontAsk") {
		t.Fatalf("triage args %q stdin %s", rec.Args, rec.Stdin)
	}
	var cfg struct {
		MCPServers map[string]struct {
			Args []string `json:"args"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(rec.MCPConfig), &cfg); err != nil {
		t.Fatalf("mcp config %q: %v", rec.MCPConfig, err)
	}
	srv := cfg.MCPServers["issuewatcher"]
	if !slices.Equal(srv.Args, []string{"mcp", "--project", pid}) {
		t.Fatalf("mcp args %q", srv.Args)
	}
	m := c.mcp(t, srv.Args[1:]...)
	var init map[string]any
	m.call("initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{},
		"clientInfo": map[string]any{"name": "e2e", "version": "0"}}, &init)
	m.send("notifications/initialized", map[string]any{}, true)
	var tools struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	m.call("tools/list", map[string]any{}, &tools)
	if len(tools.Tools) != 3 {
		t.Fatalf("project tools/list: %+v", tools)
	}
	r := m.tool("list_items", map[string]any{})
	var page struct {
		Items []struct {
			ID int64 `json:"id"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(r.Content[0].Text), &page); r.IsError || err != nil || len(page.Items) != 4 {
		t.Fatalf("list_items: %+v", r)
	}
	if r := m.tool("get_item", map[string]any{"id": page.Items[0].ID}); r.IsError {
		t.Fatalf("get_item: %+v", r)
	}
	if r := m.tool("get_item", map[string]any{"id": 99999}); !r.IsError {
		t.Fatalf("get_item of no item: %+v", r)
	}
	t.Logf("triage picks: %+v", tr.Picks)
}
