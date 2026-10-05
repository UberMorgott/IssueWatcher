package control

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// releaseAPI fakes the release endpoints and records "METHOD path?query body".
func releaseAPI(t *testing.T) (*Client, *[]string) {
	t.Helper()
	var got []string
	polls := 0
	c, _ := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if !authed(w, r) {
			return
		}
		b, _ := io.ReadAll(r.Body)
		got = append(got, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery+" "+string(b))
		switch r.Method + " " + r.URL.Path {
		case "GET /api/projects":
			_, _ = w.Write([]byte(`[{"id":1,"name":"o/r","platform":"github","key":"github:o/r"},{"id":2,"name":"o/r","platform":"nexus","key":"nexus:g/2"},` +
				`{"id":3,"name":"x","platform":"github","key":"github:a/x"},{"id":4,"name":"x","platform":"github","key":"github:b/x"}]`))
		case "GET /api/projects/1/publish-profile":
			_, _ = w.Write([]byte(`{"projectId":1,"revision":7}`))
		case "PUT /api/projects/1/publish-profile":
			_, _ = w.Write([]byte(`{"projectId":1,"revision":8}`))
		case "GET /api/projects/1/autopilot":
			_, _ = w.Write([]byte(`{"projectId":1,"revision":7,"autopilot":{"enabled":false}}`))
		case "PUT /api/projects/1/autopilot":
			_, _ = w.Write([]byte(`{"projectId":1,"revision":8,"autopilot":{"enabled":true}}`))
		case "PUT /api/projects/9/publish-profile", "PUT /api/projects/9/autopilot", "POST /api/runs/5/skip":
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":"refused: agent runs cannot release","code":"agent_caller"}`))
		case "POST /api/projects/1/release/plan":
			_, _ = w.Write([]byte(`{"ok":false,"refusals":[{"code":"dirty_folder","message":"uncommitted changes"}]}`))
		case "POST /api/projects/1/release":
			if strings.Contains(string(b), `"dryRun":true`) {
				w.WriteHeader(http.StatusConflict)
				_, _ = w.Write([]byte(`{"error":"release refused","code":"no_profile","refusals":[{"code":"no_profile","message":"no publish profile"},` +
					`{"code":"disabled","message":"autopilot off"}],"plan":{"ok":false}}`))
				return
			}
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"run":{"id":5,"state":"pending"},"plan":{"ok":true}}`))
		case "GET /api/runs":
			_, _ = w.Write([]byte(`[]`))
		case "GET /api/runs/5":
			polls++
			if polls < 2 {
				_, _ = w.Write([]byte(`{"run":{"id":5,"state":"running"},"steps":[]}`))
				return
			}
			_, _ = w.Write([]byte(`{"run":{"id":5,"state":"held","heldReason":"gate_failed"},"steps":[]}`))
		case "POST /api/runs/5/resume", "POST /api/runs/5/cancel":
			_, _ = w.Write([]byte(`{"id":5}`))
		case "POST /api/autopilot/pause":
			_, _ = w.Write([]byte(`{"paused":true}`))
		case "GET /api/autopilot/events":
			_, _ = w.Write([]byte(`{"events":[],"unread":0,"attention":0}`))
		default:
			http.NotFound(w, r)
		}
	})
	return c, &got
}

func TestReleaseClient(t *testing.T) {
	runPoll = time.Millisecond
	c, got := releaseAPI(t)
	ctx := t.Context()
	for in, want := range map[string]int64{"1": 1, "github:o/r": 1, "GITHUB:O/R": 1, "o/r": 1, "nexus:g/2": 2, "github:b/x": 4} {
		if id, err := c.ResolveProject(ctx, in); err != nil || id != want {
			t.Errorf("ResolveProject(%q) = %d, %v; want %d", in, id, err, want)
		}
	}
	for _, in := range []string{"x", "nope", "0", ""} {
		if _, err := c.ResolveProject(ctx, in); err == nil {
			t.Errorf("ResolveProject(%q) succeeded", in)
		}
	}
	if _, err := c.SetPublishProfile(ctx, 1, []byte(`[1]`)); err == nil {
		t.Fatal("non-object profile accepted")
	}
	_, err := c.Release(ctx, 1, ReleaseRequest{DryRun: true})
	var ae *APIError
	if !errors.As(err, &ae) || ae.Code != "no_profile" || len(ae.Refusals) != 2 || ae.Refusals[1].Code != "disabled" {
		t.Fatalf("refused release: %#v", err)
	}
	if s := err.Error(); s != "release refused (HTTP 409, code no_profile); refusals: no_profile: no publish profile; disabled: autopilot off" {
		t.Fatalf("error text %q", s)
	}
	view, err := c.RunWait(ctx, 5)
	if err != nil || !strings.Contains(string(view), `"held"`) {
		t.Fatalf("RunWait: %s %v", view, err)
	}
	if _, err := c.Runs(ctx, RunQuery{Project: 1, State: "held", Kind: "release", Limit: 3}); err != nil {
		t.Fatal(err)
	}
	if last := (*got)[len(*got)-1]; last != "GET /api/runs?kind=release&limit=3&project=1&state=held " {
		t.Fatalf("runs request %q", last)
	}
}

func TestMCPReleaseTools(t *testing.T) {
	c, got := releaseAPI(t)
	cs := mcpSession(t, NewMCPServer(c, "test", slog.New(slog.DiscardHandler)))
	tools, err := cs.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tl := range tools.Tools {
		if tl.Name == "pause_autopilot" && strings.Contains(string(mustJSON(t, tl.InputSchema)), "paused") {
			t.Fatalf("pause_autopilot can clear the pause: %s", mustJSON(t, tl.InputSchema))
		}
		if tl.Name == "set_publish_profile" && strings.Contains(string(mustJSON(t, tl.InputSchema)), "autopilot") {
			t.Fatalf("set_publish_profile takes autopilot switches: %s", mustJSON(t, tl.InputSchema))
		}
	}
	call := func(name string, args map[string]any) (string, bool) {
		t.Helper()
		r, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return text(t, r), r.IsError
	}
	for _, tc := range []struct {
		name    string
		args    map[string]any
		req     string
		out     string
		isError bool
	}{
		{"get_publish_profile", map[string]any{"project": 1}, "GET /api/projects/1/publish-profile? ", `"revision":7`, false},
		{"set_publish_profile", map[string]any{"project": 1, "revision": 7, "publish_profile": map[string]any{"build": map[string]any{"command": "make"}}},
			`PUT /api/projects/1/publish-profile? {"publishProfile":{"build":{"command":"make"}},"revision":7}`, `"revision":8`, false},
		{"set_publish_profile", map[string]any{"project": 9, "publish_profile": map[string]any{}},
			`PUT /api/projects/9/publish-profile? {"publishProfile":{}}`, "code agent_caller", true},
		{"set_publish_profile", map[string]any{"project": 1, "dry_run": true, "publish_profile": map[string]any{"smoke": map[string]any{"kind": "factorio"}}},
			`PUT /api/projects/1/publish-profile? {"dryRun":true,"publishProfile":{"smoke":{"kind":"factorio"}}}`, `"revision":8`, false},
		{"get_autopilot_settings", map[string]any{"project": 1}, "GET /api/projects/1/autopilot? ", `"enabled":false`, false},
		{"set_autopilot_settings", map[string]any{"project": 1, "revision": 7, "autopilot": map[string]any{"enabled": true, "publishWithoutSmoke": true}},
			`PUT /api/projects/1/autopilot? {"autopilot":{"enabled":true,"publishWithoutSmoke":true},"revision":7}`, `"enabled":true`, false},
		{"set_autopilot_settings", map[string]any{"project": 1, "dry_run": true, "autopilot": map[string]any{"enabled": true}},
			`PUT /api/projects/1/autopilot? {"autopilot":{"enabled":true},"dryRun":true}`, `"revision":8`, false},
		{"set_autopilot_settings", map[string]any{"project": 9, "autopilot": map[string]any{"enabled": true}},
			`PUT /api/projects/9/autopilot? {"autopilot":{"enabled":true}}`, "code agent_caller", true},
		{"plan_release", map[string]any{"project": 1, "version": "1.2.0", "items": []int64{4}, "targets": []string{"nexus:g/2"}},
			`POST /api/projects/1/release/plan? {"version":"1.2.0","items":[4],"targets":["nexus:g/2"]}`, "dirty_folder", false},
		{"release", map[string]any{"project": 1, "dry_run": true},
			`POST /api/projects/1/release? {"dryRun":true}`, "code no_profile); refusals: no_profile: no publish profile; disabled: autopilot off", true},
		{"release", map[string]any{"project": 1, "head": "abc"}, `POST /api/projects/1/release? {"head":"abc"}`, `"run":{"id":5`, false},
		{"list_runs", map[string]any{"project": 1, "limit": 500}, "GET /api/runs?limit=50&project=1 ", "[]", false},
		{"get_run", map[string]any{"id": 5}, "GET /api/runs/5? ", `"run"`, false},
		{"resume_run", map[string]any{"id": 5}, "POST /api/runs/5/resume? ", `"id":5`, false},
		{"cancel_run", map[string]any{"id": 5}, "POST /api/runs/5/cancel? ", `"id":5`, false},
		{"skip_step", map[string]any{"run": 5, "step": "publish:nexus:g/2"}, `POST /api/runs/5/skip? {"step":"publish:nexus:g/2","target":""}`, "agent_caller", true},
		{"pause_autopilot", map[string]any{}, `POST /api/autopilot/pause? {"paused":true}`, `"paused":true`, false},
		{"pause_autopilot", map[string]any{"project": 1}, `POST /api/autopilot/pause? {"paused":true,"project":1}`, `"paused":true`, false},
		{"list_autopilot_events", map[string]any{"unread_only": true}, "GET /api/autopilot/events?limit=20&unreadOnly=true ", `"unread":0`, false},
	} {
		out, isErr := call(tc.name, tc.args)
		if isErr != tc.isError || !strings.Contains(out, tc.out) {
			t.Fatalf("%s %v: isError %v, %s", tc.name, tc.args, isErr, out)
		}
		if last := (*got)[len(*got)-1]; last != tc.req {
			t.Fatalf("%s: request %q, want %q", tc.name, last, tc.req)
		}
	}
	// Pausing cannot be undone through MCP: an extra paused:false is not part of the tool.
	_, _ = call("pause_autopilot", map[string]any{"paused": false})
	if slices.ContainsFunc(*got, func(s string) bool { return strings.Contains(s, `"paused":false`) }) {
		t.Fatalf("pause cleared through MCP: %v", *got)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
