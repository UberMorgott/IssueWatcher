//go:build e2e

// End-to-end control test: the real executable's CLI subcommands against a
// headless instance of the same exe signed in to the GitHub fake.
//
//	go test -tags e2e -run TestControlE2E -v -timeout 10m ./cmd/issuewatcher
package main

import (
	"bytes"
	"encoding/json"
	"errors"
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
