package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/provider"
	"github.com/UberMorgott/issuewatcher/internal/provider/curseforge"
	"github.com/UberMorgott/issuewatcher/internal/store"
	"github.com/UberMorgott/issuewatcher/internal/syncer"
)

// Data API (all JSON, session or bearer required; docs/ARCHITECTURE.md → HTTP API):
//
//	GET  /api/projects[?sort=&dir=&q=&cursor=&limit=]  all projects with counts; with limit a keyset chunk
//	GET  /api/items?source=&kind=&project=&state=&label=&q=&unread=1&cursor=|after=|ids=&limit=
//	GET  /api/items/{id}                 item + body
//	GET  /api/items/{id}/comments?cursor=&limit=  comments, oldest first, in chunks
//	POST /api/items/{id}/read            clear unread (tray badge)
//	POST /api/items/{id}/comments {body} reply on the platform
//	GET  /api/stats?project=&weeks=      totals + weekly opened/closed; per-project totals when project omitted
//	GET  /api/sync   POST /api/sync       poller status / sync now
//
// Legacy query alias: repo= (= project=).
func (s *Server) registerData(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/projects", s.handleRepos)
	mux.HandleFunc("GET /api/items", s.handleIssues)
	mux.HandleFunc("GET /api/items/{id}", s.handleIssue)
	mux.HandleFunc("GET /api/items/{id}/comments", s.handleComments)
	mux.HandleFunc("POST /api/items/{id}/read", s.handleRead)
	mux.HandleFunc("POST /api/items/{id}/comments", s.handleReply)
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

// handleRepos: without limit the whole list (dashboard filters, badge); with
// limit a keyset chunk {items, nextCursor, more, total} sorted by sort (name,
// open, closed, unread, lastSync) and dir (asc|desc).
func (s *Server) handleRepos(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("limit") == "" {
		repos, err := s.opts.Store.Repos(r.Context())
		if err != nil {
			s.internalError(w, "list repos", err)
			return
		}
		writeJSON(w, http.StatusOK, repos)
		return
	}
	limit, err := queryInt(r, "limit")
	if err != nil {
		errJSON(w, http.StatusBadRequest, err.Error())
		return
	}
	rq := store.RepoQuery{Sort: q.Get("sort"), Desc: q.Get("dir") == "desc", Text: q.Get("q"), Cursor: q.Get("cursor"), Limit: int(min(limit, 1000))}
	if _, ok := store.RepoSorts[rq.Sort]; !ok && rq.Sort != "" {
		errJSON(w, http.StatusBadRequest, "bad sort")
		return
	}
	chunk, err := s.opts.Store.ReposChunk(r.Context(), rq)
	if errors.Is(err, store.ErrBadCursor) {
		errJSON(w, http.StatusBadRequest, "bad cursor")
		return
	}
	if err != nil {
		s.internalError(w, "list repos", err)
		return
	}
	writeJSON(w, http.StatusOK, chunk)
}

// handleIssues: keyset chunks (cursor = next chunk, after = newer than the head,
// ids = re-check loaded rows) with the dashboard filters.
func (s *Server) handleIssues(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var f store.IssueFilter
	repo, err1 := queryInt(r, "project", "repo")
	limit, err2 := queryInt(r, "limit")
	if err := errors.Join(err1, err2); err != nil {
		errJSON(w, http.StatusBadRequest, err.Error())
		return
	}
	f.RepoID, f.Limit = repo, int(min(limit, 1000))
	switch st := q.Get("state"); st {
	case "", "all":
	case "open", "closed":
		f.State = st
	default:
		errJSON(w, http.StatusBadRequest, "bad state")
		return
	}
	switch k := q.Get("kind"); k {
	case "", store.KindIssue, store.KindComment, store.KindBug:
		f.Kind = k
	default:
		errJSON(w, http.StatusBadRequest, "bad kind")
		return
	}
	if v := q.Get("ids"); v != "" {
		for part := range strings.SplitSeq(v, ",") {
			id, err := strconv.ParseInt(part, 10, 64)
			if err != nil || id <= 0 || len(f.IDs) >= store.MaxIDs {
				errJSON(w, http.StatusBadRequest, "bad ids")
				return
			}
			f.IDs = append(f.IDs, id)
		}
	}
	f.Platform, f.Label, f.Text, f.Unread = q.Get("source"), q.Get("label"), q.Get("q"), q.Get("unread") == "1"
	f.Cursor, f.After = q.Get("cursor"), q.Get("after")
	chunk, err := s.opts.Store.Issues(r.Context(), f)
	if errors.Is(err, store.ErrBadCursor) {
		errJSON(w, http.StatusBadRequest, "bad cursor")
		return
	}
	if err != nil {
		s.internalError(w, "list issues", err)
		return
	}
	writeJSON(w, http.StatusOK, chunk)
}

func (s *Server) handleComments(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	limit, err := queryInt(r, "limit")
	if err != nil {
		errJSON(w, http.StatusBadRequest, err.Error())
		return
	}
	chunk, err := s.opts.Store.Comments(r.Context(), id, r.URL.Query().Get("cursor"), int(min(limit, 1000)))
	if errors.Is(err, store.ErrBadCursor) {
		errJSON(w, http.StatusBadRequest, "bad cursor")
		return
	}
	if err != nil {
		s.internalError(w, "list comments", err)
		return
	}
	writeJSON(w, http.StatusOK, chunk)
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
	s.dataChanged("read", id)
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
		s.dataChanged("reply", id)
		writeJSON(w, http.StatusCreated, c)
	case errors.Is(err, store.ErrNotFound):
		errJSON(w, http.StatusNotFound, "not found")
	case errors.Is(err, curseforge.ErrRelogin):
		s.replyConflict(w, r, id, "relogin", "sign in to %s again")
	case errors.Is(err, provider.ErrNotSignedIn):
		s.replyConflict(w, r, id, "not_signed_in", "not signed in to %s")
	case errors.Is(err, syncer.ErrNoSource):
		s.replyConflict(w, r, id, "no_source", "no connected %s account for this item")
	case errors.Is(err, syncer.ErrReplyOff):
		s.replyConflict(w, r, id, "reply_off", "replying is off for %s")
	case errors.As(err, &rl):
		errJSON(w, http.StatusTooManyRequests, err.Error())
	default:
		s.opts.Log.Error("api: reply", "item", id, "err", err)
		errJSON(w, http.StatusBadGateway, "posting the comment failed: "+err.Error())
	}
}

// replyConflict answers 409 with a machine code and the item's platform, so
// the UI names the platform the reply needs (i18n replyErrors.<code>).
func (s *Server) replyConflict(w http.ResponseWriter, r *http.Request, id int64, code, format string) {
	platform := "github"
	if ref, err := s.opts.Store.ItemRef(r.Context(), id); err == nil && ref.Platform != "" {
		platform = ref.Platform
	}
	writeJSON(w, http.StatusConflict, map[string]string{"error": fmt.Sprintf(format, platform), "code": code, "platform": platform})
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
