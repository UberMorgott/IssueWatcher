package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/provider"
	"github.com/UberMorgott/issuewatcher/internal/store"
)

// Data API (all JSON, session or bearer required; docs/ARCHITECTURE.md → HTTP API):
//
//	GET  /api/projects                   projects with open/closed/unread counts, localPath, lastSync
//	GET  /api/items?source=&project=&state=&label=&q=&unread=1&page=&per=
//	GET  /api/items/{id}                 item + body + comments
//	POST /api/items/{id}/read            clear unread (tray badge)
//	POST /api/items/{id}/comments {body} reply on the platform
//	GET  /api/stats?project=&weeks=      totals + weekly opened/closed; per-project totals when project omitted
//	GET  /api/sync   POST /api/sync       poller status / sync now
//
// Legacy aliases: /api/repos, /api/issues[/{id}[/read|/comments]], repo=, per_page=.
func (s *Server) registerData(mux *http.ServeMux) {
	for _, base := range []string{"/api/projects", "/api/repos"} {
		mux.HandleFunc("GET "+base, s.handleRepos)
	}
	for _, base := range []string{"/api/items", "/api/issues"} {
		mux.HandleFunc("GET "+base, s.handleIssues)
		mux.HandleFunc("GET "+base+"/{id}", s.handleIssue)
		mux.HandleFunc("POST "+base+"/{id}/read", s.handleRead)
		mux.HandleFunc("POST "+base+"/{id}/comments", s.handleReply)
	}
	mux.HandleFunc("GET /api/stats", s.handleStats)
	mux.HandleFunc("GET /api/sync", s.handleSyncStatus)
	mux.HandleFunc("POST /api/sync", s.handleSyncNow)
}

func errJSON(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func (s *Server) internalError(w http.ResponseWriter, what string, err error) {
	s.opts.Log.Error("api: "+what, "err", err)
	errJSON(w, http.StatusInternalServerError, what+" failed")
}

func pathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		errJSON(w, http.StatusBadRequest, "bad id")
		return 0, false
	}
	return id, true
}

// queryInt parses an optional non-negative integer query parameter; the first
// of names present wins (contract name first, legacy alias after).
func queryInt(r *http.Request, names ...string) (int64, error) {
	var v, name string
	for _, name = range names {
		if v = r.URL.Query().Get(name); v != "" {
			break
		}
	}
	if v == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		return 0, errors.New("bad " + name)
	}
	return n, nil
}

func (s *Server) handleRepos(w http.ResponseWriter, r *http.Request) {
	repos, err := s.opts.Store.Repos(r.Context())
	if err != nil {
		s.internalError(w, "list repos", err)
		return
	}
	writeJSON(w, http.StatusOK, repos)
}

func (s *Server) handleIssues(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var f store.IssueFilter
	var nums [3]int64
	for i, names := range [][]string{{"project", "repo"}, {"page"}, {"per", "per_page"}} {
		n, err := queryInt(r, names...)
		if err != nil {
			errJSON(w, http.StatusBadRequest, err.Error())
			return
		}
		nums[i] = n
	}
	f.RepoID, f.Page, f.PerPage = nums[0], int(min(nums[1], 1<<20)), int(min(nums[2], 1000))
	switch st := q.Get("state"); st {
	case "", "all":
	case "open", "closed":
		f.State = st
	default:
		errJSON(w, http.StatusBadRequest, "bad state")
		return
	}
	f.Platform, f.Label, f.Text, f.Unread = q.Get("source"), q.Get("label"), q.Get("q"), q.Get("unread") == "1"
	page, err := s.opts.Store.Issues(r.Context(), f)
	if err != nil {
		s.internalError(w, "list issues", err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (s *Server) handleIssue(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	d, err := s.opts.Store.Issue(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		errJSON(w, http.StatusNotFound, "not found")
		return
	}
	if err != nil {
		s.internalError(w, "get issue", err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

func (s *Server) handleRead(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	err := s.opts.Store.MarkRead(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		errJSON(w, http.StatusNotFound, "not found")
		return
	}
	if err != nil {
		s.internalError(w, "mark read", err)
		return
	}
	if s.opts.OnUnreadChange != nil {
		s.opts.OnUnreadChange()
	}
	w.WriteHeader(http.StatusNoContent)
}

// maxCommentBytes bounds a reply (GitHub caps comment bodies at 65536 characters).
const maxCommentBytes = 256 << 10

func (s *Server) handleReply(w http.ResponseWriter, r *http.Request) {
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
	if strings.TrimSpace(req.Body) == "" {
		errJSON(w, http.StatusBadRequest, "empty comment")
		return
	}
	c, err := s.opts.Sync.Reply(r.Context(), id, req.Body)
	var rl *provider.RateLimitError
	switch {
	case err == nil:
		writeJSON(w, http.StatusCreated, c)
	case errors.Is(err, store.ErrNotFound):
		errJSON(w, http.StatusNotFound, "not found")
	case errors.Is(err, provider.ErrNotSignedIn):
		errJSON(w, http.StatusConflict, "not signed in to GitHub")
	case errors.As(err, &rl):
		errJSON(w, http.StatusTooManyRequests, err.Error())
	default:
		s.opts.Log.Error("api: reply", "item", id, "err", err)
		errJSON(w, http.StatusBadGateway, "posting the comment failed: "+err.Error())
	}
}

type statsResponse struct {
	store.Stats
	Projects []store.Repo `json:"projects,omitempty"` // per-project totals (global view only)
	Repos    []store.Repo `json:"repos,omitempty"`    // legacy alias of projects
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	repo, err1 := queryInt(r, "project", "repo")
	weeks, err2 := queryInt(r, "weeks")
	if err := errors.Join(err1, err2); err != nil {
		errJSON(w, http.StatusBadRequest, err.Error())
		return
	}
	if weeks == 0 {
		weeks = 26
	}
	st, err := s.opts.Store.Stats(r.Context(), repo, int(min(weeks, 520)), time.Now())
	if err != nil {
		s.internalError(w, "stats", err)
		return
	}
	resp := statsResponse{Stats: st}
	if repo == 0 {
		if resp.Projects, err = s.opts.Store.Repos(r.Context()); err != nil {
			s.internalError(w, "stats", err)
			return
		}
		resp.Repos = resp.Projects
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleSyncStatus(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.opts.Sync.Status())
}

func (s *Server) handleSyncNow(w http.ResponseWriter, _ *http.Request) {
	s.opts.Sync.Trigger()
	w.WriteHeader(http.StatusAccepted)
}
