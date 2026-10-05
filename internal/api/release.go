package api

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/UberMorgott/issuewatcher/internal/config"
	"github.com/UberMorgott/issuewatcher/internal/release"
	"github.com/UberMorgott/issuewatcher/internal/store"
)

// Autopilot release runs (docs/AUTOPILOT.md, Phase 1):
//
//	GET  /api/projects/{id}/publish-profile          → ProfileDoc (profile + autopilot + resolved plan)
//	PUT  /api/projects/{id}/publish-profile          {revision?, publishProfile?, autopilot?} → ProfileDoc   (agent callers refused)
//	POST /api/projects/{id}/publish-profile/check    release.Request? → release.Plan
//	POST /api/projects/{id}/release/plan             release.Request → release.Plan
//	POST /api/projects/{id}/release                  release.Request: dryRun → 200 Plan; else 202 {run, plan}  (agent callers refused)
//	                                                 refused → 409 {error, code, refusals, plan}
//	GET  /api/runs?project=&kind=&state=&limit=      → []store.Run (newest first)
//	GET  /api/runs/{id}                              → release.RunView
//	POST /api/runs/{id}/resume                       → 202 store.Run
//	POST /api/runs/{id}/cancel                       → release.CancelResult
//	POST /api/runs/{id}/skip {step, target}          → store.Run   (agent callers refused)
//	POST /api/autopilot/pause {project?, paused}     → {paused, project?, enabled?}  (clearing refused for agent callers)
//	GET  /api/autopilot/events?unreadOnly=&limit=    → {events, unread, attention}
//	POST /api/autopilot/events/read {ids?, all?}     → {marked}
//
// SSE: autopilot.run (release.RunView) on every run / step change,
// autopilot.event (store.AutopilotEvent) on every activity log entry.

// SSE events of the autopilot.
const (
	EventAutopilotRun   = "autopilot.run"
	EventAutopilotEvent = "autopilot.event"
)

// codeAgentCaller is the 403 code for a refused agent run caller.
const codeAgentCaller = "agent_caller"

func (s *Server) registerRelease(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/projects/{id}/publish-profile", s.handleProfileGet)
	mux.HandleFunc("PUT /api/projects/{id}/publish-profile", s.handleProfilePut)
	mux.HandleFunc("POST /api/projects/{id}/publish-profile/check", s.handlePlan)
	mux.HandleFunc("POST /api/projects/{id}/release/plan", s.handlePlan)
	mux.HandleFunc("POST /api/projects/{id}/release", s.handleRelease)
	mux.HandleFunc("GET /api/runs", s.handleRuns)
	mux.HandleFunc("GET /api/runs/{id}", s.handleRun)
	mux.HandleFunc("POST /api/runs/{id}/resume", s.handleRunResume)
	mux.HandleFunc("POST /api/runs/{id}/cancel", s.handleRunCancel)
	mux.HandleFunc("POST /api/runs/{id}/skip", s.handleRunSkip)
	mux.HandleFunc("POST /api/autopilot/pause", s.handlePause)
	mux.HandleFunc("GET /api/autopilot/events", s.handleAutopilotEvents)
	mux.HandleFunc("POST /api/autopilot/events/read", s.handleAutopilotEventsRead)
}

// refuseAgent answers 403 {code: agent_caller} when the loopback client is a
// process inside a runner job object (an agent run, its shell or its MCP
// server); a client that cannot be resolved is refused too (caller_unknown).
func (s *Server) refuseAgent(w http.ResponseWriter, r *http.Request) bool {
	local := ""
	if a, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr); ok {
		local = a.String()
	}
	pid, err := s.callerPID(r.RemoteAddr, local)
	if err != nil {
		s.opts.Log.Warn("api: caller pid", "remote", r.RemoteAddr, "err", err)
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "cannot tell which program called: refused", "code": "caller_unknown"})
		return true
	}
	if s.inAgentJob(pid) {
		s.opts.Log.Warn("api: agent caller refused", "pid", pid, "path", r.URL.Path)
		writeJSON(w, http.StatusForbidden, map[string]string{
			"error": "refused: agent runs cannot release, publish, change the publish profile, skip steps or clear the pause",
			"code":  codeAgentCaller,
		})
		return true
	}
	return false
}

// origin of a release: the browser session = the owner's click (manual); the
// bearer token = MCP / CLI.
func (s *Server) origin(r *http.Request) string {
	if c, err := r.Cookie(sessionCookie); err == nil && equal(c.Value, s.session) {
		return store.RunOriginManual
	}
	return store.RunOriginMCP
}

// RunChanged publishes autopilot.run for run id (release.Deps.OnChange).
func (s *Server) RunChanged(id int64) {
	if s.opts.Release == nil {
		return
	}
	v, err := s.opts.Release.View(s.bg, id)
	if err != nil {
		return
	}
	s.Publish(EventAutopilotRun, v)
}

// AutopilotEvent publishes autopilot.event (release.Deps.OnEvent).
func (s *Server) AutopilotEvent(ev store.AutopilotEvent) { s.Publish(EventAutopilotEvent, ev) }

// codeProject resolves {id} to a GitHub code project; false = answered.
func (s *Server) codeProject(w http.ResponseWriter, r *http.Request) (store.Repo, bool) {
	id, ok := pathID(w, r)
	if !ok {
		return store.Repo{}, false
	}
	rp, err := s.opts.Store.Repo(r.Context(), id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		errJSON(w, http.StatusNotFound, "no such project")
		return rp, false
	case err != nil:
		s.internalError(w, "project", err)
		return rp, false
	case rp.Platform != "github":
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "releases belong to the GitHub code project of a mod", "code": "not_code_project"})
		return rp, false
	}
	return rp, true
}

// ProfileKinds are the allowed source kinds (for editors and agents).
type ProfileKinds struct {
	Version   []string `json:"version"`
	Changelog []string `json:"changelog"`
	Smoke     []string `json:"smoke"`
}

// ProfileDoc is GET/PUT /api/projects/{id}/publish-profile (MCP get_publish_profile).
type ProfileDoc struct {
	ProjectID      int64                   `json:"projectId"`
	Project        string                  `json:"project"`
	Revision       int                     `json:"revision"`
	PublishProfile config.PublishProfile   `json:"publishProfile"`
	Autopilot      config.ProjectAutopilot `json:"autopilot"`
	Global         config.AgentsAutopilot  `json:"global"`
	Kinds          ProfileKinds            `json:"kinds"`
	// Resolved is the dry-run plan: current / next version, changelog preview,
	// targets with their latest versions and auth state, steps, refusals.
	Resolved release.Plan `json:"resolved"`
}

func (s *Server) profileDoc(r *http.Request, rp store.Repo) (ProfileDoc, error) {
	doc, err := s.opts.Settings.Settings()
	if err != nil {
		return ProfileDoc{}, err
	}
	ag := doc.Settings.Agents
	pa := ag.Projects[rp.Key]
	plan, err := s.opts.Release.Plan(r.Context(), rp.ID, release.Request{})
	if err != nil {
		return ProfileDoc{}, err
	}
	return ProfileDoc{ProjectID: rp.ID, Project: rp.Key, Revision: doc.Revision, PublishProfile: pa.PublishProfile,
		Autopilot: ag.AutopilotFor(rp.Key), Global: ag.Autopilot, Resolved: plan,
		Kinds: ProfileKinds{
			Version:   []string{config.VersionFactorioInfo, config.VersionJSON, config.VersionRegex, config.VersionGitTag},
			Changelog: []string{config.ChangelogFactorio, config.ChangelogKeepAChangelog, config.ChangelogCommits},
			Smoke:     []string{config.SmokeFactorio, config.SmokeCommand, config.SmokeNone},
		}}, nil
}

func (s *Server) handleProfileGet(w http.ResponseWriter, r *http.Request) {
	rp, ok := s.codeProject(w, r)
	if !ok {
		return
	}
	doc, err := s.profileDoc(r, rp)
	if err != nil {
		s.internalError(w, "publish profile", err)
		return
	}
	writeJSON(w, http.StatusOK, doc)
}

// handleProfilePut replaces the project's publishProfile and/or autopilot
// blocks (a missing one stays); validation errors → 400, a stale revision → 409.
func (s *Server) handleProfilePut(w http.ResponseWriter, r *http.Request) {
	rp, ok := s.codeProject(w, r)
	if !ok || s.refuseAgent(w, r) {
		return
	}
	var req struct {
		Revision       *int                     `json:"revision"`
		PublishProfile *config.PublishProfile   `json:"publishProfile"`
		Autopilot      *config.ProjectAutopilot `json:"autopilot"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<10)).Decode(&req); err != nil ||
		(req.PublishProfile == nil && req.Autopilot == nil) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "body must be {revision?, publishProfile?, autopilot?}", "code": "bad_request"})
		return
	}
	cur, err := s.opts.Settings.Settings()
	if err != nil {
		s.internalError(w, "read settings", err)
		return
	}
	rev := cur.Revision
	if req.Revision != nil {
		rev = *req.Revision
	}
	pa := cur.Settings.Agents.Projects[rp.Key]
	block := map[string]any{}
	if req.PublishProfile != nil {
		block["publishProfile"] = mergeDiff(toJSONValue(pa.PublishProfile), toJSONValue(*req.PublishProfile))
	}
	if req.Autopilot != nil {
		block["autopilot"] = mergeDiff(toJSONValue(cur.Settings.Agents.AutopilotFor(rp.Key)), toJSONValue(*req.Autopilot))
	}
	patch, _ := json.Marshal(map[string]any{"agents": map[string]any{"projects": map[string]any{rp.Key: block}}})
	doc, err := s.opts.Settings.PatchSettings(rev, patch)
	if err != nil {
		s.settingsResult(w, doc, err)
		return
	}
	s.Publish(EventSettingsChanged, doc)
	out, err := s.profileDoc(r, rp)
	if err != nil {
		s.internalError(w, "publish profile", err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// toJSONValue is v as decoded JSON (maps, slices, scalars).
func toJSONValue(v any) any {
	b, _ := json.Marshal(v)
	var out any
	_ = json.Unmarshal(b, &out)
	return out
}

// mergeDiff is the JSON merge patch (RFC 7386) that turns old into next:
// keys gone from next become null.
func mergeDiff(old, next any) any {
	nm, ok := next.(map[string]any)
	if !ok {
		return next
	}
	om, _ := old.(map[string]any)
	out := map[string]any{}
	for k := range om {
		if _, keep := nm[k]; !keep {
			out[k] = nil
		}
	}
	for k, v := range nm {
		out[k] = mergeDiff(om[k], v)
	}
	return out
}

func decodeRequest(w http.ResponseWriter, r *http.Request, req *release.Request) bool {
	if r.ContentLength == 0 {
		return true
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad json", "code": "bad_request"})
		return false
	}
	return true
}

func (s *Server) handlePlan(w http.ResponseWriter, r *http.Request) {
	rp, ok := s.codeProject(w, r)
	if !ok {
		return
	}
	var req release.Request
	if !decodeRequest(w, r, &req) {
		return
	}
	p, err := s.opts.Release.Plan(r.Context(), rp.ID, req)
	if err != nil {
		s.internalError(w, "release plan", err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) handleRelease(w http.ResponseWriter, r *http.Request) {
	rp, ok := s.codeProject(w, r)
	if !ok {
		return
	}
	var req release.Request
	if !decodeRequest(w, r, &req) {
		return
	}
	if !req.DryRun && s.refuseAgent(w, r) {
		return
	}
	req.Origin = s.origin(r)
	run, p, err := s.opts.Release.Release(r.Context(), rp.ID, req)
	var re *release.RefusedError
	switch {
	case errors.As(err, &re):
		writeJSON(w, http.StatusConflict, map[string]any{"error": re.Error(), "code": re.Plan.Refusals[0].Code,
			"refusals": re.Plan.Refusals, "plan": re.Plan})
	case err != nil:
		s.internalError(w, "release", err)
	case req.DryRun:
		writeJSON(w, http.StatusOK, p)
	default:
		writeJSON(w, http.StatusAccepted, map[string]any{"run": run, "plan": p})
	}
}

func (s *Server) handleRuns(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := store.RunFilter{Kind: q.Get("kind"), State: q.Get("state")}
	if v := q.Get("project"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			errJSON(w, http.StatusBadRequest, "bad project")
			return
		}
		f.ProjectID = n
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 || n > 500 {
			errJSON(w, http.StatusBadRequest, "bad limit (1-500)")
			return
		}
		f.Limit = n
	}
	runs, err := s.opts.Store.Runs(r.Context(), f)
	if err != nil {
		s.internalError(w, "runs", err)
		return
	}
	writeJSON(w, http.StatusOK, runs)
}

// runErr answers a failed run action.
func (s *Server) runErr(w http.ResponseWriter, what string, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		errJSON(w, http.StatusNotFound, "no such run or step")
	case errors.Is(err, release.ErrState):
		writeJSON(w, http.StatusConflict, map[string]string{"error": "the run's state does not allow this", "code": "bad_state"})
	case errors.Is(err, release.ErrBadStep):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error(), "code": "bad_step"})
	default:
		s.internalError(w, what, err)
	}
}

func (s *Server) handleRun(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	v, err := s.opts.Release.View(r.Context(), id)
	if err != nil {
		s.runErr(w, "run", err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) handleRunResume(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	run, err := s.opts.Release.Resume(r.Context(), id)
	if err != nil {
		s.runErr(w, "resume", err)
		return
	}
	writeJSON(w, http.StatusAccepted, run)
}

func (s *Server) handleRunCancel(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	res, err := s.opts.Release.Cancel(r.Context(), id)
	if err != nil {
		s.runErr(w, "cancel", err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleRunSkip(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var req struct {
		Step   string `json:"step"`
		Target string `json:"target"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "body must be {step, target}", "code": "bad_request"})
		return
	}
	if step, target, ok := strings.Cut(req.Step, ":"); ok && req.Target == "" { // "publish:nexus:x/1"
		req.Step, req.Target = step, target
	}
	if s.refuseAgent(w, r) {
		return
	}
	run, err := s.opts.Release.Skip(r.Context(), id, req.Step, req.Target)
	if err != nil {
		s.runErr(w, "skip", err)
		return
	}
	writeJSON(w, http.StatusOK, run)
}

// handlePause sets / clears the kill switch: global agents.autopilot.paused,
// or with project the project's autopilot.enabled (paused = !enabled).
func (s *Server) handlePause(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Project *int64 `json:"project"`
		Paused  *bool  `json:"paused"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil || req.Paused == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "body must be {project?, paused}", "code": "bad_request"})
		return
	}
	if !*req.Paused && s.refuseAgent(w, r) {
		return
	}
	cur, err := s.opts.Settings.Settings()
	if err != nil {
		s.internalError(w, "read settings", err)
		return
	}
	var patch map[string]any
	key := ""
	if req.Project != nil {
		rp, err := s.opts.Store.Repo(r.Context(), *req.Project)
		if err != nil {
			s.runErr(w, "project", err)
			return
		}
		key = rp.Key
		patch = map[string]any{"agents": map[string]any{"projects": map[string]any{key: map[string]any{
			"autopilot": map[string]any{"enabled": !*req.Paused}}}}}
	} else {
		patch = map[string]any{"agents": map[string]any{"autopilot": map[string]any{"paused": *req.Paused}}}
	}
	b, _ := json.Marshal(patch)
	doc, err := s.opts.Settings.PatchSettings(cur.Revision, b)
	if err != nil {
		s.settingsResult(w, doc, err)
		return
	}
	s.Publish(EventSettingsChanged, doc)
	out := map[string]any{"paused": doc.Settings.Agents.Autopilot.Paused}
	if key != "" {
		out["project"] = *req.Project
		out["enabled"] = doc.Settings.Agents.AutopilotFor(key).Enabled
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleAutopilotEvents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	unreadOnly := q.Get("unreadOnly") == "true" || q.Get("unreadOnly") == "1"
	limit := 0
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 || n > 5000 {
			errJSON(w, http.StatusBadRequest, "bad limit (1-5000)")
			return
		}
		limit = n
	}
	evs, err := s.opts.Store.Events(r.Context(), unreadOnly, limit)
	if err != nil {
		s.internalError(w, "autopilot events", err)
		return
	}
	unread, attention, err := s.opts.Store.UnreadEventCount(r.Context())
	if err != nil {
		s.internalError(w, "autopilot events", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": evs, "unread": unread, "attention": attention})
}

func (s *Server) handleAutopilotEventsRead(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IDs []int64 `json:"ids"`
		All bool    `json:"all"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil || (!req.All && len(req.IDs) == 0) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "body must be {ids} or {all: true}", "code": "bad_request"})
		return
	}
	n, err := s.opts.Store.MarkEventsRead(r.Context(), req.IDs, req.All)
	if err != nil {
		s.internalError(w, "mark events read", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int64{"marked": n})
}
