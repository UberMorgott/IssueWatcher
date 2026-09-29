package runner

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/UberMorgott/issuewatcher/internal/config"
	"github.com/UberMorgott/issuewatcher/internal/store"
)

type fakeRecord struct {
	Args      []string `json:"args"`
	Stdin     string   `json:"stdin"`
	MCPConfig string   `json:"mcpConfig"`
}

func readRecord(t *testing.T, path string) fakeRecord {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // G304: test record file
	if err != nil {
		t.Fatal(err)
	}
	var r fakeRecord
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatal(err)
	}
	return r
}

// The job's scoped MCP server is passed per run (claude --mcp-config temp file,
// removed after; codex -c), never with --strict-mcp-config.
func TestJobMCPClaude(t *testing.T) {
	mode(t, "ok")
	e := setup(t, 1, func(s *config.Settings) { s.Agents.Roles.Responder = "claude" })
	e.r.opts.Exe = `C:\Apps\IssueWatcher\issuewatcher.exe`
	j := e.wait(e.enqueue("reply", e.items[0])[0].ID, store.JobNeedsReview)
	rec := readRecord(t, e.record)
	i := slices.Index(rec.Args, "--mcp-config")
	if i < 0 || slices.Contains(rec.Args, "--strict-mcp-config") {
		t.Fatalf("claude args %v", rec.Args)
	}
	if k := slices.Index(rec.Args, "--allowedTools"); k < 0 || rec.Args[k+1] != "mcp__issuewatcher" {
		t.Fatalf("claude args %v", rec.Args)
	}
	var cfg struct {
		MCPServers map[string]struct {
			Type    string            `json:"type"`
			Command string            `json:"command"`
			Args    []string          `json:"args"`
			Env     map[string]string `json:"env"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(rec.MCPConfig), &cfg); err != nil {
		t.Fatalf("mcp config %q: %v", rec.MCPConfig, err)
	}
	srv := cfg.MCPServers["issuewatcher"]
	if len(cfg.MCPServers) != 1 || srv.Type != "stdio" || srv.Command != e.r.opts.Exe ||
		!slices.Equal(srv.Args, []string{"mcp", "--item", strconv.FormatInt(e.items[0], 10)}) || srv.Env["IW_DATA_DIR"] != e.data {
		t.Fatalf("mcp config %s", rec.MCPConfig)
	}
	if exists(rec.Args[i+1]) {
		t.Fatalf("mcp config %s left after the run", rec.Args[i+1])
	}
	sys, err := os.ReadFile(filepath.Join(jobDir(e.data, j.ID), "reply.system.md"))
	if err != nil || !strings.Contains(string(sys), "list_item_comments") {
		t.Fatalf("system prompt: %s %v", sys, err)
	}
}

func TestJobMCPCodexAndSettings(t *testing.T) {
	mode(t, "ok")
	e := setup(t, 2, nil)
	e.r.opts.Exe = `C:\Apps\IssueWatcher\issuewatcher.exe`
	e.wait(e.enqueue("reply", e.items[0])[0].ID, store.JobNeedsReview)
	rec := readRecord(t, e.record)
	want := `mcp_servers.issuewatcher={command="C:\\Apps\\IssueWatcher\\issuewatcher.exe",args=["mcp","--item","` +
		strconv.FormatInt(e.items[0], 10) + `"],env={IW_DATA_DIR=` + strconv.Quote(e.data) + `},default_tools_approval_mode="approve"}`
	if k := slices.Index(rec.Args, want); k < 1 || rec.Args[k-1] != "-c" || !strings.Contains(rec.Stdin, "list_item_comments") {
		t.Fatalf("codex args %q, want -c %s", rec.Args, want)
	}

	// Project override off: no server, no note.
	e.mu.Lock()
	e.cfg.Agents.Projects["octo/demo"] = config.ProjectAgent{Mode: config.ModeDirect, JobMCP: new(false)}
	e.mu.Unlock()
	e.wait(e.enqueue("reply", e.items[1])[0].ID, store.JobNeedsReview)
	rec = readRecord(t, e.record)
	if slices.Contains(rec.Args, "-c") || strings.Contains(rec.Stdin, "list_item_comments") {
		t.Fatalf("codex args with jobMcp off: %q", rec.Args)
	}
}

func TestJobMCPFor(t *testing.T) {
	cfg := config.Defaults()
	r := New(Options{DataDir: "D", Exe: "iw.exe", Settings: func() config.Settings { return cfg }})
	s := agentSpec{item: 3, repo: "o/r"}
	if m := r.jobMCPFor(s); m == nil || m.item != 3 || m.exe != "iw.exe" || m.dataDir != "D" {
		t.Fatalf("default on: %+v", m)
	}
	cfg.Agents.JobMCP = false
	if r.jobMCPFor(s) != nil {
		t.Fatal("global off")
	}
	cfg.Agents.Projects["o/r"] = config.ProjectAgent{JobMCP: new(true)}
	if r.jobMCPFor(s) == nil {
		t.Fatal("project on beats global off")
	}
	r.opts.Exe = ""
	if r.jobMCPFor(s) != nil {
		t.Fatal("no exe")
	}
}
