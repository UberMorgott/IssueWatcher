package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/UberMorgott/issuewatcher/internal/provider/steam"
)

// SteamSettings is the Steam provider's account settings (steam.Provider).
type SteamSettings interface {
	Status() (steam.Status, error)
	Save(u steam.Update) (steam.Status, error)
}

// Steam account settings (docs/ARCHITECTURE.md → Mod platforms, Settings ›
// Платформы). Secrets are write-only: GET reports only whether they are set.
//
//	GET /api/providers/steam  {configured, steamId, appId, hasApiKey, hasCookies, session}
//	PUT /api/providers/steam  {steamId?, appId?, apiKey?, steamLoginSecure?, sessionid?} (absent = unchanged, "" = clear)
func (s *Server) registerSteam(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/providers/steam", s.handleSteam)
	mux.HandleFunc("PUT /api/providers/steam", s.handleSteamSave)
}

func (s *Server) handleSteam(w http.ResponseWriter, _ *http.Request) {
	st, err := s.opts.Steam.Status()
	if err != nil {
		s.internalError(w, "steam settings", err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) handleSteamSave(w http.ResponseWriter, r *http.Request) {
	var u steam.Update
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&u); err != nil {
		errJSON(w, http.StatusBadRequest, "bad json")
		return
	}
	st, err := s.opts.Steam.Save(u)
	switch {
	case errors.Is(err, steam.ErrBadSettings):
		errJSON(w, http.StatusBadRequest, err.Error())
		return
	case err != nil:
		s.internalError(w, "steam settings", err)
		return
	}
	if s.opts.Sync != nil {
		s.opts.Sync.Trigger() // read the (new) account's items now
	}
	writeJSON(w, http.StatusOK, st)
}
