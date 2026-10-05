package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/UberMorgott/issuewatcher/internal/config"
	"github.com/UberMorgott/issuewatcher/internal/folders"
)

// EventSettingsChanged carries the new SettingsDoc after every saved change.
const EventSettingsChanged = "settings.changed"

// SettingsInfo is read-only context shown next to the settings.
type SettingsInfo struct {
	DataDir    string `json:"dataDir"`
	ConfigFile string `json:"configFile"`
	Version    string `json:"version"`
	Exe        string `json:"exe"`
	// SyncPresets are the fixed plans behind the balanced and fast sync modes.
	SyncPresets map[string]config.ProviderSync `json:"syncPresets"`
	// Palettes are the colour presets (Appearance).
	Palettes []config.Palette `json:"palettes"`
}

// SettingsDoc is GET /api/settings and every successful PATCH.
type SettingsDoc struct {
	Revision int             `json:"revision"`
	Settings config.Settings `json:"settings"`
	Info     SettingsInfo    `json:"info"`
}

// SettingsStore reads and changes the app settings (cmd/issuewatcher applies
// side effects such as the autostart entry before a change is written).
type SettingsStore interface {
	Settings() (SettingsDoc, error)
	PatchSettings(revision int, patch json.RawMessage) (SettingsDoc, error)
	// PreviewSettings validates patch like PatchSettings without writing it (dry run).
	PreviewSettings(revision int, patch json.RawMessage) (SettingsDoc, error)
	ResetSettings(revision int, section string) (SettingsDoc, error)
}

func (s *Server) registerSettings(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/settings", s.handleSettings)
	mux.HandleFunc("PATCH /api/settings", s.handleSettingsPatch)
	mux.HandleFunc("POST /api/settings/reset", s.handleSettingsReset)
	mux.HandleFunc("GET /api/settings/export", s.handleSettingsExport)
}

func (s *Server) handleSettings(w http.ResponseWriter, _ *http.Request) {
	doc, err := s.opts.Settings.Settings()
	if err != nil {
		s.internalError(w, "read settings", err)
		return
	}
	writeJSON(w, http.StatusOK, doc)
}

// handleSettingsPatch: {revision, patch} where patch is a JSON merge patch of
// the settings object. 409 = someone saved first (body: the current doc).
func (s *Server) handleSettingsPatch(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Revision *int            `json:"revision"`
		Patch    json.RawMessage `json:"patch"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 512<<10)).Decode(&req); err != nil || req.Revision == nil || len(req.Patch) == 0 {
		errJSON(w, http.StatusBadRequest, "body must be {revision, patch}")
		return
	}
	doc, err := s.opts.Settings.PatchSettings(*req.Revision, req.Patch)
	s.settingsResult(w, doc, err)
}

func (s *Server) handleSettingsReset(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Revision *int   `json:"revision"`
		Section  string `json:"section"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil || req.Revision == nil || req.Section == "" {
		errJSON(w, http.StatusBadRequest, "body must be {revision, section}")
		return
	}
	doc, err := s.opts.Settings.ResetSettings(*req.Revision, req.Section)
	s.settingsResult(w, doc, err)
}

func (s *Server) settingsResult(w http.ResponseWriter, doc SettingsDoc, err error) {
	var ve *config.ValidationError
	switch {
	case err == nil:
		folders.Invalidate() // roots or project settings may change what a folder means
		s.redetectAgents()   // agent profiles may name other CLIs
		s.Publish(EventSettingsChanged, doc)
		writeJSON(w, http.StatusOK, doc)
	case errors.Is(err, config.ErrConflict):
		cur, rerr := s.opts.Settings.Settings()
		if rerr != nil {
			s.internalError(w, "read settings", rerr)
			return
		}
		writeJSON(w, http.StatusConflict, map[string]any{"error": "conflict", "current": cur})
	case errors.As(err, &ve):
		writeJSON(w, http.StatusBadRequest, ve)
	default:
		s.internalError(w, "save settings", err)
	}
}

func (s *Server) handleTestNotification(w http.ResponseWriter, _ *http.Request) {
	s.opts.TestNotification()
	w.WriteHeader(http.StatusNoContent)
}

// handleSettingsExport downloads the settings (no secrets live there).
func (s *Server) handleSettingsExport(w http.ResponseWriter, _ *http.Request) {
	doc, err := s.opts.Settings.Settings()
	if err != nil {
		s.internalError(w, "read settings", err)
		return
	}
	w.Header().Set("Content-Disposition", `attachment; filename="issuewatcher-settings.json"`)
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(doc.Settings)
}
