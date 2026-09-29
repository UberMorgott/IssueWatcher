package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"

	"github.com/UberMorgott/issuewatcher/internal/provider"
	"github.com/UberMorgott/issuewatcher/internal/runner"
	"github.com/UberMorgott/issuewatcher/internal/store"
)

// Agent jobs (docs/ARCHITECTURE.md → HTTP API, Runner):
//
//	GET  /api/jobs?state=&flow=&origin=&project=&item=&cursor=&limit=  keyset chunk, newest first
//	POST /api/jobs {itemIds[], flow: fix|reply, profileId?}    one job per item → 201 {jobs:[{itemId, job?, error?}]};
//	                                     fix items without a usable local folder get error "no_folder" (no job);
//	                                     when that is every item → 409 {error, code: "no_folder", jobs[, hint: "link_mod"]}
//	GET  /api/jobs/{id}                  job
//	GET  /api/jobs/{id}/log?attempt=     {attempt, steps:[{t, kind, text}]}
//	GET  /api/jobs/{id}/diff?attempt=    unified diff (text/plain)
//	GET  /api/jobs/{id}/attempts         {attempts:[{attempt, state, error, errorCode, result, startedAt, finishedAt}]} earlier snapshots + current
//	POST /api/jobs/{id}/cancel|retry|dismiss|pr|push   job (push: direct fix commits → the platform)
//	POST /api/jobs/{id}/reply {body}     post the (edited) reply draft
//	POST /api/jobs/{id}/labels {labels[]} add labels to the label job's issue (checked, add only) → done
//	GET  /api/projects/{id}/labels       [{name, color, description}] the project's labels
//	POST /api/projects/{id}/triage {profileId?}  queue the project triage → 201 job (409 {error, job} when one is unfinished)
//	GET  /api/agents/detect              [{cli, path, version}] CLIs on PATH
//	GET  /api/automation/log?cursor=&limit=  rule decisions, newest first {items, nextCursor, more}
//
// SSE: job.changed (job), job.log {id, attempt, steps}.

// SSE event names of the runner.
const (
	EventJobChanged = "job.changed"
	EventJobLog     = "job.log"
)

// JobSteps is the job.log payload.
type JobSteps struct {
	ID      int64         `json:"id"`
	Attempt int           `json:"attempt"`
	Steps   []runner.Step `json:"steps"`
}

func (s *Server) registerJobs(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/jobs", s.handleJobs)
	mux.HandleFunc("POST /api/jobs", s.handleJobsCreate)
	mux.HandleFunc("GET /api/jobs/{id}", s.handleJob)
	mux.HandleFunc("GET /api/jobs/{id}/log", s.handleJobLog)
	mux.HandleFunc("GET /api/jobs/{id}/diff", s.handleJobDiff)
	mux.HandleFunc("GET /api/jobs/{id}/attempts", s.handleJobAttempts)
	mux.HandleFunc("POST /api/jobs/{id}/reply", s.handleJobReply)
	mux.HandleFunc("POST /api/jobs/{id}/labels", s.handleJobLabels)
	mux.HandleFunc("GET /api/projects/{id}/labels", s.handleProjectLabels)
	mux.HandleFunc("POST /api/projects/{id}/triage", s.handleProjectTriage)
	for action, f := range map[string]func(context.Context, int64) (store.Job, error){
		"cancel": s.opts.Runner.Cancel, "retry": s.opts.Runner.Retry, "dismiss": s.opts.Runner.Dismiss, "pr": s.opts.Runner.CreatePR,
		"push": s.opts.Runner.Push,
	} {
		mux.HandleFunc("POST /api/jobs/{id}/"+action, s.jobAction(f))
	}
	mux.HandleFunc("GET /api/agents/detect", s.handleDetect)
	mux.HandleFunc("GET /api/automation/log", s.handleAutomationLog)
}

func (s *Server) handleJobs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	project, err1 := queryInt(r, "project")
	item, err2 := queryInt(r, "item")
	limit, err3 := queryInt(r, "limit")
	if err := errors.Join(err1, err2, err3); err != nil {
		errJSON(w, http.StatusBadRequest, err.Error())
		return
	}
	f := store.JobFilter{State: q.Get("state"), Flow: q.Get("flow"), Origin: q.Get("origin"), ProjectID: project, ItemID: item, Cursor: q.Get("cursor"), Limit: int(min(limit, 1000))}
	chunk, err := s.opts.Store.Jobs(r.Context(), f)
	if errors.Is(err, store.ErrBadCursor) {
		errJSON(w, http.StatusBadRequest, "bad cursor")
		return
	}
	if err != nil {
		s.internalError(w, "list jobs", err)
		return
	}
	writeJSON(w, http.StatusOK, chunk)
}

func (s *Server) handleAutomationLog(w http.ResponseWriter, r *http.Request) {
	limit, err := queryInt(r, "limit")
	if err != nil {
		errJSON(w, http.StatusBadRequest, err.Error())
		return
	}
	chunk, err := s.opts.Store.AutomationLog(r.Context(), r.URL.Query().Get("cursor"), int(min(limit, 1000)))
	if errors.Is(err, store.ErrBadCursor) {
		errJSON(w, http.StatusBadRequest, "bad cursor")
		return
	}
	if err != nil {
		s.internalError(w, "automation log", err)
		return
	}
	writeJSON(w, http.StatusOK, chunk)
}

func (s *Server) handleJobsCreate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ItemIDs   []int64 `json:"itemIds"`
		Flow      string  `json:"flow"`
		ProfileID string  `json:"profileId"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		errJSON(w, http.StatusBadRequest, "bad json")
		return
	}
	out, err := s.opts.Runner.Enqueue(r.Context(), req.ItemIDs, req.Flow, req.ProfileID)
	if errors.Is(err, runner.ErrBadRequest) {
		errJSON(w, http.StatusBadRequest, err.Error())
		return
	}
	if errors.Is(err, runner.ErrUnavailable) {
		errJSON(w, http.StatusConflict, err.Error())
		return
	}
	if errors.Is(err, runner.ErrNoFolder) { // fix for items without a usable folder: nothing was queued
		body := map[string]any{"error": err.Error(), "code": runner.CodeNoFolder, "jobs": out}
		if !slices.ContainsFunc(out, func(q runner.Queued) bool { return q.Hint != runner.HintLinkMod }) {
			body["hint"] = runner.HintLinkMod // every item is a mod page not linked to a code project
			body["error"] = "the mod page is not linked to a code project; link the mod to a project with a local folder (Projects)"
		}
		writeJSON(w, http.StatusConflict, body)
		return
	}
	if err != nil {
		s.internalError(w, "queue jobs", err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"jobs": out})
}

func (s *Server) handleJob(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	j, err := s.opts.Store.Job(r.Context(), id)
	if s.jobError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, j)
}

// attempt reads ?attempt= (default: the job's current attempt).
func (s *Server) attempt(w http.ResponseWriter, r *http.Request) (int64, int, bool) {
	id, ok := pathID(w, r)
	if !ok {
		return 0, 0, false
	}
	a, err := queryInt(r, "attempt")
	if err != nil {
		errJSON(w, http.StatusBadRequest, err.Error())
		return 0, 0, false
	}
	if a == 0 {
		j, err := s.opts.Store.Job(r.Context(), id)
		if s.jobError(w, err) {
			return 0, 0, false
		}
		a = int64(j.Attempt)
	}
	return id, int(a), true
}

func (s *Server) handleJobLog(w http.ResponseWriter, r *http.Request) {
	id, attempt, ok := s.attempt(w, r)
	if !ok {
		return
	}
	steps, err := s.opts.Runner.Log(id, attempt)
	if err != nil {
		s.internalError(w, "job log", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"attempt": attempt, "steps": steps})
}

func (s *Server) handleJobDiff(w http.ResponseWriter, r *http.Request) {
	id, attempt, ok := s.attempt(w, r)
	if !ok {
		return
	}
	diff, err := s.opts.Runner.Diff(id, attempt)
	if err != nil {
		s.internalError(w, "job diff", err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(diff))
}

func (s *Server) handleJobAttempts(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	attempts, err := s.opts.Runner.Attempts(r.Context(), id)
	if s.jobError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"attempts": attempts})
}

func (s *Server) jobAction(f func(context.Context, int64) (store.Job, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(w, r)
		if !ok {
			return
		}
		j, err := f(r.Context(), id)
		if s.jobError(w, err) {
			return
		}
		writeJSON(w, http.StatusOK, j)
	}
}

func (s *Server) handleJobReply(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var req struct {
		Body string `json:"body"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxCommentBytes)).Decode(&req); err != nil {
		errJSON(w, http.StatusBadRequest, "bad json")
		return
	}
	j, err := s.opts.Runner.SendReply(r.Context(), id, req.Body)
	if s.jobError(w, err) {
		return
	}
	s.dataChanged("reply", j.ItemID)
	writeJSON(w, http.StatusOK, j)
}

func (s *Server) handleJobLabels(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var req struct {
		Labels []string `json:"labels"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		errJSON(w, http.StatusBadRequest, "bad json")
		return
	}
	j, err := s.opts.Runner.ApplyLabels(r.Context(), id, req.Labels)
	if s.jobError(w, err) {
		return
	}
	s.dataChanged("labels", j.ItemID)
	writeJSON(w, http.StatusOK, j)
}

// handleProjectTriage queues the project's triage job; an unfinished one →
// 409 {error, job}.
func (s *Server) handleProjectTriage(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var req struct {
		ProfileID string `json:"profileId"`
	}
	if r.ContentLength != 0 {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
			errJSON(w, http.StatusBadRequest, "bad json")
			return
		}
	}
	j, err := s.opts.Runner.Triage(r.Context(), id, req.ProfileID)
	if errors.Is(err, store.ErrJobExists) {
		writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error(), "job": j})
		return
	}
	if s.jobError(w, err) {
		return
	}
	writeJSON(w, http.StatusCreated, j)
}

// jobError writes the HTTP error for a runner/store error; false when err is nil.
// A publish that failed returns 502 with the job (state back to needs_review).
func (s *Server) jobError(w http.ResponseWriter, err error) bool {
	var rl *provider.RateLimitError
	switch {
	case err == nil:
		return false
	case errors.Is(err, store.ErrNotFound):
		errJSON(w, http.StatusNotFound, "not found")
	case errors.Is(err, runner.ErrModItem):
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error(), "code": runner.CodeModItem})
	case errors.Is(err, runner.ErrNotAllowed), errors.Is(err, store.ErrJobExists):
		errJSON(w, http.StatusConflict, err.Error())
	case errors.Is(err, runner.ErrBadRequest):
		errJSON(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, runner.ErrUnavailable), errors.Is(err, provider.ErrNotSignedIn):
		errJSON(w, http.StatusConflict, err.Error())
	case errors.As(err, &rl):
		errJSON(w, http.StatusTooManyRequests, err.Error())
	default:
		s.opts.Log.Error("api: job action", "err", err)
		errJSON(w, http.StatusBadGateway, err.Error())
	}
	return true
}

// handleDetect answers from the cached detection (agentDetect); ?refresh=1
// detects again.
func (s *Server) handleDetect(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.detectAgents(r.Context(), r.URL.Query().Get("refresh") == "1"))
}
