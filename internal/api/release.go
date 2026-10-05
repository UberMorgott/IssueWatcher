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
//	PUT  /api/projects/{id}/publish-profile          {revision?, publishProfile?, autopilot?, verify?, dryRun?} → ProfileDoc   (agent callers refused)
//	                                                 dryRun → ProfilePreview (validated, nothing written; agent callers allowed)
//	POST /api/projects/{id}/publish-profile/check    release.Request? → release.CheckResult (plan + trial build at HEAD in a temp
//	                                                 worktree, archive check, per-target publisher dry run; nothing committed or sent)
//	GET  /api/projects/{id}/autopilot                → AutopilotDoc (the project's autopilot block + the global one)
//	PUT  /api/projects/{id}/autopilot                {revision?, autopilot, dryRun?} → AutopilotDoc   (agent callers refused
//	                                                 unless dryRun: validated, nothing written)
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
//	POST /api/autopilot/events/read {ids?, all?}     → {marked, unread, attention}
//
// SSE: autopilot.run (release.RunView) on every run / step change,
// autopilot.event (store.AutopilotEvent) on every activity log entry,
// autopilot.unread (Unread) after every new event and every mark-read.

// SSE events of the autopilot.
const (
	EventAutopilotRun   = "autopilot.run"
	EventAutopilotEvent = "autopilot.event"
	// EventAutopilotUnread carries Unread after every new event and every mark-read.
	EventAutopilotUnread = "autopilot.unread"
)

// codeAgentCaller is the 403 code for a refused agent run caller.
const codeAgentCaller = "agent_caller"

func (s *Server) registerRelease(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/projects/{id}/publish-profile", s.handleProfileGet)
	mux.HandleFunc("PUT /api/projects/{id}/publish-profile", s.handleProfilePut)
	mux.HandleFunc("POST /api/projects/{id}/publish-profile/check", s.handleCheck)
	mux.HandleFunc("GET /api/projects/{id}/autopilot", s.handleAutopilotGet)
	mux.HandleFunc("PUT /api/projects/{id}/autopilot", s.handleAutopilotPut)
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

// AutopilotEvent publishes autopilot.event (release.Deps.OnEvent) and the new unread counts.
func (s *Server) AutopilotEvent(ev store.AutopilotEvent) {
	s.Publish(EventAutopilotEvent, ev)
	s.publishUnread()
}

// Unread is the activity log's unread counter (top-bar badge).
type Unread struct {
	Unread    int `json:"unread"`
	Attention int `json:"attention"`
}

// publishUnread sends autopilot.unread with the current counts.
func (s *Server) publishUnread() {
	u, a, err := s.opts.Store.UnreadEventCount(s.bg)
	if err != nil {
		s.opts.Log.Warn("api: unread events", "err", err)
		return
	}
	s.Publish(EventAutopilotUnread, Unread{Unread: u, Attention: a})
}

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
	ProjectID      int64                 `json:"projectId"`
	Project        string                `json:"project"`
	Revision       int                   `json:"revision"`
	PublishProfile config.PublishProfile `json:"publishProfile"`
	// Verify is the project's verify command (agents.projects[key].verify): the
	// release / fix gate when the folder has no Aegis; {name} / {version} expand.
	Verify    string                  `json:"verify"`
	Autopilot config.ProjectAutopilot `json:"autopilot"`
	Global    config.AgentsAutopilot  `json:"global"`
	Kinds     ProfileKinds            `json:"kinds"`
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
	plan, err := s.opts.Release.Plan(r.Context(), rp.ID, release.Request{Origin: s.origin(r)})
	if err != nil {
		return ProfileDoc{}, err
	}
	return ProfileDoc{ProjectID: rp.ID, Project: rp.Key, Revision: doc.Revision, PublishProfile: pa.PublishProfile, Verify: pa.Verify,
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

// ProfilePreview is the dry run of PUT …/publish-profile: the blocks as they
// would be saved (validated, nothing written).
type ProfilePreview struct {
	DryRun         bool                    `json:"dryRun"`
	OK             bool                    `json:"ok"`
	ProjectID      int64                   `json:"projectId"`
	Project        string                  `json:"project"`
	Revision       int                     `json:"revision"` // the revision it was validated against
	PublishProfile config.PublishProfile   `json:"publishProfile"`
	Verify         string                  `json:"verify"`
	Autopilot      config.ProjectAutopilot `json:"autopilot"`
}

// handleProfilePut replaces the project's publishProfile and/or autopilot
// blocks (a missing one stays); validation errors → 400, a stale revision → 409.
// dryRun validates the change and answers ProfilePreview without writing it
// (allowed for agent callers: nothing changes).
func (s *Server) handleProfilePut(w http.ResponseWriter, r *http.Request) {
	rp, ok := s.codeProject(w, r)
	if !ok {
		return
	}
	var req struct {
		Revision       *int                     `json:"revision"`
		PublishProfile *config.PublishProfile   `json:"publishProfile"`
		Autopilot      *config.ProjectAutopilot `json:"autopilot"`
		Verify         *string                  `json:"verify"`
		DryRun         bool                     `json:"dryRun"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<10)).Decode(&req); err != nil ||
		(req.PublishProfile == nil && req.Autopilot == nil && req.Verify == nil) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "body must be {revision?, publishProfile?, autopilot?, verify?, dryRun?}", "code": "bad_request"})
		return
	}
	if !req.DryRun && s.refuseAgent(w, r) {
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
	if req.Verify != nil {
		block["verify"] = strings.TrimSpace(*req.Verify)
	}
	patch, _ := json.Marshal(map[string]any{"agents": map[string]any{"projects": map[string]any{rp.Key: block}}})
	if req.DryRun {
		doc, err := s.opts.Settings.PreviewSettings(rev, patch)
		if err != nil {
			s.settingsResult(w, doc, err)
			return
		}
		ag := doc.Settings.Agents
		writeJSON(w, http.StatusOK, ProfilePreview{DryRun: true, OK: true, ProjectID: rp.ID, Project: rp.Key, Revision: rev,
			PublishProfile: ag.Projects[rp.Key].PublishProfile, Verify: ag.Projects[rp.Key].Verify, Autopilot: ag.AutopilotFor(rp.Key)})
		return
	}
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

// AutopilotDoc is GET/PUT /api/projects/{id}/autopilot (MCP get/set_autopilot_settings).
type AutopilotDoc struct {
	DryRun    bool                    `json:"dryRun,omitempty"`
	ProjectID int64                   `json:"projectId"`
	Project   string                  `json:"project"`
	Revision  int                     `json:"revision"`
	Autopilot config.ProjectAutopilot `json:"autopilot"`
	Global    config.AgentsAutopilot  `json:"global"`
}

func (s *Server) handleAutopilotGet(w http.ResponseWriter, r *http.Request) {
	rp, ok := s.codeProject(w, r)
	if !ok {
		return
	}
	doc, err := s.opts.Settings.Settings()
	if err != nil {
		s.internalError(w, "read settings", err)
		return
	}
	ag := doc.Settings.Agents
	writeJSON(w, http.StatusOK, AutopilotDoc{ProjectID: rp.ID, Project: rp.Key, Revision: doc.Revision,
		Autopilot: ag.AutopilotFor(rp.Key), Global: ag.Autopilot})
}

// handleAutopilotPut replaces the project's autopilot block (it can switch the
// project on, so agent callers are refused unless dryRun).
func (s *Server) handleAutopilotPut(w http.ResponseWriter, r *http.Request) {
	rp, ok := s.codeProject(w, r)
	if !ok {
		return
	}
	var req struct {
		Revision  *int                     `json:"revision"`
		Autopilot *config.ProjectAutopilot `json:"autopilot"`
		DryRun    bool                     `json:"dryRun"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil || req.Autopilot == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "body must be {revision?, autopilot, dryRun?}", "code": "bad_request"})
		return
	}
	if !req.DryRun && s.refuseAgent(w, r) {
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
	block := mergeDiff(toJSONValue(cur.Settings.Agents.AutopilotFor(rp.Key)), toJSONValue(*req.Autopilot))
	patch, _ := json.Marshal(map[string]any{"agents": map[string]any{"projects": map[string]any{rp.Key: map[string]any{"autopilot": block}}}})
	var doc SettingsDoc
	if req.DryRun {
		doc, err = s.opts.Settings.PreviewSettings(rev, patch)
	} else {
		doc, err = s.opts.Settings.PatchSettings(rev, patch)
	}
	if err != nil {
		s.settingsResult(w, doc, err)
		return
	}
	if !req.DryRun {
		s.Publish(EventSettingsChanged, doc)
	}
	ag := doc.Settings.Agents
	writeJSON(w, http.StatusOK, AutopilotDoc{DryRun: req.DryRun, ProjectID: rp.ID, Project: rp.Key, Revision: doc.Revision,
		Autopilot: ag.AutopilotFor(rp.Key), Global: ag.Autopilot})
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
	req.Origin = s.origin(r)
	p, err := s.opts.Release.Plan(r.Context(), rp.ID, req)
	if err != nil {
		s.internalError(w, "release plan", err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// handleCheck is «Проверить настройку»: the full dry run (slow: it builds).
func (s *Server) handleCheck(w http.ResponseWriter, r *http.Request) {
	rp, ok := s.codeProject(w, r)
	if !ok {
		return
	}
	var req release.Request
	if !decodeRequest(w, r, &req) {
		return
	}
	req.Origin = s.origin(r)
	res, err := s.opts.Release.Check(r.Context(), rp.ID, req)
	if err != nil {
		s.internalError(w, "publish profile check", err)
		return
	}
	writeJSON(w, http.StatusOK, res)
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
	u, a, err := s.opts.Store.UnreadEventCount(r.Context())
	if err != nil {
		s.internalError(w, "mark events read", err)
		return
	}
	if n > 0 {
		s.Publish(EventAutopilotUnread, Unread{Unread: u, Attention: a})
	}
	writeJSON(w, http.StatusOK, map[string]any{"marked": n, "unread": u, "attention": a})
}
