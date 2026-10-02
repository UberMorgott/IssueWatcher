package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/UberMorgott/issuewatcher/internal/provider"
	"github.com/UberMorgott/issuewatcher/internal/provider/nexus"
	"github.com/UberMorgott/issuewatcher/internal/store"
)

// Mod page edit (Phase 7; Nexus: the mod editor's General tab, ModPageDialog):
//
//	GET /api/projects/{id}/page → provider.ModPage {name, summary, description (BBCode), version, category, author, tags, url, maxSummary}
//	PUT /api/projects/{id}/page provider.ModPageEdit {name?, summary?, description?, version?, dryRun?}
//	    → provider.ModPageSave {dryRun, changed, request{method, url, body}, saved}
//
// Errors: 404 project, 400 {code: bad_request}, 403 {code: cannot_edit},
// 409 {code: unavailable | not_signed_in | relogin}, 502 {code: save_unsure}
// (sent without a clear answer: check the page before saving again) | {code: platform_error}.

func (s *Server) registerModPage(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/projects/{id}/page", s.handleModPage)
	mux.HandleFunc("PUT /api/projects/{id}/page", s.handleModPageSave)
}

// pageTarget resolves the project and its page editor; false = answered.
func (s *Server) pageTarget(w http.ResponseWriter, r *http.Request) (provider.Project, provider.PageEditor, bool) {
	id, ok := pathID(w, r)
	if !ok {
		return provider.Project{}, nil, false
	}
	rp, err := s.opts.Store.Repo(r.Context(), id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		errJSON(w, http.StatusNotFound, "no such project")
		return provider.Project{}, nil, false
	case err != nil:
		s.internalError(w, "project", err)
		return provider.Project{}, nil, false
	}
	ed := s.opts.PageEditors(rp.Platform)
	if ed == nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "mod page editing is not available for " + rp.Platform, "code": "unavailable"})
		return provider.Project{}, nil, false
	}
	return provider.Project{ExternalID: strings.TrimPrefix(rp.Key, rp.Platform+":"), Name: rp.Name, URL: rp.URL}, ed, true
}

// pageErr answers a failed read or save.
func (s *Server) pageErr(w http.ResponseWriter, err error) {
	code, status := "platform_error", http.StatusBadGateway
	switch {
	case errors.Is(err, provider.ErrBadPageEdit):
		code, status = "bad_request", http.StatusBadRequest
	case errors.Is(err, provider.ErrCannotEdit):
		code, status = "cannot_edit", http.StatusForbidden
	case errors.Is(err, provider.ErrRelogin):
		code, status = CodeRelogin, http.StatusConflict
	case errors.Is(err, provider.ErrNotSignedIn):
		code, status = CodeNotSignedIn, http.StatusConflict
	case errors.Is(err, nexus.ErrWriteUnsure):
		code = "save_unsure"
	}
	if status != http.StatusBadRequest {
		s.opts.Log.Warn("api: mod page", "code", code, "err", err)
	}
	writeJSON(w, status, map[string]string{"error": err.Error(), "code": code})
}

func (s *Server) handleModPage(w http.ResponseWriter, r *http.Request) {
	project, ed, ok := s.pageTarget(w, r)
	if !ok {
		return
	}
	pg, err := ed.ModPage(r.Context(), project)
	if err != nil {
		s.pageErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, pg)
}

func (s *Server) handleModPageSave(w http.ResponseWriter, r *http.Request) {
	project, ed, ok := s.pageTarget(w, r)
	if !ok {
		return
	}
	var edit provider.ModPageEdit
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&edit); err != nil {
		errJSON(w, http.StatusBadRequest, "bad json")
		return
	}
	// The save outlives a closed tab: once sent it must finish (never re-sent).
	res, err := ed.SaveModPage(context.WithoutCancel(r.Context()), project, edit)
	if err != nil {
		s.pageErr(w, err)
		return
	}
	if res.Saved {
		s.opts.Log.Info("api: mod page saved", "project", project.ExternalID, "changed", res.Changed)
	}
	writeJSON(w, http.StatusOK, res)
}
