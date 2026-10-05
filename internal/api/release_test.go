package api

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/config"
	"github.com/UberMorgott/issuewatcher/internal/provider"
	"github.com/UberMorgott/issuewatcher/internal/release"
	"github.com/UberMorgott/issuewatcher/internal/store"
)

func releaseEnv(t *testing.T, extra ...func(*Options)) (*env, int64) {
	t.Helper()
	cs, err := config.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e := syncedEnv(t, append([]func(*Options){func(o *Options) {
		o.Settings = cfgStore{cs}
		o.Release = release.New(release.Deps{Store: o.Store, Settings: cs.Get, DataDir: filepath.Join(t.TempDir(), "data")})
	}}, extra...)...)
	var repos []store.Repo
	if code := e.call(t, http.MethodGet, "/api/projects", "", &repos); code != http.StatusOK || len(repos) != 1 {
		t.Fatalf("repos %d %+v", code, repos)
	}
	return e, repos[0].ID
}

func TestPublishProfileAndReleaseRoutes(t *testing.T) {
	e, id := releaseEnv(t)
	base := "/api/projects/" + itoa(id)

	var doc ProfileDoc
	if code := e.call(t, http.MethodGet, base+"/publish-profile", "", &doc); code != http.StatusOK || doc.Project != "github:octo/app" ||
		len(doc.Kinds.Version) != 4 || doc.Autopilot.Enabled || doc.Resolved.OK {
		t.Fatalf("get %d %+v", code, doc)
	}
	ap := doc.Autopilot
	ap.Enabled = true
	put := `{"publishProfile": {"build": {"command": "pwsh -File build.ps1", "output": "dist/{name}_{version}.zip"},
		"version": {"kind": "factorio-info"}, "changelog": {"kind": "factorio"}}, "autopilot": ` + mustJSON(t, ap) + `}`
	if code := e.call(t, http.MethodPut, base+"/publish-profile", put, &doc); code != http.StatusOK ||
		doc.PublishProfile.Build.Output != "dist/{name}_{version}.zip" || !doc.Autopilot.Enabled || doc.Autopilot.CoalesceMinutes != 60 {
		t.Fatalf("put %d %+v", code, doc)
	}
	// Replacing drops what the new profile leaves out.
	put = `{"publishProfile": {"build": {"path": "E:/ready.zip"}, "version": {"kind": "git-tag"}, "changelog": {"kind": "commits"}}}`
	doc = ProfileDoc{}
	if code := e.call(t, http.MethodPut, base+"/publish-profile", put, &doc); code != http.StatusOK ||
		doc.PublishProfile.Build.Command != "" || doc.PublishProfile.Build.Path != "E:/ready.zip" || !doc.Autopilot.Enabled {
		t.Fatalf("replace %d %+v", code, doc.PublishProfile)
	}
	var bad apiErr
	if code := e.call(t, http.MethodPut, base+"/publish-profile", `{"publishProfile": {"build": {"command": "x"}}}`, &bad); code != http.StatusBadRequest {
		t.Fatalf("invalid profile: %d %+v", code, bad)
	}

	var plan release.Plan
	if code := e.call(t, http.MethodPost, base+"/release/plan", `{"version": "2.0.0"}`, &plan); code != http.StatusOK || plan.OK ||
		len(plan.Refusals) == 0 || plan.Refusals[0].Code != release.CodeNoFolder {
		t.Fatalf("plan %d %+v", code, plan)
	}
	if code, got := e.callErr(t, http.MethodPost, base+"/release", `{}`); code != http.StatusConflict || got != release.CodeNoFolder {
		t.Fatalf("release %d %q", code, got)
	}
	var chk release.CheckResult
	if code := e.call(t, http.MethodPost, base+"/publish-profile/check", `{}`, &chk); code != http.StatusOK || chk.Build.Skipped == "" ||
		chk.Refusals[0].Code != release.CodeNoFolder {
		t.Fatalf("check %d %+v", code, chk)
	}
	var runs []store.Run
	if code := e.call(t, http.MethodGet, "/api/runs?project="+itoa(id), "", &runs); code != http.StatusOK || len(runs) != 0 {
		t.Fatalf("runs %d %+v", code, runs)
	}
	if code := e.call(t, http.MethodGet, "/api/runs/99", "", nil); code != http.StatusNotFound {
		t.Fatalf("run 99: %d", code)
	}
	var paused map[string]any
	if code := e.call(t, http.MethodPost, "/api/autopilot/pause", `{"paused": true}`, &paused); code != http.StatusOK || paused["paused"] != true {
		t.Fatalf("pause %d %+v", code, paused)
	}
	if code := e.call(t, http.MethodPost, "/api/autopilot/pause", `{"project": `+itoa(id)+`, "paused": true}`, &paused); code != http.StatusOK ||
		paused["enabled"] != false {
		t.Fatalf("pause project %d %+v", code, paused)
	}
	if code := e.call(t, http.MethodPost, "/api/autopilot/pause", `{"paused": false}`, &paused); code != http.StatusOK || paused["paused"] != false {
		t.Fatalf("unpause %d %+v", code, paused)
	}
	var evs struct {
		Events []store.AutopilotEvent `json:"events"`
		Unread int                    `json:"unread"`
	}
	if code := e.call(t, http.MethodGet, "/api/autopilot/events?unreadOnly=true", "", &evs); code != http.StatusOK || len(evs.Events) != 0 {
		t.Fatalf("events %d %+v", code, evs)
	}
	if code := e.call(t, http.MethodPost, "/api/autopilot/events/read", `{"all": true}`, nil); code != http.StatusOK {
		t.Fatalf("read %d", code)
	}
}

// Calls from a process inside a runner job object are refused (the hook
// stands for runner.InAgentJob; release_windows_test.go runs a real child).
func TestAgentCallerRefusedRoutes(t *testing.T) {
	pub := &fakePublisher{}
	e, id := releaseEnv(t, func(o *Options) {
		o.Publishers = func(string) provider.Publisher { return pub }
	})
	e.s.inAgentJob = func(uint32) bool { return true }
	base := "/api/projects/" + itoa(id)
	for _, c := range []struct{ method, path, body string }{
		{http.MethodPut, base + "/publish-profile", `{"publishProfile": {}}`},
		{http.MethodPost, base + "/release", `{}`},
		{http.MethodPost, "/api/runs/1/skip", `{"step": "publish", "target": "nexus:a/1"}`},
		{http.MethodPost, "/api/autopilot/pause", `{"paused": false}`},
		{http.MethodPost, base + "/publish", `{"version": "1.0.0", "fileId": "f", "path": "C:/a.zip"}`},
	} {
		if code, out := e.callErr(t, c.method, c.path, c.body); code != http.StatusForbidden || out != codeAgentCaller {
			t.Errorf("%s %s: %d %q", c.method, c.path, code, out)
		}
	}
	// Allowed for agents: dry runs, plans, reads, setting the pause.
	for _, c := range []struct{ method, path, body string }{
		{http.MethodPost, base + "/release", `{"dryRun": true}`},
		{http.MethodPost, base + "/release/plan", `{}`},
		{http.MethodGet, base + "/publish-profile", ``},
		{http.MethodPost, "/api/autopilot/pause", `{"paused": true}`},
		{http.MethodPost, base + "/publish", `{"version": "1.0.0", "dryRun": true}`},
	} {
		if code := e.call(t, c.method, c.path, c.body, nil); code == http.StatusForbidden {
			t.Errorf("%s %s refused for an agent", c.method, c.path)
		}
	}
}

// The autopilot block has its own route; dry runs validate without writing.
func TestAutopilotSettingsAndDryRuns(t *testing.T) {
	e, id := releaseEnv(t)
	base := "/api/projects/" + itoa(id)
	var doc AutopilotDoc
	if code := e.call(t, http.MethodGet, base+"/autopilot", "", &doc); code != http.StatusOK || doc.Autopilot.Enabled ||
		doc.Autopilot.CoalesceMinutes != 60 || doc.Global.MaxReleasesPerDay != 5 {
		t.Fatalf("get %d %+v", code, doc)
	}
	rev := doc.Revision
	ap := doc.Autopilot
	ap.Enabled, ap.PublishWithoutSmoke = true, true
	body := `{"autopilot": ` + mustJSON(t, ap) + `, "dryRun": true}`
	if code := e.call(t, http.MethodPut, base+"/autopilot", body, &doc); code != http.StatusOK || !doc.DryRun || !doc.Autopilot.Enabled ||
		!doc.Autopilot.PublishWithoutSmoke || doc.Revision != rev {
		t.Fatalf("dry run %d %+v", code, doc)
	}
	if code := e.call(t, http.MethodGet, base+"/autopilot", "", &doc); code != http.StatusOK || doc.Autopilot.Enabled || doc.Revision != rev {
		t.Fatalf("the dry run saved: %d %+v", code, doc)
	}
	var bad apiErr
	if code := e.call(t, http.MethodPut, base+"/autopilot", `{"autopilot": {"coalesceMinutes": 0}, "dryRun": true}`, &bad); code != http.StatusBadRequest {
		t.Fatalf("invalid dry run: %d %+v", code, bad)
	}
	doc = AutopilotDoc{}
	if code := e.call(t, http.MethodPut, base+"/autopilot", `{"autopilot": `+mustJSON(t, ap)+`}`, &doc); code != http.StatusOK ||
		doc.DryRun || !doc.Autopilot.Enabled || doc.Revision != rev+1 {
		t.Fatalf("put %d %+v", code, doc)
	}

	// Publish profile dry run: validated, returned, not written.
	var pv ProfilePreview
	put := `{"dryRun": true, "publishProfile": {"build": {"command": "build.cmd", "output": "dist/x.zip"},
		"smoke": {"kind": "factorio", "install": "D:/Games/Factorio", "ticks": 300}}}`
	if code := e.call(t, http.MethodPut, base+"/publish-profile", put, &pv); code != http.StatusOK || !pv.DryRun || !pv.OK ||
		pv.PublishProfile.Smoke.Install != "D:/Games/Factorio" || !pv.Autopilot.Enabled {
		t.Fatalf("profile dry run %d %+v", code, pv)
	}
	var pd ProfileDoc
	if code := e.call(t, http.MethodGet, base+"/publish-profile", "", &pd); code != http.StatusOK || pd.PublishProfile.Smoke.Kind != "" {
		t.Fatalf("the profile dry run saved: %d %+v", code, pd.PublishProfile)
	}
	if code := e.call(t, http.MethodPut, base+"/publish-profile", `{"dryRun": true, "publishProfile": {"smoke": {"kind": "command"}}}`, &bad); code != http.StatusBadRequest {
		t.Fatalf("invalid profile dry run: %d %+v", code, bad)
	}

	// The project's verify command: dry run (not written), then saved alone
	// (publishProfile / autopilot untouched).
	pv = ProfilePreview{}
	if code := e.call(t, http.MethodPut, base+"/publish-profile", `{"dryRun": true, "verify": " luac -p {name}.lua "}`, &pv); code != http.StatusOK ||
		!pv.DryRun || pv.Verify != "luac -p {name}.lua" {
		t.Fatalf("verify dry run %d %+v", code, pv)
	}
	pd = ProfileDoc{}
	if code := e.call(t, http.MethodGet, base+"/publish-profile", "", &pd); code != http.StatusOK || pd.Verify != "" {
		t.Fatalf("the verify dry run saved: %d %q", code, pd.Verify)
	}
	pd = ProfileDoc{}
	if code := e.call(t, http.MethodPut, base+"/publish-profile", `{"verify": "go test ./..."}`, &pd); code != http.StatusOK ||
		pd.Verify != "go test ./..." || !pd.Autopilot.Enabled {
		t.Fatalf("verify put %d %+v", code, pd)
	}

	// Agent callers: refused writes, allowed dry runs and reads.
	e.s.inAgentJob = func(uint32) bool { return true }
	if code, out := e.callErr(t, http.MethodPut, base+"/autopilot", `{"autopilot": {}}`); code != http.StatusForbidden || out != codeAgentCaller {
		t.Fatalf("agent put: %d %q", code, out)
	}
	if code, out := e.callErr(t, http.MethodPut, base+"/publish-profile", `{"verify": "x"}`); code != http.StatusForbidden || out != codeAgentCaller {
		t.Fatalf("agent verify put: %d %q", code, out)
	}
	for _, c := range []struct{ method, path, body string }{
		{http.MethodGet, base + "/autopilot", ``},
		{http.MethodPut, base + "/autopilot", `{"autopilot": ` + mustJSON(t, ap) + `, "dryRun": true}`},
		{http.MethodPut, base + "/publish-profile", `{"publishProfile": {}, "dryRun": true}`},
	} {
		if code := e.call(t, c.method, c.path, c.body, nil); code != http.StatusOK {
			t.Errorf("agent %s %s: %d", c.method, c.path, code)
		}
	}
}

// A partial autopilot block changes only its keys: the rest keep their stored
// values (no "must be 1–1440" for a left-out coalesceMinutes); publish is
// replaced whole, so a target left out of it is switched off.
func TestAutopilotPartialUpdate(t *testing.T) {
	e, id := releaseEnv(t)
	base := "/api/projects/" + itoa(id)
	var doc AutopilotDoc
	if code := e.call(t, http.MethodPut, base+"/autopilot", `{"autopilot": {"autoRelease": true}, "dryRun": true}`, &doc); code != http.StatusOK ||
		!doc.DryRun || !doc.Autopilot.AutoRelease || doc.Autopilot.CoalesceMinutes != 60 || !doc.Autopilot.AutoTriage {
		t.Fatalf("partial dry run %d %+v", code, doc)
	}
	if code := e.call(t, http.MethodPut, base+"/autopilot", `{"autopilot": {"coalesceMinutes": 30, "publish": {"steam:1": true, "nexus:g/2": true}}}`, &doc); code != http.StatusOK ||
		doc.Autopilot.CoalesceMinutes != 30 || len(doc.Autopilot.Publish) != 2 || doc.Autopilot.MaxBatchAgeHours != 24 {
		t.Fatalf("partial put %d %+v", code, doc)
	}
	if code := e.call(t, http.MethodPut, base+"/autopilot", `{"autopilot": {"enabled": true}}`, &doc); code != http.StatusOK ||
		!doc.Autopilot.Enabled || doc.Autopilot.CoalesceMinutes != 30 || len(doc.Autopilot.Publish) != 2 {
		t.Fatalf("publish kept %d %+v", code, doc)
	}
	doc = AutopilotDoc{} // decoding into a filled map would merge it
	if code := e.call(t, http.MethodPut, base+"/autopilot", `{"autopilot": {"publish": {"steam:1": true}}}`, &doc); code != http.StatusOK ||
		len(doc.Autopilot.Publish) != 1 || !doc.Autopilot.Publish["steam:1"] || !doc.Autopilot.Enabled {
		t.Fatalf("publish replaced %d %+v", code, doc)
	}
	// The publish profile route merges its autopilot block the same way.
	var pd ProfileDoc
	if code := e.call(t, http.MethodPut, base+"/publish-profile", `{"autopilot": {"autoReply": true}}`, &pd); code != http.StatusOK ||
		!pd.Autopilot.AutoReply || pd.Autopilot.CoalesceMinutes != 30 || !pd.Autopilot.Enabled {
		t.Fatalf("profile partial autopilot %d %+v", code, pd.Autopilot)
	}
	// A given out-of-range value is still refused; a non-object too.
	for _, body := range []string{`{"autopilot": {"coalesceMinutes": 0}}`, `{"autopilot": [1]}`, `{"autopilot": {"coalesceMinutes": "x"}}`} {
		if code, out := e.callErr(t, http.MethodPut, base+"/autopilot", body); code != http.StatusBadRequest {
			t.Errorf("%s: %d %q", body, code, out)
		}
	}
}

// New events and mark-read send autopilot.unread with the counts (top-bar badge).
func TestAutopilotUnreadEvents(t *testing.T) {
	e, id := releaseEnv(t)
	events := make(chan string, 8)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, e.s.BaseURL()+"/api/events", nil)
	resp, err := e.browser.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	go func() {
		sc := bufio.NewScanner(resp.Body)
		name := ""
		for sc.Scan() {
			if n, ok := strings.CutPrefix(sc.Text(), "event: "); ok {
				name = n
			} else if d, ok := strings.CutPrefix(sc.Text(), "data: "); ok && name != "" {
				events <- name + " " + d
			}
		}
	}()
	for e.s.Clients() == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	next := func(prefix string) string {
		t.Helper()
		for {
			select {
			case ev := <-events:
				if strings.HasPrefix(ev, prefix) {
					return ev
				}
			case <-time.After(2 * time.Second):
				t.Fatalf("no %s event", prefix)
			}
		}
	}
	add := func(sev string) store.AutopilotEvent {
		ev, err := e.s.opts.Store.AddEvent(t.Context(), store.AutopilotEvent{ProjectID: id, Kind: "release.held", Severity: sev, Title: "held"})
		if err != nil {
			t.Fatal(err)
		}
		e.s.AutopilotEvent(ev)
		return ev
	}
	a := add(store.SeverityAttention)
	next(EventAutopilotEvent)
	if got := next(EventAutopilotUnread); !strings.Contains(got, `"unread":1`) || !strings.Contains(got, `"attention":1`) {
		t.Fatalf("unread %s", got)
	}
	add(store.SeverityInfo)
	if got := next(EventAutopilotUnread); !strings.Contains(got, `"unread":2`) || !strings.Contains(got, `"attention":1`) {
		t.Fatalf("unread %s", got)
	}
	var read struct{ Marked, Unread, Attention int }
	if code := e.call(t, http.MethodPost, "/api/autopilot/events/read", `{"ids": [`+itoa(a.ID)+`]}`, &read); code != http.StatusOK ||
		read.Marked != 1 || read.Unread != 1 || read.Attention != 0 {
		t.Fatalf("read %d %+v", code, read)
	}
	if got := next(EventAutopilotUnread); !strings.Contains(got, `"unread":1`) || !strings.Contains(got, `"attention":0`) {
		t.Fatalf("unread after read %s", got)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// callErr sends a request with the browser session and returns status + error code.
func (e *env) callErr(t *testing.T, method, path, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, e.s.BaseURL()+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := e.browser.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out apiErr
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out.Code
}

// The browser session (owner) plans without the disabled refusal; the bearer
// token (MCP / CLI) gets it.
func TestReleaseOriginByAuth(t *testing.T) {
	e, id := releaseEnv(t)
	path := "/api/projects/" + itoa(id) + "/release/plan"
	var p release.Plan
	if code := e.call(t, http.MethodPost, path, `{}`, &p); code != http.StatusOK || hasCode(p, release.CodeDisabled) {
		t.Fatalf("owner: %d %+v", code, p.Refusals)
	}
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, e.s.BaseURL()+path, strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+e.s.Token())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	p = release.Plan{}
	if err := json.NewDecoder(resp.Body).Decode(&p); err != nil || !hasCode(p, release.CodeDisabled) {
		t.Fatalf("mcp: %v %+v", err, p.Refusals)
	}
}

func hasCode(p release.Plan, code string) bool {
	for _, r := range p.Refusals {
		if r.Code == code {
			return true
		}
	}
	return false
}
