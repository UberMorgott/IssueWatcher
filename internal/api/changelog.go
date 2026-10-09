package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/UberMorgott/issuewatcher/internal/provider"
)

// Mod changelogs (Nexus: the mod editor's changelog; one version = all its entries):
//
//	GET    /api/projects/{id}/changelogs → [provider.ChangelogVersion {version, entries[{id, text}]}]
//	PUT    /api/projects/{id}/changelogs/{version} {lines[] | text, dryRun?}
//	       replaces every entry of the version (adds it when absent; equal = nothing sent)
//	DELETE /api/projects/{id}/changelogs/{version}[?dryRun=1] (or body {dryRun})
//	       → provider.ChangelogSave {dryRun, version, action none|add|edit|delete, changed, before, after, request, saved}
//
// Errors as /page, plus 403 {code: agent_caller | caller_unknown} (non-dry-run from an agent run): 404 project, 400 {code: bad_request}, 403 {code: cannot_edit},
// 409 {code: unavailable | not_signed_in | relogin}, 502 {code: save_unsure | platform_error}.

func (s *Server) registerChangelogs(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/projects/{id}/changelogs", s.handleChangelogs)
	mux.HandleFunc("PUT /api/projects/{id}/changelogs/{version}", s.handleChangelogSet)
	mux.HandleFunc("DELETE /api/projects/{id}/changelogs/{version}", s.handleChangelogDelete)
}

// changelogTarget resolves the project and its changelog editor; false = answered.
func (s *Server) changelogTarget(w http.ResponseWriter, r *http.Request) (provider.Project, provider.ChangelogEditor, bool) {
	project, ed, ok := s.pageTarget(w, r)
	if !ok {
		return provider.Project{}, nil, false
	}
	ce, ok := ed.(provider.ChangelogEditor)
	if !ok {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "changelog editing is not available for this platform", "code": "unavailable"})
		return provider.Project{}, nil, false
	}
	return project, ce, true
}

// changelogErr answers a failed read or change.
func (s *Server) changelogErr(w http.ResponseWriter, err error) {
	if errors.Is(err, provider.ErrBadChangelog) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error(), "code": "bad_request"})
		return
	}
	s.pageErr(w, err)
}

func (s *Server) handleChangelogs(w http.ResponseWriter, r *http.Request) {
	project, ce, ok := s.changelogTarget(w, r)
	if !ok {
		return
	}
	out, err := ce.Changelogs(r.Context(), project)
	if err != nil {
		s.changelogErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleChangelogSet(w http.ResponseWriter, r *http.Request) {
	project, ce, ok := s.changelogTarget(w, r)
	if !ok {
		return
	}
	var edit provider.ChangelogEdit
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&edit); err != nil {
		errJSON(w, http.StatusBadRequest, "bad json")
		return
	}
	edit.Version = r.PathValue("version")
	if !edit.DryRun && s.refuseAgent(w, r) {
		return
	}
	// The change outlives a closed tab: once sent it must finish (never re-sent).
	res, err := ce.SetChangelog(context.WithoutCancel(r.Context()), project, edit)
	if err != nil {
		s.changelogErr(w, err)
		return
	}
	if res.Saved {
		s.opts.Log.Info("api: changelog saved", "project", project.ExternalID, "version", res.Version, "action", res.Action)
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleChangelogDelete(w http.ResponseWriter, r *http.Request) {
	project, ce, ok := s.changelogTarget(w, r)
	if !ok {
		return
	}
	var in struct {
		DryRun bool `json:"dryRun"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&in); err != nil && !errors.Is(err, io.EOF) {
		errJSON(w, http.StatusBadRequest, "bad json")
		return
	}
	if q := r.URL.Query().Get("dryRun"); q == "1" || q == "true" {
		in.DryRun = true
	}
	if !in.DryRun && s.refuseAgent(w, r) {
		return
	}
	res, err := ce.DeleteChangelog(context.WithoutCancel(r.Context()), project, r.PathValue("version"), in.DryRun)
	if err != nil {
		s.changelogErr(w, err)
		return
	}
	if res.Saved {
		s.opts.Log.Info("api: changelog deleted", "project", project.ExternalID, "version", res.Version)
	}
	writeJSON(w, http.StatusOK, res)
}
