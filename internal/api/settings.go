package api

import (
	"encoding/json"
	"net/http"
)

// Settings is the GET/PUT /api/settings payload: app settings that live outside
// the browser (config.json, the autostart Run entry).
type Settings struct {
	StartWithWindows    bool `json:"startWithWindows"` // Run entry exists and starts this exe
	StartMinimized      bool `json:"startMinimized"`
	PollIntervalMinutes int  `json:"pollIntervalMinutes"`
}

// SettingsPatch is a PUT body; nil fields stay unchanged.
type SettingsPatch struct {
	StartWithWindows *bool `json:"startWithWindows"`
	StartMinimized   *bool `json:"startMinimized"`
}

// SettingsStore reads and writes the app settings (cmd/issuewatcher).
type SettingsStore interface {
	Settings() (Settings, error)
	UpdateSettings(SettingsPatch) (Settings, error)
}

func (s *Server) registerSettings(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/settings", s.handleSettings)
	mux.HandleFunc("PUT /api/settings", s.handleSettingsPut)
}

func (s *Server) handleSettings(w http.ResponseWriter, _ *http.Request) {
	st, err := s.opts.Settings.Settings()
	if err != nil {
		s.internalError(w, "read settings", err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) handleSettingsPut(w http.ResponseWriter, r *http.Request) {
	var p SettingsPatch
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&p); err != nil {
		errJSON(w, http.StatusBadRequest, "bad json")
		return
	}
	st, err := s.opts.Settings.UpdateSettings(p)
	if err != nil {
		s.internalError(w, "save settings", err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}
