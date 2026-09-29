//go:build e2e

// End-to-end control test: the real executable's CLI subcommands against a
// headless instance of the same exe signed in to the GitHub fake.
//
//	go test -tags e2e -run TestControlE2E -v -timeout 10m ./cmd/issuewatcher
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/instance"
	"github.com/UberMorgott/issuewatcher/internal/provider/github"
	"github.com/UberMorgott/issuewatcher/internal/provider/github/githubtest"
	"github.com/UberMorgott/issuewatcher/internal/secret"
)

// controlApp is a headless instance plus the built exe for CLI calls.
type controlApp struct {
	*app
	gh      *githubtest.Server
	fakecli string
}

func startControlApp(t *testing.T) *controlApp {
	t.Helper()
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
	gh.Mu.Lock()
	gh.Issues = append(gh.Issues, fakeIssue(1, "Crash on start", []string{"bug"}))
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
	cmd.Env = append(os.Environ(), "IW_HEADLESS=1", "IW_DATA_DIR="+a.data, "IW_PORT=", "IW_GITHUB_API="+gh.URL, "FAKECLI_MODE=ok")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	e.track(cmd.Process.Pid)
	go func() { _ = cmd.Wait() }()
	e.waitRuntime(a, func(rt instance.Runtime) bool { return rt.PID == cmd.Process.Pid })
	waitFor(t, "first sync", func() bool { return len(items(t, a)) == 1 })
	return &controlApp{app: a, gh: gh, fakecli: fakecli}
}

// cli runs `exe args...` against the instance; stdout must be JSON when code is 0.
func (c *controlApp) cli(t *testing.T, stdin string, args ...string) (int, json.RawMessage, string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), c.exe, args...) //nolint:gosec // our test build
	cmd.Env = append(os.Environ(), "IW_DATA_DIR="+c.data)
	cmd.Stdin = strings.NewReader(stdin)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	code := 0
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("%v: %v", args, err)
	}
	if code == 0 && !json.Valid(out.Bytes()) {
		t.Fatalf("%v: stdout is not JSON: %q", args, out.String())
	}
	return code, out.Bytes(), errb.String()
}

func (c *controlApp) ok(t *testing.T, stdin string, out any, args ...string) {
	t.Helper()
	code, b, stderr := c.cli(t, stdin, args...)
	if code != 0 {
		t.Fatalf("%v: exit %d: %s", args, code, stderr)
	}
	if out != nil {
		if err := json.Unmarshal(b, out); err != nil {
			t.Fatalf("%v: %v: %s", args, err, b)
		}
	}
}

// mcpProc is `exe mcp` spoken to in raw newline-delimited JSON-RPC.
type mcpProc struct {
	t      *testing.T
	in     io.WriteCloser
	out    *bufio.Scanner
	nextID int
}

func (c *controlApp) mcp(t *testing.T) *mcpProc {
	t.Helper()
	cmd := exec.Command(c.exe, "mcp") //nolint:gosec,noctx // our test build; ended by stdin EOF in Cleanup (t.Context is cancelled first)
	cmd.Env = append(os.Environ(), "IW_DATA_DIR="+c.data)
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = in.Close() // EOF ends the server
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("mcp exit: %v; stderr %q", err, stderr.String())
			}
		case <-time.After(10 * time.Second):
			_ = cmd.Process.Kill()
			t.Error("mcp did not exit on stdin EOF")
		}
		if stderr.Len() > 0 {
			t.Errorf("mcp wrote to stderr: %q", stderr.String())
		}
	})
	sc := bufio.NewScanner(out)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	return &mcpProc{t: t, in: in, out: sc}
}

func (m *mcpProc) send(method string, params any, notify bool) {
	m.t.Helper()
	msg := map[string]any{"jsonrpc": "2.0", "method": method, "params": params}
	if !notify {
		m.nextID++
		msg["id"] = m.nextID
	}
	b, _ := json.Marshal(msg)
	if _, err := m.in.Write(append(b, '\n')); err != nil {
		m.t.Fatal(err)
	}
}

// call sends a request and returns its result; every stdout line must be a JSON-RPC message.
func (m *mcpProc) call(method string, params any, result any) {
	m.t.Helper()
	m.send(method, params, false)
	for m.out.Scan() {
		var msg struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      *int            `json:"id"`
			Result  json.RawMessage `json:"result"`
			Error   json.RawMessage `json:"error"`
		}
		if err := json.Unmarshal(m.out.Bytes(), &msg); err != nil || msg.JSONRPC != "2.0" {
			m.t.Fatalf("stdout line is not JSON-RPC: %q", m.out.Text())
		}
		if msg.ID == nil || *msg.ID != m.nextID {
			continue // a notification
		}
		if msg.Error != nil {
			m.t.Fatalf("%s: %s", method, msg.Error)
		}
		if err := json.Unmarshal(msg.Result, result); err != nil {
			m.t.Fatalf("%s: %v: %s", method, err, msg.Result)
		}
		return
	}
	m.t.Fatalf("%s: stdout closed: %v", method, m.out.Err())
}

type toolResult struct {
	IsError bool `json:"isError"`
	Content []struct {
		Text string `json:"text"`
	} `json:"content"`
}

func (m *mcpProc) tool(name string, args map[string]any) toolResult {
	m.t.Helper()
	var r toolResult
	m.call("tools/call", map[string]any{"name": name, "arguments": args}, &r)
	if len(r.Content) != 1 {
		m.t.Fatalf("%s: %+v", name, r)
	}
	return r
}

func TestMCPE2E(t *testing.T) {
	c := startControlApp(t)
	c.useFakeCLI(t)
	m := c.mcp(t)
	var init struct {
		ProtocolVersion string `json:"protocolVersion"`
		ServerInfo      struct {
			Name string `json:"name"`
		} `json:"serverInfo"`
	}
	m.call("initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{},
		"clientInfo": map[string]any{"name": "e2e", "version": "0"}}, &init)
	if init.ServerInfo.Name != "issuewatcher" || init.ProtocolVersion != "2025-06-18" {
		t.Fatalf("initialize: %+v", init)
	}
	m.send("notifications/initialized", map[string]any{}, true)
	var tools struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	m.call("tools/list", map[string]any{}, &tools)
	if len(tools.Tools) != 16 {
		t.Fatalf("tools/list: %d tools", len(tools.Tools))
	}
	r := m.tool("list_items", map[string]any{"limit": 5})
	var page struct {
		Items []struct {
			ID int64 `json:"id"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(r.Content[0].Text), &page); r.IsError || err != nil || len(page.Items) != 1 {
		t.Fatalf("list_items: %+v", r)
	}
	itemID := page.Items[0].ID
	r = m.tool("start_jobs", map[string]any{"flow": "reply", "item_ids": []int64{itemID}})
	var queued struct {
		Jobs []struct {
			Job struct {
				ID int64 `json:"id"`
			} `json:"job"`
		} `json:"jobs"`
	}
	if err := json.Unmarshal([]byte(r.Content[0].Text), &queued); r.IsError || err != nil || len(queued.Jobs) != 1 {
		t.Fatalf("start_jobs: %+v", r)
	}
	jobID := queued.Jobs[0].Job.ID
	waitFor(t, "reply job needs_review", func() bool {
		r := m.tool("get_job", map[string]any{"id": jobID})
		return strings.Contains(r.Content[0].Text, `"state":"needs_review"`)
	})
	r = m.tool("send_job_reply", map[string]any{"id": jobID, "body": "Sent over MCP."})
	if r.IsError || !strings.Contains(r.Content[0].Text, `"state":"done"`) || c.comments(1) != 1 {
		t.Fatalf("send_job_reply: %+v, fake comments %d", r, c.comments(1))
	}
	if r := m.tool("send_job_reply", map[string]any{"id": jobID, "body": "again"}); !r.IsError || !strings.Contains(r.Content[0].Text, "HTTP 409") {
		t.Fatalf("second send_job_reply: %+v", r)
	}
}

// comments counts the fake issue's comments.
func (c *controlApp) comments(number int) int {
	c.gh.Mu.Lock()
	defer c.gh.Mu.Unlock()
	for _, is := range c.gh.Issues {
		if is.Number == number {
			return len(is.Comments)
		}
	}
	return -1
}

// useFakeCLI points both agent profiles at fakecli.
func (c *controlApp) useFakeCLI(t *testing.T) {
	t.Helper()
	profile := func(id, cli string) map[string]any {
		return map[string]any{"id": id, "name": id, "cli": cli, "path": c.fakecli, "model": "", "args": []string{},
			"timeoutMinutes": 5, "maxParallel": 1, "maxBudgetUsd": 0}
	}
	patchSettings(t, c.app, map[string]any{"agents": map[string]any{
		"profiles": []any{profile("claude", "claude"), profile("codex", "codex")},
	}})
}

func TestControlE2E(t *testing.T) {
	c := startControlApp(t)

	var st struct {
		Running bool
		Health  struct{ Version string }
	}
	c.ok(t, "", &st, "status")
	if !st.Running || st.Health.Version != "v0.0.0-e2e" {
		t.Fatalf("status %+v", st)
	}
	// The windowed build keeps a console copy next to it for interactive shells.
	gui, err := os.ReadFile(c.exe)
	if err != nil {
		t.Fatal(err)
	}
	want, ok, err := consoleCopy(gui)
	if err != nil || !ok {
		t.Fatalf("e2e build is not windowed: ok %v err %v", ok, err)
	}
	cliExe := filepath.Join(c.dir, cliExeName)
	waitFor(t, "console copy", func() bool { b, _ := os.ReadFile(cliExe); return bytes.Equal(b, want) })
	viaCopy := &controlApp{app: &app{dir: c.dir, exe: cliExe, data: c.data}}
	st.Running = false
	viaCopy.ok(t, "", &st, "status")
	if !st.Running || st.Health.Version != "v0.0.0-e2e" {
		t.Fatalf("status via %s: %+v", cliExeName, st)
	}
	if code, _, stderr := viaCopy.cli(t, ""); code != exitUsage || !strings.Contains(stderr, "usage:") {
		t.Fatalf("%s without a command: exit %d, stderr %q", cliExeName, code, stderr)
	}
	var projects []struct {
		ID   int64
		Name string
	}
	c.ok(t, "", &projects, "projects")
	if len(projects) != 1 || projects[0].Name != e2eRepo {
		t.Fatalf("projects %+v", projects)
	}
	var list struct {
		Items []struct {
			ID     int64
			Number int
		}
	}
	c.ok(t, "", &list, "items", "--project", strconv.FormatInt(projects[0].ID, 10), "--label", "bug", "--state", "open")
	if len(list.Items) != 1 || list.Items[0].Number != 1 {
		t.Fatalf("items %+v", list)
	}
	itemID := strconv.FormatInt(list.Items[0].ID, 10)
	var item struct {
		Item     struct{ Title string }
		Comments struct{ Items []json.RawMessage }
	}
	c.ok(t, "", &item, "item", itemID)
	if item.Item.Title != "Crash on start" {
		t.Fatalf("item %+v", item)
	}
	c.ok(t, "", nil, "jobs")
	if code, _, stderr := c.cli(t, "", "job", "999"); code != exitAPI || !strings.Contains(stderr, "not found") {
		t.Fatalf("job 999: %d %s", code, stderr)
	}
	if code, _, _ := c.cli(t, "", "items", "--bogus"); code != exitUsage {
		t.Fatalf("usage: exit %d", code)
	}

	// Actions: direct reply, then a reply job's draft sent with `job reply`.
	c.ok(t, "Looking into it.", nil, "reply", itemID, "-")
	if n := c.comments(1); n != 1 {
		t.Fatalf("fake comments after reply: %d", n)
	}
	c.useFakeCLI(t)
	var queued struct {
		Jobs []struct {
			Job   struct{ ID int64 }
			Error string
		}
	}
	c.ok(t, "", &queued, "jobs", "create", "--flow", "reply", itemID)
	if len(queued.Jobs) != 1 || queued.Jobs[0].Job.ID == 0 {
		t.Fatalf("jobs create: %+v", queued)
	}
	jobID := strconv.FormatInt(queued.Jobs[0].Job.ID, 10)
	var j struct{ State string }
	waitFor(t, "reply job needs_review", func() bool {
		c.ok(t, "", &j, "job", jobID)
		return j.State == "needs_review"
	})
	c.ok(t, "", nil, "job", "log", jobID)
	c.ok(t, "Edited draft.", &j, "job", "reply", jobID, "-")
	if j.State != "done" || c.comments(1) != 2 {
		t.Fatalf("job reply: state %s, fake comments %d", j.State, c.comments(1))
	}
	if code, _, stderr := c.cli(t, "again", "job", "reply", jobID, "-"); code != exitAPI || !strings.Contains(stderr, "HTTP 409") {
		t.Fatalf("second job reply: %d %s", code, stderr)
	}
	c.ok(t, "", nil, "sync")

	cmd := exec.CommandContext(t.Context(), c.exe, "status") //nolint:gosec // our test build
	cmd.Env = append(os.Environ(), "IW_DATA_DIR="+t.TempDir())
	if err := cmd.Run(); err == nil || cmd.ProcessState.ExitCode() != exitNotRunning {
		t.Fatalf("empty data dir: %v", err)
	}
}
