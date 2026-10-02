package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/UberMorgott/issuewatcher/internal/provider"
	"github.com/UberMorgott/issuewatcher/internal/store"
)

// fakePublisher stands in for a platform's Publisher (provider tests cover Nexus).
type fakePublisher struct {
	mu       sync.Mutex
	project  provider.Project
	reqs     []provider.PublishRequest
	gate     chan struct{} // non-nil: Publish waits for it after the upload stage
	fail     error
	uploadID string
}

func (f *fakePublisher) PublishTargets(_ context.Context, p provider.Project) (provider.PublishTargets, error) {
	f.mu.Lock()
	f.project = p
	f.mu.Unlock()
	return provider.PublishTargets{ModUID: "uid", Files: []provider.PublishFile{{ID: "file-main", Name: "Main"}}}, nil
}

func (f *fakePublisher) CheckPublish(_ provider.Project, req provider.PublishRequest) error {
	if req.Version == "" {
		return fmt.Errorf("%w: version", provider.ErrBadPublish)
	}
	return nil
}

func (f *fakePublisher) Publish(ctx context.Context, p provider.Project, req provider.PublishRequest, progress func(provider.PublishProgress)) (provider.PublishResult, error) {
	f.mu.Lock()
	f.project = p
	f.reqs = append(f.reqs, req)
	gate, fail, id := f.gate, f.fail, f.uploadID
	f.mu.Unlock()
	if req.DryRun {
		return provider.PublishResult{DryRun: true, Plan: []provider.PublishStep{{Method: "POST", URL: "/uploads/multipart"}}}, nil
	}
	progress(provider.PublishProgress{Stage: provider.StageUpload, Sent: 5, Total: 10})
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return provider.PublishResult{}, &provider.PublishError{Stage: provider.StageUpload, Err: ctx.Err()}
		}
	}
	progress(provider.PublishProgress{Stage: provider.StagePublish})
	if fail != nil {
		return provider.PublishResult{UploadID: id}, &provider.PublishError{Stage: provider.StagePublish, UploadID: id, Err: fail}
	}
	return provider.PublishResult{UploadID: id, VersionID: "ver-3"}, nil
}

func publishEnv(t *testing.T, pub provider.Publisher) (*env, int64) {
	t.Helper()
	e := syncedEnv(t, func(o *Options) {
		o.Publishers = func(platform string) provider.Publisher {
			if platform == "github" && pub != nil {
				return pub
			}
			return nil
		}
	})
	var repos []store.Repo
	if code := e.call(t, http.MethodGet, "/api/projects", "", &repos); code != http.StatusOK || len(repos) != 1 {
		t.Fatalf("repos %d %+v", code, repos)
	}
	return e, repos[0].ID
}

type apiErr struct {
	Code string `json:"code"`
}

func TestPublishUnavailable(t *testing.T) {
	e, id := publishEnv(t, nil)
	var res apiErr
	if code := e.callAny(t, http.MethodGet, fmt.Sprintf("/api/projects/%d/publish/targets", id), "", &res); code != http.StatusConflict || res.Code != "unavailable" {
		t.Fatalf("targets: %d %+v", code, res)
	}
	if code := e.callAny(t, http.MethodGet, "/api/projects/99999/publish/targets", "", &res); code != http.StatusNotFound {
		t.Fatalf("unknown project: %d", code)
	}
}

// Targets, dry run, a background task with paced SSE progress, one task per
// project, a failed publish that keeps the upload id, and forgetting a task.
func TestPublishTaskFlow(t *testing.T) {
	fp := &fakePublisher{gate: make(chan struct{}), uploadID: "up-1"}
	e, id := publishEnv(t, fp)
	base := fmt.Sprintf("/api/projects/%d/publish", id)

	var tg provider.PublishTargets
	if code := e.call(t, http.MethodGet, base+"/targets", "", &tg); code != http.StatusOK || tg.ModUID != "uid" || fp.project.ExternalID != "octo/app" {
		t.Fatalf("targets %d %+v %+v", code, tg, fp.project)
	}
	var bad apiErr
	if code := e.callAny(t, http.MethodPost, base, `{"fileId":"file-main"}`, &bad); code != http.StatusBadRequest || bad.Code != "bad_request" {
		t.Fatalf("invalid: %d %+v", code, bad)
	}
	var dry provider.PublishResult
	if code := e.call(t, http.MethodPost, base, `{"fileId":"file-main","path":"C:\\a.zip","version":"0.3.0","dryRun":true}`, &dry); code != http.StatusOK ||
		!dry.DryRun || len(dry.Plan) != 1 {
		t.Fatalf("dry run %d %+v", code, dry)
	}

	br := openStream(t, e.s)
	var task PublishTask
	if code := e.call(t, http.MethodPost, base, `{"fileId":"file-main","path":"C:\\a.zip","version":"0.3.0"}`, &task); code != http.StatusAccepted ||
		task.State != PublishRunning || task.ID == "" {
		t.Fatalf("start %d %+v", code, task)
	}
	var busy struct {
		Code string      `json:"code"`
		Task PublishTask `json:"task"`
	}
	if code := e.callAny(t, http.MethodPost, base, `{"fileId":"file-main","path":"C:\\a.zip","version":"0.3.0"}`, &busy); code != http.StatusConflict ||
		busy.Code != "busy" || busy.Task.ID != task.ID {
		t.Fatalf("second start %d %+v", code, busy)
	}
	waitFor(t, func() bool {
		var cur PublishTask
		e.call(t, http.MethodGet, "/api/publish/"+task.ID, "", &cur)
		return cur.Stage == provider.StageUpload && cur.Sent == 5
	})
	close(fp.gate)
	var states []string
	for len(states) == 0 || states[len(states)-1] == PublishRunning {
		name, data := readEvent(t, br)
		if name != EventPublishProgress {
			continue
		}
		var ev PublishTask
		if err := json.Unmarshal([]byte(data), &ev); err != nil || ev.ID != task.ID {
			t.Fatalf("event %s: %v", data, err)
		}
		states = append(states, ev.State)
	}
	if states[len(states)-1] != PublishDone {
		t.Fatalf("states %v", states)
	}
	var done PublishTask
	if code := e.call(t, http.MethodGet, "/api/publish/"+task.ID, "", &done); code != http.StatusOK ||
		done.State != PublishDone || done.Result == nil || done.Result.VersionID != "ver-3" || done.UploadID != "up-1" {
		t.Fatalf("done %d %+v", code, done)
	}

	// A failed publish keeps the upload id (retry with it, no new upload).
	fp.mu.Lock()
	fp.gate, fp.fail = nil, fmt.Errorf("HTTP 500")
	fp.mu.Unlock()
	if code := e.call(t, http.MethodPost, base, `{"fileId":"file-main","path":"C:\\a.zip","version":"0.3.0"}`, &task); code != http.StatusAccepted {
		t.Fatalf("start 2: %d", code)
	}
	var failed PublishTask
	waitFor(t, func() bool {
		e.call(t, http.MethodGet, "/api/publish/"+task.ID, "", &failed)
		return failed.State != PublishRunning
	})
	if failed.State != PublishFailed || failed.ErrorCode != "publish_failed" || failed.UploadID != "up-1" || !strings.Contains(failed.Error, "up-1") {
		t.Fatalf("failed %+v", failed)
	}
	fp.mu.Lock()
	n := len(fp.reqs)
	fp.mu.Unlock()
	if n != 3 { // dry run + two runs: the server never re-sends a publish
		t.Fatalf("publish calls %d", n)
	}
	if code := e.call(t, http.MethodDelete, "/api/publish/"+task.ID, "", nil); code != http.StatusNoContent {
		t.Fatalf("forget %d", code)
	}
	if code := e.call(t, http.MethodGet, "/api/publish/"+task.ID, "", nil); code != http.StatusNotFound {
		t.Fatalf("forgotten task %d", code)
	}
}

// DELETE on a running task cancels it.
func TestPublishCancel(t *testing.T) {
	fp := &fakePublisher{gate: make(chan struct{})}
	e, id := publishEnv(t, fp)
	var task PublishTask
	if code := e.call(t, http.MethodPost, fmt.Sprintf("/api/projects/%d/publish", id), `{"fileId":"f","path":"C:\\a.zip","version":"1"}`, &task); code != http.StatusAccepted {
		t.Fatalf("start %d", code)
	}
	if code := e.call(t, http.MethodDelete, "/api/publish/"+task.ID, "", nil); code != http.StatusAccepted {
		t.Fatalf("cancel %d", code)
	}
	var cur PublishTask
	waitFor(t, func() bool {
		e.call(t, http.MethodGet, "/api/publish/"+task.ID, "", &cur)
		return cur.State != PublishRunning
	})
	if cur.State != PublishCancelled || cur.ErrorCode != "cancelled" {
		t.Fatalf("cancelled %+v", cur)
	}
}
