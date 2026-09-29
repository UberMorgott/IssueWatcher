package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/folders"
	"github.com/UberMorgott/issuewatcher/internal/store"
)

// Projects & folders (Settings):
//
//	GET  /api/folders                   every project with its mapped folder and its status
//	PUT  /api/projects/{id}/path {path} map a folder ("" = unmap); a folder that is not a clone of
//	                                    the project → 422 {error, code: missing|notGit|mismatch}, nothing saved
//	POST /api/folders/discover          scan settings.projects.roots for matching clones (suggestions only)

// FolderRow is one project in GET /api/folders.
type FolderRow struct {
	ProjectID int64          `json:"projectId"`
	Name      string         `json:"name"`
	URL       string         `json:"url"`
	Platform  string         `json:"platform"`
	Key       string         `json:"key"` // settings key platform:external_id
	LocalPath string         `json:"localPath"`
	Status    folders.Status `json:"status"`
}

func (s *Server) registerFolders(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/folders", s.handleFolders)
	mux.HandleFunc("PUT /api/projects/{id}/path", s.handleSetPath)
	mux.HandleFunc("POST /api/folders/discover", s.handleDiscover)
}

func (s *Server) handleFolders(w http.ResponseWriter, r *http.Request) {
	repos, err := s.opts.Store.Repos(r.Context())
	if err != nil {
		s.internalError(w, "list folders", err)
		return
	}
	out := make([]FolderRow, 0, len(repos))
	for _, p := range repos {
		out = append(out, FolderRow{
			ProjectID: p.ID, Name: p.Name, URL: p.URL, Platform: p.Platform, Key: p.Key, LocalPath: p.LocalPath,
			Status: folders.Check(p.LocalPath, p.URL),
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleSetPath(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var req struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil {
		errJSON(w, http.StatusBadRequest, "bad json")
		return
	}
	p := strings.TrimSpace(req.Path)
	if p != "" {
		if !filepath.IsAbs(p) {
			errJSON(w, http.StatusBadRequest, "path must be absolute")
			return
		}
		p = filepath.Clean(p)
	}
	repos, err := s.opts.Store.Repos(r.Context())
	if err != nil {
		s.internalError(w, "list folders", err)
		return
	}
	i := slices.IndexFunc(repos, func(rp store.Repo) bool { return rp.ID == id })
	if i < 0 {
		errJSON(w, http.StatusNotFound, "not found")
		return
	}
	rp := repos[i]
	// Only a git clone of this very project is mapped (a fix job runs there).
	if st := folders.Check(p, rp.URL); p != "" && st != folders.StatusOK {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "folder not usable for " + rp.Name + ": " + string(st), "code": string(st)})
		return
	}
	err = s.opts.Store.SetLocalPath(r.Context(), id, p)
	if errors.Is(err, store.ErrNotFound) {
		errJSON(w, http.StatusNotFound, "not found")
		return
	}
	if err != nil {
		s.internalError(w, "set folder", err)
		return
	}
	s.Publish(EventDataChanged, DataChange{Reason: "folder"})
	writeJSON(w, http.StatusOK, FolderRow{
		ProjectID: rp.ID, Name: rp.Name, URL: rp.URL, Platform: rp.Platform, Key: rp.Key, LocalPath: p,
		Status: folders.Check(p, rp.URL),
	})
}

// handleDiscover suggests clones for the projects without a working mapping.
func (s *Server) handleDiscover(w http.ResponseWriter, r *http.Request) {
	doc, err := s.opts.Settings.Settings()
	if err != nil {
		s.internalError(w, "read settings", err)
		return
	}
	cfg := doc.Settings.Projects
	repos, err := s.opts.Store.Repos(r.Context())
	if err != nil {
		s.internalError(w, "list folders", err)
		return
	}
	var todo []folders.Project
	for _, p := range repos {
		if folders.Check(p.LocalPath, p.URL) != folders.StatusOK {
			todo = append(todo, folders.Project{ID: p.ID, Name: p.Name, URL: p.URL})
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	sugg, visited, err := folders.Discover(ctx, cfg.Roots, cfg.ScanDepth, cfg.Exclude, todo)
	if err != nil {
		errJSON(w, http.StatusGatewayTimeout, "scan stopped: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"suggestions": sugg, "visited": visited, "roots": cfg.Roots})
}
