package api

import (
	"context"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/config"
	"github.com/UberMorgott/issuewatcher/internal/runner"
	"github.com/UberMorgott/issuewatcher/internal/store"
)

func TestJobsAPI(t *testing.T) {
	var r *runner.Runner
	var st *store.Store
	e := syncedEnv(t, func(o *Options) {
		st = o.Store
		r = runner.New(runner.Options{Store: o.Store, DataDir: filepath.Join(t.TempDir(), "data"), Settings: config.Defaults,
			LookPath: func(string) (string, error) { return "", context.Canceled }})
		o.Runner = r
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); r.Wait() })
	if err := r.Start(ctx); err != nil {
		t.Fatal(err)
	}
	var items store.IssueChunk
	if code := e.call(t, http.MethodGet, "/api/items?state=open", "", &items); code != http.StatusOK || len(items.Items) != 1 {
		t.Fatalf("items %d %+v", code, items)
	}
	id := strconv.FormatInt(items.Items[0].ID, 10)

	for body, want := range map[string]int{
		`{"itemIds":[` + id + `],"flow":"label"}`:                    http.StatusConflict, // no label adapter configured
		`{"itemIds":[` + id + `],"flow":"verify"}`:                   http.StatusBadRequest,
		`{"itemIds":[],"flow":"fix"}`:                                http.StatusBadRequest,
		`{"itemIds":[` + id + `],"flow":"fix","profileId":"nobody"}`: http.StatusBadRequest,
		`nope`: http.StatusBadRequest,
	} {
		if code := e.call(t, http.MethodPost, "/api/jobs", body, nil); code != want {
			t.Errorf("POST %s: %d, want %d", body, code, want)
		}
	}
	// The project has no local folder: a fix is refused up front, no job is created.
	var refused struct {
		Error string          `json:"error"`
		Code  string          `json:"code"`
		Jobs  []runner.Queued `json:"jobs"`
	}
	if code := e.callAny(t, http.MethodPost, "/api/jobs", `{"itemIds":[`+id+`],"flow":"fix"}`, &refused); code != http.StatusConflict ||
		refused.Code != runner.CodeNoFolder || len(refused.Jobs) != 1 || refused.Jobs[0].Job != nil || refused.Jobs[0].Error != runner.CodeNoFolder ||
		!strings.Contains(refused.Error, "Projects and folders") {
		t.Fatalf("fix without a folder: %d %+v", code, refused)
	}
	var none store.JobChunk
	if e.call(t, http.MethodGet, "/api/jobs?flow=fix", "", &none); len(none.Items) != 0 {
		t.Fatalf("a refused fix created a job: %+v", none.Items)
	}

	// A fix queued before its folder went away (here: straight into the store)
	// still fails at run time with the code the UI links.
	fj, err := st.CreateJob(t.Context(), items.Items[0].ID, "fix", "claude", store.OriginManual, "")
	if err != nil {
		t.Fatal(err)
	}
	r.Refresh()
	jid := strconv.FormatInt(fj.ID, 10)
	var j store.Job
	for deadline := time.Now().Add(10 * time.Second); j.State != store.JobFailed; {
		if time.Now().After(deadline) {
			t.Fatalf("job never failed: %+v", j)
		}
		time.Sleep(20 * time.Millisecond)
		e.call(t, http.MethodGet, "/api/jobs/"+jid, "", &j)
	}
	if string(j.Result) != `{"errorCode":"no_folder"}` || j.Repo != "octo/app" || j.Number != 1 {
		t.Fatalf("job %+v %s", j, j.Result)
	}
	// Project triage: queued for the project (no item); no CLI on this machine → failed no_cli.
	pid := strconv.FormatInt(items.Items[0].RepoID, 10)
	for path, want := range map[string]int{"/api/projects/9999/triage": http.StatusNotFound, "/api/projects/x/triage": http.StatusBadRequest} {
		if code := e.call(t, http.MethodPost, path, "", nil); code != want {
			t.Errorf("POST %s: %d, want %d", path, code, want)
		}
	}
	if code := e.call(t, http.MethodPost, "/api/projects/"+pid+"/triage", `{"profileId":"nobody"}`, nil); code != http.StatusBadRequest {
		t.Errorf("triage with an unknown profile: %d", code)
	}
	var tj store.Job
	if code := e.call(t, http.MethodPost, "/api/projects/"+pid+"/triage", "", &tj); code != http.StatusCreated || tj.Flow != "triage" || tj.ItemID != 0 ||
		tj.Repo != "octo/app" || tj.ProfileID != "codex" {
		t.Fatalf("triage %d %+v", code, tj)
	}
	for deadline := time.Now().Add(10 * time.Second); tj.State != store.JobFailed; {
		if time.Now().After(deadline) {
			t.Fatalf("triage never failed: %+v", tj)
		}
		time.Sleep(20 * time.Millisecond)
		e.call(t, http.MethodGet, "/api/jobs/"+strconv.FormatInt(tj.ID, 10), "", &tj)
	}
	if string(tj.Result) == "" || !strings.Contains(string(tj.Result), `"errorCode":"no_cli"`) {
		t.Fatalf("triage result %s", tj.Result)
	}

	var chunk store.JobChunk
	if code := e.call(t, http.MethodGet, "/api/jobs?state=failed&flow=fix", "", &chunk); code != http.StatusOK || len(chunk.Items) != 1 || *chunk.Total != 1 {
		t.Fatalf("list %d %+v", code, chunk)
	}
	if code := e.call(t, http.MethodGet, "/api/jobs?state=active", "", &chunk); code != http.StatusOK || len(chunk.Items) != 0 {
		t.Fatalf("active %d %+v", code, chunk)
	}
	if code := e.call(t, http.MethodGet, "/api/jobs?origin=manual&flow=fix", "", &chunk); code != http.StatusOK || len(chunk.Items) != 1 ||
		chunk.Items[0].Origin != store.OriginManual || chunk.Items[0].RuleID != "" {
		t.Fatalf("origin manual %d %+v", code, chunk)
	}
	if code := e.call(t, http.MethodGet, "/api/jobs?origin=rule", "", &chunk); code != http.StatusOK || len(chunk.Items) != 0 || *chunk.Total != 0 {
		t.Fatalf("origin rule %d %+v", code, chunk)
	}
	var log struct {
		Attempt int           `json:"attempt"`
		Steps   []runner.Step `json:"steps"`
	}
	if code := e.call(t, http.MethodGet, "/api/jobs/"+jid+"/log", "", &log); code != http.StatusOK || log.Attempt != 1 || len(log.Steps) == 0 {
		t.Fatalf("log %d %+v", code, log)
	}
	if code := e.call(t, http.MethodGet, "/api/jobs/"+jid+"/diff", "", nil); code != http.StatusOK {
		t.Fatalf("diff %d", code)
	}
	var attempts struct {
		Attempts []runner.Attempt `json:"attempts"`
	}
	if code := e.call(t, http.MethodGet, "/api/jobs/"+jid+"/attempts", "", &attempts); code != http.StatusOK ||
		len(attempts.Attempts) != 1 || attempts.Attempts[0].Attempt != 1 || attempts.Attempts[0].ErrorCode != "no_folder" {
		t.Fatalf("attempts %d %+v", code, attempts)
	}
	if code := e.call(t, http.MethodGet, "/api/jobs/999999/attempts", "", nil); code != http.StatusNotFound {
		t.Fatalf("attempts of a missing job: %d", code)
	}
	var d store.IssueDetail
	if e.call(t, http.MethodGet, "/api/items/"+id, "", &d); d.Job == nil || d.Job.State != store.JobFailed {
		t.Fatalf("badge %+v", d.Job)
	}
	for path, want := range map[string]int{
		"/api/jobs/" + jid + "/cancel": http.StatusConflict,   // not queued/running
		"/api/jobs/" + jid + "/pr":     http.StatusConflict,   // no publisher configured
		"/api/jobs/" + jid + "/push":   http.StatusConflict,   // not a direct fix with commits
		"/api/jobs/" + jid + "/reply":  http.StatusBadRequest, // bad json
		"/api/jobs/999/retry":          http.StatusNotFound,
		"/api/jobs/x/retry":            http.StatusBadRequest,
	} {
		if code := e.call(t, http.MethodPost, path, "", nil); code != want {
			t.Errorf("POST %s: %d, want %d", path, code, want)
		}
	}
	if code := e.call(t, http.MethodPost, "/api/jobs/"+jid+"/dismiss", "", &j); code != http.StatusOK || j.State != store.JobCancelled {
		t.Fatalf("dismiss %d %+v", code, j)
	}
	if code := e.call(t, http.MethodPost, "/api/jobs/"+jid+"/retry", "", &j); code != http.StatusOK || j.Attempt != 2 {
		t.Fatalf("retry %d %+v", code, j)
	}
	var det []runner.Detected
	if code := e.call(t, http.MethodGet, "/api/agents/detect", "", &det); code != http.StatusOK || len(det) != 2 || det[0].Path != "" {
		t.Fatalf("detect %d %+v", code, det)
	}
}

func TestAutomationLogAPI(t *testing.T) {
	cfg := config.Defaults()
	cfg.Agents.Automation.Enabled = true
	cfg.Agents.Automation.Rules = []config.Rule{{ID: "r1", Enabled: true, Project: "github:octo/app", Event: config.EventNewIssue, Flow: config.FlowReply, LabelsAny: []string{}}}
	var r *runner.Runner
	e := syncedEnv(t, func(o *Options) {
		r = runner.New(runner.Options{Store: o.Store, DataDir: filepath.Join(t.TempDir(), "data"), Settings: func() config.Settings { return cfg }})
		o.Runner = r
	})
	var items store.IssueChunk
	if code := e.call(t, http.MethodGet, "/api/items?state=open", "", &items); code != http.StatusOK || len(items.Items) != 1 {
		t.Fatalf("items %d %+v", code, items)
	}
	r.Automate(t.Context(), []store.Event{{Kind: store.EventNewIssue, ItemID: items.Items[0].ID, Project: "github:octo/app", Repo: "octo/app"}})
	var log store.AutomationChunk
	if code := e.call(t, http.MethodGet, "/api/automation/log?limit=10", "", &log); code != http.StatusOK || len(log.Items) != 1 ||
		log.Items[0].Decision != store.DecisionQueued || log.Items[0].RuleID != "r1" || log.Items[0].JobID == nil || log.Items[0].Repo != "octo/app" {
		t.Fatalf("log %d %+v", code, log)
	}
	if code := e.call(t, http.MethodGet, "/api/automation/log?cursor=bad!", "", nil); code != http.StatusBadRequest {
		t.Fatalf("bad cursor: %d", code)
	}
}

// Agent CLI detection runs once (at start) and GETs answer from the cache;
// ?refresh=1 detects again.
func TestAgentDetectCached(t *testing.T) {
	var looks atomic.Int32
	e := syncedEnv(t, func(o *Options) {
		o.Runner = runner.New(runner.Options{Store: o.Store, DataDir: filepath.Join(t.TempDir(), "data"), Settings: config.Defaults,
			LookPath: func(string) (string, error) { looks.Add(1); return "", context.Canceled }})
	})
	var det []runner.Detected
	for range 2 {
		if code := e.call(t, http.MethodGet, "/api/agents/detect", "", &det); code != http.StatusOK || len(det) != 2 {
			t.Fatalf("detect %d %+v", code, det)
		}
	}
	if n := looks.Load(); n != 2 { // one detection: claude + codex
		t.Fatalf("PATH lookups for two GETs: %d, want 2", n)
	}
	e.call(t, http.MethodGet, "/api/agents/detect?refresh=1", "", &det)
	if n := looks.Load(); n != 4 {
		t.Fatalf("PATH lookups after refresh: %d, want 4", n)
	}
}
