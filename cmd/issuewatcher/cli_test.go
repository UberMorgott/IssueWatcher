package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/UberMorgott/issuewatcher/internal/instance"
	"github.com/UberMorgott/issuewatcher/internal/paths"
)

// apiCall is one request the API fake saw.
type apiCall struct{ Method, Path, Query, Body string }

// fakeAPI serves canned answers per "METHOD /path" and records requests; the
// data dir (IW_DATA_DIR) points at it.
func fakeAPI(t *testing.T, answers map[string]string) *[]apiCall {
	t.Helper()
	var (
		mu    sync.Mutex
		calls []apiCall
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		calls = append(calls, apiCall{r.Method, r.URL.Path, r.URL.RawQuery, string(b)})
		mu.Unlock()
		a, ok := answers[r.Method+" "+r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"not found"}`))
			return
		}
		status, body, _ := strings.Cut(a, " ")
		code := map[string]int{"200": 200, "201": 201, "202": 202, "400": 400, "409": 409, "502": 502}[status]
		w.WriteHeader(code)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	t.Setenv(paths.EnvDataDir, dir)
	port, _ := strconv.Atoi(strings.TrimPrefix(srv.URL, "http://127.0.0.1:"))
	if err := instance.WriteRuntime(dir, instance.Runtime{PID: 1, Port: port, URL: srv.URL, Token: "tok"}); err != nil {
		t.Fatal(err)
	}
	return &calls
}

func cliRun(stdin string, args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	code := runCLI(args, strings.NewReader(stdin), &out, &errb)
	return code, out.String(), errb.String()
}

func TestCLIReadCommands(t *testing.T) {
	calls := fakeAPI(t, map[string]string{
		"GET /api/health":           `200 {"version":"v1","port":1}`,
		"GET /api/sync":             `200 {"running":false}`,
		"GET /api/items":            `200 {"items":[],"nextCursor":"","more":false}`,
		"GET /api/jobs/9/log":       `200 {"attempt":2,"steps":[]}`,
		"GET /api/jobs":             `200 {"items":[]}`,
		"GET /api/items/4":          `200 {"id":4}`,
		"GET /api/items/4/comments": `200 {"items":[]}`,
	})
	code, out, stderr := cliRun("", "status")
	var st struct {
		Running bool `json:"running"`
		Health  struct {
			Version string `json:"version"`
		} `json:"health"`
	}
	if code != exitOK || json.Unmarshal([]byte(out), &st) != nil || !st.Running || st.Health.Version != "v1" {
		t.Fatalf("status: %d %q %q", code, out, stderr)
	}
	for _, tc := range []struct {
		args []string
		want apiCall
	}{
		{[]string{"items", "--project", "3", "--unread", "--cursor", "abc", "--limit", "20"}, apiCall{"GET", "/api/items", "cursor=abc&limit=20&project=3&unread=1", ""}},
		{[]string{"job", "log", "9", "--attempt", "2"}, apiCall{"GET", "/api/jobs/9/log", "attempt=2", ""}},
		{[]string{"jobs", "--state", "needs_review", "--flow", "reply", "--item", "4"}, apiCall{"GET", "/api/jobs", "flow=reply&item=4&state=needs_review", ""}},
		{[]string{"item", "4", "--comments", "5"}, apiCall{"GET", "/api/items/4/comments", "limit=5", ""}},
	} {
		if code, out, stderr := cliRun("", tc.args...); code != exitOK || !json.Valid([]byte(out)) {
			t.Fatalf("%v: %d %q %q", tc.args, code, out, stderr)
		}
		if got := (*calls)[len(*calls)-1]; got != tc.want {
			t.Fatalf("%v: request %+v, want %+v", tc.args, got, tc.want)
		}
	}
}

func TestCLIExitCodes(t *testing.T) {
	fakeAPI(t, map[string]string{})
	if code, _, stderr := cliRun("", "job", "77"); code != exitAPI || !strings.Contains(stderr, "not found (HTTP 404)") {
		t.Fatalf("404: %d %q", code, stderr)
	}
	for _, args := range [][]string{{"bogus"}, {"item"}, {"item", "x"}, {"items", "--nope"}, {"job", "log"}} {
		if code, out, stderr := cliRun("", args...); code != exitUsage || out != "" || !strings.Contains(stderr, "usage:") {
			t.Fatalf("%v: %d %q %q", args, code, out, stderr)
		}
	}
	if code, out, _ := cliRun("", "help"); code != exitOK || !strings.Contains(out, "usage:") {
		t.Fatalf("help: %d %q", code, out)
	}
	t.Setenv(paths.EnvDataDir, t.TempDir()) // no runtime.json
	if code, out, stderr := cliRun("", "projects"); code != exitNotRunning || out != "" || !strings.Contains(stderr, "not running") {
		t.Fatalf("not running: %d %q %q", code, out, stderr)
	}
}

// TestCLIMCPScopeFlags: a scope flag given with a non-positive ID is a usage
// error, never the full (publishing) MCP server.
func TestCLIMCPScopeFlags(t *testing.T) {
	t.Setenv(paths.EnvDataDir, t.TempDir())
	for _, args := range [][]string{
		{"mcp", "--item", "0"}, {"mcp", "--project", "0"}, {"mcp", "--item", "-1"},
		{"mcp", "--item", "1", "--project", "2"}, {"mcp", "--item", "0", "--project", "2"},
	} {
		if code, _, stderr := cliRun("", args...); code != exitUsage || !strings.Contains(stderr, "usage: mcp") {
			t.Fatalf("%v: %d %q", args, code, stderr)
		}
	}
}

func TestCLIActionCommands(t *testing.T) {
	calls := fakeAPI(t, map[string]string{
		"POST /api/sync":             `202 `,
		"POST /api/items/4/comments": `201 {"id":10}`,
		"POST /api/jobs":             `201 {"jobs":[]}`,
		"POST /api/jobs/9/cancel":    `200 {"id":9}`,
		"POST /api/jobs/9/retry":     `200 {"id":9}`,
		"POST /api/jobs/9/dismiss":   `200 {"id":9}`,
		"POST /api/jobs/9/push":      `200 {"id":9}`,
		"POST /api/jobs/9/pr":        `200 {"id":9}`,
		"POST /api/jobs/9/reply":     `200 {"id":9}`,
		"POST /api/jobs/9/labels":    `200 {"id":9}`,
	})
	bodyFile := filepath.Join(t.TempDir(), "body.md")
	if err := os.WriteFile(bodyFile, []byte("from file"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		stdin string
		args  []string
		want  apiCall
		out   string
	}{
		{"", []string{"sync"}, apiCall{"POST", "/api/sync", "", ""}, `"ok": true`},
		{"hi\n", []string{"reply", "4", "-"}, apiCall{"POST", "/api/items/4/comments", "", `{"body":"hi\n"}`}, `"id": 10`},
		{"", []string{"reply", "--body-file", bodyFile, "4"}, apiCall{"POST", "/api/items/4/comments", "", `{"body":"from file"}`}, `"id": 10`},
		{"", []string{"jobs", "create", "--flow", "reply", "4", "5"}, apiCall{"POST", "/api/jobs", "", `{"flow":"reply","itemIds":[4,5],"profileId":""}`}, `"jobs"`},
		{"", []string{"job", "cancel", "9"}, apiCall{"POST", "/api/jobs/9/cancel", "", ""}, `"id": 9`},
		{"", []string{"job", "retry", "9"}, apiCall{"POST", "/api/jobs/9/retry", "", ""}, `"id": 9`},
		{"", []string{"job", "dismiss", "9"}, apiCall{"POST", "/api/jobs/9/dismiss", "", ""}, `"id": 9`},
		{"", []string{"job", "push", "9"}, apiCall{"POST", "/api/jobs/9/push", "", ""}, `"id": 9`},
		{"", []string{"job", "pr", "9"}, apiCall{"POST", "/api/jobs/9/pr", "", ""}, `"id": 9`},
		{"draft", []string{"job", "reply", "9", "-"}, apiCall{"POST", "/api/jobs/9/reply", "", `{"body":"draft"}`}, `"id": 9`},
		{"", []string{"job", "labels", "9", "bug", "ui"}, apiCall{"POST", "/api/jobs/9/labels", "", `{"labels":["bug","ui"]}`}, `"id": 9`},
	} {
		code, out, stderr := cliRun(tc.stdin, tc.args...)
		if code != exitOK || !strings.Contains(out, tc.out) {
			t.Fatalf("%v: %d %q %q", tc.args, code, out, stderr)
		}
		if got := (*calls)[len(*calls)-1]; got != tc.want {
			t.Fatalf("%v: request %+v, want %+v", tc.args, got, tc.want)
		}
	}
	for _, args := range [][]string{{"reply", "4"}, {"reply", "4", "-", "--body-file", bodyFile}, {"jobs", "create", "4"}, {"job", "labels", "9"}, {"job", "push"}} {
		if code, _, stderr := cliRun("", args...); code != exitUsage {
			t.Fatalf("%v: %d %q", args, code, stderr)
		}
	}
}

func TestCLIActionErrors(t *testing.T) {
	fakeAPI(t, map[string]string{
		"POST /api/jobs":             `400 {"error":"runner: bad flow"}`,
		"POST /api/jobs/9/push":      `409 {"error":"runner: not allowed in state done"}`,
		"POST /api/items/4/comments": `502 {"error":"posting the comment failed: boom"}`,
	})
	for args, want := range map[string]string{
		"jobs create --flow nope 4": "runner: bad flow (HTTP 400)",
		"job push 9":                "not allowed in state done (HTTP 409)",
		"reply 4 -":                 "posting the comment failed: boom (HTTP 502)",
	} {
		code, out, stderr := cliRun("x", strings.Fields(args)...)
		if code != exitAPI || out != "" || !strings.Contains(stderr, want) {
			t.Fatalf("%s: %d %q %q", args, code, out, stderr)
		}
	}
}

func TestIsCLI(t *testing.T) {
	for args, want := range map[string]bool{"": false, "--minimized": false, "--after-update=5": false, "/minimized": false, "status": true, "mcp": true} {
		if got := isCLI(strings.Fields(args)); got != want {
			t.Errorf("isCLI(%q) = %v", args, got)
		}
	}
}
