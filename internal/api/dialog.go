package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// Native folder dialog (FolderDialog «Обзор…»):
//
//	GET  /api/dialog/folder {available}          whether this instance can show it (not headless)
//	POST /api/dialog/folder {projectId?, title?, initial?}
//	     → 200 {path, cancelled}; 409 {error, code: unavailable|busy}
//
// The chosen folder is not saved here: the UI maps it via PUT /api/projects/{id}/path,
// which validates it (folders.Check).

// FolderPicker shows the desktop folder dialog (internal/picker) and blocks
// until the user picks a folder (ok) or cancels.
type FolderPicker interface {
	PickFolder(ctx context.Context, title, initial string) (path string, ok bool, err error)
}

const maxDialogTitle = 200

func (s *Server) registerDialog(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/dialog/folder", s.handleDialogInfo)
	mux.HandleFunc("POST /api/dialog/folder", s.handlePickFolder)
}

func (s *Server) handleDialogInfo(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]bool{"available": s.opts.Picker != nil})
}

func (s *Server) handlePickFolder(w http.ResponseWriter, r *http.Request) {
	if s.opts.Picker == nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "folder dialog unavailable", "code": "unavailable"})
		return
	}
	var req struct {
		ProjectID int64  `json:"projectId"`
		Title     string `json:"title"`
		Initial   string `json:"initial"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		errJSON(w, http.StatusBadRequest, "bad json")
		return
	}
	if !s.pickMu.TryLock() {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "a folder dialog is already open", "code": "busy"})
		return
	}
	defer s.pickMu.Unlock()
	title := strings.TrimSpace(req.Title)
	if len([]rune(title)) > maxDialogTitle {
		title = string([]rune(title)[:maxDialogTitle])
	}
	path, ok, err := s.opts.Picker.PickFolder(r.Context(), title, s.pickerStart(r.Context(), req.ProjectID, req.Initial))
	if err != nil {
		s.internalError(w, "folder dialog", err)
		return
	}
	if !ok {
		path = ""
	}
	writeJSON(w, http.StatusOK, map[string]any{"path": path, "cancelled": !ok})
}

// pickerStart is the folder the dialog opens in: the typed path, else the
// project's mapped folder (or its parent when it moved), else the parent of
// another mapped clone, else the first clone-discovery root; "" = shell default.
func (s *Server) pickerStart(ctx context.Context, id int64, hint string) string {
	if d := existingDir(hint); d != "" {
		return d
	}
	if s.opts.Store != nil {
		if repos, err := s.opts.Store.Repos(ctx); err == nil {
			for _, rp := range repos {
				if rp.ID == id && rp.LocalPath != "" {
					if d := existingDir(rp.LocalPath); d != "" {
						return d
					}
					if d := existingDir(filepath.Dir(rp.LocalPath)); d != "" {
						return d
					}
				}
			}
			for _, rp := range repos {
				if rp.ID != id && rp.LocalPath != "" {
					if d := existingDir(filepath.Dir(rp.LocalPath)); d != "" {
						return d
					}
				}
			}
		}
	}
	if s.opts.Settings != nil {
		if doc, err := s.opts.Settings.Settings(); err == nil {
			for _, root := range doc.Settings.Projects.Roots {
				if d := existingDir(root); d != "" {
					return d
				}
			}
		}
	}
	return ""
}

// existingDir returns p cleaned when it is an absolute path of an existing folder, else "".
func existingDir(p string) string {
	p = strings.TrimSpace(p)
	if p == "" || !filepath.IsAbs(p) {
		return ""
	}
	p = filepath.Clean(p)
	if st, err := os.Stat(p); err == nil && st.IsDir() {
		return p
	}
	return ""
}
