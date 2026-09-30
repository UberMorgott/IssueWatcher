package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/UberMorgott/issuewatcher/internal/store"
)

// Project links (docs/ARCHITECTURE.md → Mod platforms › Linking): mod pages →
// the code project (GitHub repo) whose folder their fixes run in.
//
//	GET    /api/projects/{id}/links            {linkedTo, links}: a mod page's code project, a code project's mod pages
//	PUT    /api/projects/{id}/links {mods:[]}  code project id: its mod pages become exactly mods (a mod page linked elsewhere moves)
//	DELETE /api/projects/{id}/links            removes every link of the project
func (s *Server) registerLinks(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/projects/{id}/links", s.handleLinks)
	mux.HandleFunc("PUT /api/projects/{id}/links", s.handleSetLinks)
	mux.HandleFunc("DELETE /api/projects/{id}/links", s.handleUnlink)
}

// maxLinks bounds one PUT.
const maxLinks = 500

func (s *Server) handleLinks(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	l, err := s.opts.Store.Links(r.Context(), id)
	if s.linkError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, l)
}

func (s *Server) handleSetLinks(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var req struct {
		Mods []int64 `json:"mods"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		errJSON(w, http.StatusBadRequest, "bad json")
		return
	}
	if len(req.Mods) > maxLinks {
		errJSON(w, http.StatusBadRequest, "too many mod pages")
		return
	}
	if s.linkError(w, s.opts.Store.SetProjectLinks(r.Context(), id, req.Mods)) {
		return
	}
	s.dataChanged("links", 0)
	s.handleLinks(w, r)
}

func (s *Server) handleUnlink(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if s.linkError(w, s.opts.Store.UnlinkProject(r.Context(), id)) {
		return
	}
	s.dataChanged("links", 0)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) linkError(w http.ResponseWriter, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, store.ErrNotFound):
		errJSON(w, http.StatusNotFound, "not found")
	case errors.Is(err, store.ErrBadLink):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error(), "code": "bad_link"})
	default:
		s.internalError(w, "project links", err)
	}
	return true
}
