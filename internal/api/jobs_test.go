package api

import (
	"context"
	"net/http"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/config"
	"github.com/UberMorgott/issuewatcher/internal/runner"
	"github.com/UberMorgott/issuewatcher/internal/store"
)

func TestJobsAPI(t *testing.T) {
	var r *runner.Runner
	e := syncedEnv(t, func(o *Options) {
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
		`{"itemIds":[` + id + `],"flow":"label"}`:                    http.StatusBadRequest,
		`{"itemIds":[],"flow":"fix"}`:                                http.StatusBadRequest,
		`{"itemIds":[` + id + `],"flow":"fix","profileId":"nobody"}`: http.StatusBadRequest,
		`nope`: http.StatusBadRequest,
	} {
		if code := e.call(t, http.MethodPost, "/api/jobs", body, nil); code != want {
			t.Errorf("POST %s: %d, want %d", body, code, want)
		}
	}
	var created struct {
		Jobs []runner.Queued `json:"jobs"`
	}
	if code := e.call(t, http.MethodPost, "/api/jobs", `{"itemIds":[`+id+`],"flow":"fix"}`, &created); code != http.StatusCreated ||
		len(created.Jobs) != 1 || created.Jobs[0].Job == nil || created.Jobs[0].Job.ProfileID != "claude" {
		t.Fatalf("create %d %+v", code, created)
	}
	jid := strconv.FormatInt(created.Jobs[0].Job.ID, 10)

	// The project has no local folder: the job fails with a code the UI links.
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
	var chunk store.JobChunk
	if code := e.call(t, http.MethodGet, "/api/jobs?state=failed&flow=fix", "", &chunk); code != http.StatusOK || len(chunk.Items) != 1 || *chunk.Total != 1 {
		t.Fatalf("list %d %+v", code, chunk)
	}
	if code := e.call(t, http.MethodGet, "/api/jobs?state=active", "", &chunk); code != http.StatusOK || len(chunk.Items) != 0 {
		t.Fatalf("active %d %+v", code, chunk)
	}
	if code := e.call(t, http.MethodGet, "/api/jobs?origin=manual", "", &chunk); code != http.StatusOK || len(chunk.Items) != 1 ||
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
