package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCLIReleaseCommands(t *testing.T) {
	calls := fakeAPI(t, map[string]string{
		"GET /api/projects":                   `200 [{"id":1,"name":"o/r","platform":"github","key":"github:o/r"}]`,
		"GET /api/projects/1/publish-profile": `200 {"revision":7}`,
		"PUT /api/projects/1/publish-profile": `200 {"revision":8}`,
		"GET /api/projects/1/autopilot":       `200 {"revision":7,"autopilot":{"enabled":false}}`,
		"PUT /api/projects/1/autopilot":       `200 {"revision":9}`,
		"POST /api/projects/1/release/plan":   `200 {"ok":true}`,
		"POST /api/projects/1/release":        `202 {"run":{"id":5,"state":"done"},"plan":{"ok":true}}`,
		"GET /api/runs":                       `200 []`,
		"GET /api/runs/5":                     `200 {"run":{"id":5,"state":"done"}}`,
		"POST /api/runs/5/resume":             `202 {"id":5}`,
		"POST /api/runs/5/cancel":             `200 {"run":{"id":5}}`,
		"POST /api/runs/5/skip":               `200 {"id":5}`,
	})
	profile := filepath.Join(t.TempDir(), "p.json")
	if err := os.WriteFile(profile, []byte("\xEF\xBB\xBF{\"publishProfile\":{}}"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		stdin string
		args  []string
		want  apiCall
		out   string
	}{
		{"", []string{"profile", "get", "github:o/r"}, apiCall{"GET", "/api/projects/1/publish-profile", "", ""}, `"revision": 7`},
		{"", []string{"profile", "set", "1", "--file", profile}, apiCall{"PUT", "/api/projects/1/publish-profile", "", `{"publishProfile":{}}`}, `"revision": 8`},
		{`{"revision":7,"autopilot":{"enabled":true}}`, []string{"profile", "set", "o/r", "-"},
			apiCall{"PUT", "/api/projects/1/publish-profile", "", `{"revision":7,"autopilot":{"enabled":true}}`}, `"revision": 8`},
		{"", []string{"profile", "set", "1", "--dry-run", "--file", profile},
			apiCall{"PUT", "/api/projects/1/publish-profile", "", `{"dryRun":true,"publishProfile":{}}`}, `"revision": 8`},
		{"", []string{"autopilot", "get", "o/r"}, apiCall{"GET", "/api/projects/1/autopilot", "", ""}, `"enabled": false`},
		{`{"revision":7,"autopilot":{"autoFix":true}}`, []string{"autopilot", "set", "1", "-"},
			apiCall{"PUT", "/api/projects/1/autopilot", "", `{"revision":7,"autopilot":{"autoFix":true}}`}, `"revision": 9`},
		{`{"autopilot":{"autoPush":true}}`, []string{"autopilot", "set", "github:o/r", "--dry-run", "-"},
			apiCall{"PUT", "/api/projects/1/autopilot", "", `{"autopilot":{"autoPush":true},"dryRun":true}`}, `"revision": 9`},
		{"", []string{"release", "plan", "o/r", "--version", "1.2.0", "--items", "4, 6", "--targets", "nexus:g/2"},
			apiCall{"POST", "/api/projects/1/release/plan", "", `{"version":"1.2.0","items":[4,6],"targets":["nexus:g/2"]}`}, `"ok": true`},
		{"", []string{"release", "run", "1", "--dry-run", "--head", "abc"}, apiCall{"POST", "/api/projects/1/release", "", `{"head":"abc","dryRun":true}`}, `"run"`},
		{"", []string{"release", "run", "1", "--wait"}, apiCall{"GET", "/api/runs/5", "", ""}, `"state": "done"`},
		{"", []string{"runs", "--project", "github:o/r", "--state", "held"}, apiCall{"GET", "/api/runs", "project=1&state=held", ""}, `[]`},
		{"", []string{"run", "5"}, apiCall{"GET", "/api/runs/5", "", ""}, `"done"`},
		{"", []string{"run", "5", "resume"}, apiCall{"POST", "/api/runs/5/resume", "", ""}, `"id": 5`},
		{"", []string{"run", "5", "cancel"}, apiCall{"POST", "/api/runs/5/cancel", "", ""}, `"run"`},
		{"", []string{"run", "5", "skip", "publish", "--target", "nexus:g/2"}, apiCall{"POST", "/api/runs/5/skip", "", `{"step":"publish","target":"nexus:g/2"}`}, `"id": 5`},
	} {
		code, out, stderr := cliRun(tc.stdin, tc.args...)
		if code != exitOK || !strings.Contains(out, tc.out) {
			t.Fatalf("%v: %d %q %q", tc.args, code, out, stderr)
		}
		if got := (*calls)[len(*calls)-1]; got != tc.want {
			t.Fatalf("%v: request %+v, want %+v", tc.args, got, tc.want)
		}
	}
	for _, args := range [][]string{
		{"release"}, {"release", "ship", "1"}, {"release", "plan"}, {"release", "plan", "1", "2"}, {"release", "plan", "1", "--items", "x"},
		{"release", "plan", "1", "--wait"}, {"run"}, {"run", "x"}, {"run", "5", "bogus"}, {"run", "5", "skip"}, {"run", "5", "--target", "t"},
		{"profile"}, {"profile", "get"}, {"profile", "set", "1"}, {"runs", "extra"},
		{"autopilot"}, {"autopilot", "bogus"}, {"autopilot", "get"}, {"autopilot", "get", "1", "2"}, {"autopilot", "set", "1"},
		{"autopilot", "set", "1", "--file", profile, "-"},
	} {
		if code, _, stderr := cliRun("", args...); code != exitUsage {
			t.Fatalf("%v: %d %q", args, code, stderr)
		}
	}
	if code, _, stderr := cliRun("", "release", "plan", "nope"); code != exitAPI || !strings.Contains(stderr, `no project "nope"`) {
		t.Fatalf("unknown project: %d %q", code, stderr)
	}
}

func TestCLIReleaseRefusals(t *testing.T) {
	fakeAPI(t, map[string]string{
		"POST /api/projects/1/release": `409 {"error":"release refused","code":"dirty_folder","refusals":[{"code":"dirty_folder","message":"uncommitted"}],"plan":{}}`,
		"POST /api/runs/5/skip":        `403 {"error":"refused","code":"agent_caller"}`,
	})
	for args, want := range map[string]string{
		"release run 1":            "release refused (HTTP 409, code dirty_folder); refusals: dirty_folder: uncommitted",
		"run 5 skip publish:n:g/2": "refused (HTTP 403, code agent_caller)",
	} {
		code, out, stderr := cliRun("", strings.Fields(args)...)
		if code != exitAPI || out != "" || !strings.Contains(stderr, want) {
			t.Fatalf("%s: %d %q %q", args, code, out, stderr)
		}
	}
}
