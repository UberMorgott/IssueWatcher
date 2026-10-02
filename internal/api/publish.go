package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/provider"
	"github.com/UberMorgott/issuewatcher/internal/provider/nexus"
	"github.com/UberMorgott/issuewatcher/internal/store"
)

// Publishing a new mod version (Phase 7):
//
//	GET    /api/projects/{id}/publish/targets → provider.PublishTargets
//	POST   /api/projects/{id}/publish  provider.PublishRequest
//	       dryRun → 200 provider.PublishResult {dryRun, plan, md5, size}
//	       else   → 202 PublishTask (runs in the background; SSE publish.progress)
//	GET    /api/publish/{taskId}       → PublishTask
//	DELETE /api/publish/{taskId}       running: cancel (202 PublishTask; the publish
//	                                   call itself is never aborted once sent); finished: forget (204)
//
// Errors: 404 project, 409 {code: unavailable | no_api_key | bad_api_key | busy},
// 400 {code: bad_request}. A failed task keeps uploadId: POST again with
// {uploadId} (no path) to publish the uploaded archive without re-uploading.

// EventPublishProgress carries a PublishTask on each step of a publish.
const EventPublishProgress = "publish.progress"

// Publish task states.
const (
	PublishRunning   = "running"
	PublishDone      = "done"
	PublishFailed    = "failed"
	PublishCancelled = "cancelled"
)

// PublishTask is one publish run (in memory; lost on restart).
type PublishTask struct {
	ID         string                  `json:"id"`
	ProjectID  int64                   `json:"projectId"`
	State      string                  `json:"state"`
	Stage      string                  `json:"stage,omitempty"`
	Sent       int64                   `json:"sent,omitempty"`
	Total      int64                   `json:"total,omitempty"`
	Version    string                  `json:"version"`
	UploadID   string                  `json:"uploadId,omitempty"`
	Result     *provider.PublishResult `json:"result,omitempty"`
	Error      string                  `json:"error,omitempty"`
	ErrorCode  string                  `json:"errorCode,omitempty"` // no_api_key | bad_api_key | upload_failed | publish_failed | cancelled
	StartedAt  string                  `json:"startedAt"`
	FinishedAt string                  `json:"finishedAt,omitempty"`
}

// publishEvery paces upload progress events (state changes go out at once).
const publishEvery = 250 * time.Millisecond

type publishTask struct {
	PublishTask
	cancel context.CancelFunc
	sentAt time.Time
}

type publishTasks struct {
	mu sync.Mutex
	m  map[string]*publishTask
}

func (s *Server) registerPublish(mux *http.ServeMux) {
	s.publishes.m = map[string]*publishTask{}
	mux.HandleFunc("GET /api/projects/{id}/publish/targets", s.handlePublishTargets)
	mux.HandleFunc("POST /api/projects/{id}/publish", s.handlePublish)
	mux.HandleFunc("GET /api/publish/{taskId}", s.handlePublishTask)
	mux.HandleFunc("DELETE /api/publish/{taskId}", s.handlePublishCancel)
}

// publishTarget resolves the project and its publisher; false = answered.
func (s *Server) publishTarget(w http.ResponseWriter, r *http.Request) (int64, provider.Project, provider.Publisher, bool) {
	id, ok := pathID(w, r)
	if !ok {
		return 0, provider.Project{}, nil, false
	}
	rp, err := s.opts.Store.Repo(r.Context(), id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		errJSON(w, http.StatusNotFound, "no such project")
		return 0, provider.Project{}, nil, false
	case err != nil:
		s.internalError(w, "project", err)
		return 0, provider.Project{}, nil, false
	}
	pub := s.opts.Publishers(rp.Platform)
	if pub == nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "publishing is not available for " + rp.Platform, "code": "unavailable"})
		return 0, provider.Project{}, nil, false
	}
	ext := strings.TrimPrefix(rp.Key, rp.Platform+":")
	return id, provider.Project{ExternalID: ext, Name: rp.Name, URL: rp.URL}, pub, true
}

// publishErr answers a failed targets / dry-run / check call.
func (s *Server) publishErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, provider.ErrBadPublish):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error(), "code": "bad_request"})
	case errors.Is(err, nexus.ErrNoAPIKey):
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error(), "code": "no_api_key"})
	case errors.Is(err, nexus.ErrBadAPIKey):
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error(), "code": "bad_api_key"})
	default:
		s.opts.Log.Warn("api: publish", "err", err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error(), "code": "platform_error"})
	}
}

func (s *Server) handlePublishTargets(w http.ResponseWriter, r *http.Request) {
	_, project, pub, ok := s.publishTarget(w, r)
	if !ok {
		return
	}
	t, err := pub.PublishTargets(r.Context(), project)
	if err != nil {
		s.publishErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, t)
}

func (s *Server) handlePublish(w http.ResponseWriter, r *http.Request) {
	id, project, pub, ok := s.publishTarget(w, r)
	if !ok {
		return
	}
	var req provider.PublishRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<10)).Decode(&req); err != nil {
		errJSON(w, http.StatusBadRequest, "bad json")
		return
	}
	if err := pub.CheckPublish(project, req); err != nil {
		s.publishErr(w, err)
		return
	}
	if req.DryRun {
		res, err := pub.Publish(r.Context(), project, req, nil)
		if err != nil {
			s.publishErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, res)
		return
	}
	t, busy := s.startPublish(id, project, pub, req) //nolint:contextcheck // the publish outlives the request: server background context
	if busy {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "a publish of this project is running", "code": "busy", "task": t})
		return
	}
	writeJSON(w, http.StatusAccepted, t)
}

// startPublish runs one publish in the background; one per project at a time.
func (s *Server) startPublish(projectID int64, project provider.Project, pub provider.Publisher, req provider.PublishRequest) (PublishTask, bool) {
	s.publishes.mu.Lock()
	for _, t := range s.publishes.m {
		if t.ProjectID == projectID && t.State == PublishRunning {
			snap := t.PublishTask
			s.publishes.mu.Unlock()
			return snap, true
		}
	}
	ctx, cancel := context.WithCancel(s.bg)
	t := &publishTask{cancel: cancel}
	t.PublishTask = PublishTask{ID: randomHex(8), ProjectID: projectID, State: PublishRunning,
		Version: req.Version, UploadID: req.UploadID, StartedAt: time.Now().UTC().Format(time.RFC3339)}
	s.publishes.m[t.ID] = t
	snap := t.PublishTask
	s.publishes.mu.Unlock()
	s.Publish(EventPublishProgress, snap)

	go func() {
		defer cancel()
		res, err := pub.Publish(ctx, project, req, func(p provider.PublishProgress) { s.publishStep(t, p) })
		s.finishPublish(t, res, err, ctx.Err() != nil)
	}()
	return snap, false
}

func (s *Server) publishStep(t *publishTask, p provider.PublishProgress) {
	s.publishes.mu.Lock()
	changed := t.Stage != p.Stage
	t.Stage, t.Sent, t.Total = p.Stage, p.Sent, p.Total
	now := time.Now()
	if !changed && now.Sub(t.sentAt) < publishEvery && p.Sent < p.Total {
		s.publishes.mu.Unlock()
		return
	}
	t.sentAt = now
	snap := t.PublishTask
	s.publishes.mu.Unlock()
	s.Publish(EventPublishProgress, snap)
}

func (s *Server) finishPublish(t *publishTask, res provider.PublishResult, err error, cancelled bool) {
	s.publishes.mu.Lock()
	t.FinishedAt = time.Now().UTC().Format(time.RFC3339)
	if res.UploadID != "" {
		t.UploadID = res.UploadID
	}
	var pe *provider.PublishError
	switch {
	case err == nil:
		t.State, t.Result = PublishDone, &res
	case errors.As(err, &pe) && pe.Stage == provider.StagePublish:
		t.State, t.Error, t.ErrorCode = PublishFailed, err.Error(), "publish_failed"
	case cancelled:
		t.State, t.Error, t.ErrorCode = PublishCancelled, err.Error(), "cancelled"
	case errors.Is(err, nexus.ErrNoAPIKey):
		t.State, t.Error, t.ErrorCode = PublishFailed, err.Error(), "no_api_key"
	case errors.Is(err, nexus.ErrBadAPIKey):
		t.State, t.Error, t.ErrorCode = PublishFailed, err.Error(), "bad_api_key"
	default:
		t.State, t.Error, t.ErrorCode = PublishFailed, err.Error(), "upload_failed"
	}
	snap := t.PublishTask
	s.publishes.mu.Unlock()
	if err != nil {
		s.opts.Log.Warn("api: publish failed", "project", t.ProjectID, "version", t.Version, "upload", snap.UploadID, "err", err)
	} else {
		s.opts.Log.Info("api: published", "project", t.ProjectID, "version", t.Version, "upload", snap.UploadID)
	}
	s.Publish(EventPublishProgress, snap)
}

func (s *Server) handlePublishTask(w http.ResponseWriter, r *http.Request) {
	s.publishes.mu.Lock()
	t, ok := s.publishes.m[r.PathValue("taskId")]
	var snap PublishTask
	if ok {
		snap = t.PublishTask
	}
	s.publishes.mu.Unlock()
	if !ok {
		errJSON(w, http.StatusNotFound, "no such publish")
		return
	}
	writeJSON(w, http.StatusOK, snap)
}

func (s *Server) handlePublishCancel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("taskId")
	s.publishes.mu.Lock()
	t, ok := s.publishes.m[id]
	if !ok {
		s.publishes.mu.Unlock()
		errJSON(w, http.StatusNotFound, "no such publish")
		return
	}
	if t.State == PublishRunning {
		t.cancel()
		snap := t.PublishTask
		s.publishes.mu.Unlock()
		writeJSON(w, http.StatusAccepted, snap)
		return
	}
	delete(s.publishes.m, id)
	s.publishes.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}
