package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/UberMorgott/issuewatcher/internal/provider"
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

// SteamComments posts new top-level comments on Workshop items (steam.Provider).
type SteamComments interface {
	PostComment(ctx context.Context, fileID, body string, dryRun bool) (steam.CommentResult, error)
}

// handleSteamComment: POST /api/steam/items/{id}/comments {body, dryRun} posts
// body as a new top-level comment on Workshop item id with the stored web
// session (dryRun: the resolved endpoint and creator only). 201 posted, 200 dry run;
// 403 {code: agent_caller | caller_unknown} (non-dry-run from an agent run),
// 409 {code: not_signed_in | too_long}, 502 {code: outcome_unknown}.
func (s *Server) handleSteamComment(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Body   string `json:"body"`
		DryRun bool   `json:"dryRun"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxCommentBytes)).Decode(&req); err != nil {
		errJSON(w, http.StatusBadRequest, "bad json")
		return
	}
	id := r.PathValue("id")
	if strings.TrimSpace(req.Body) == "" || id == "" || strings.Trim(id, "0123456789") != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "want a Workshop item id and a non-empty body", "code": "bad_request"})
		return
	}
	if !req.DryRun && s.refuseAgent(w, r) {
		return
	}
	res, err := s.opts.SteamComments.PostComment(r.Context(), id, req.Body, req.DryRun)
	var rl *provider.RateLimitError
	switch {
	case err == nil && req.DryRun:
		writeJSON(w, http.StatusOK, res)
	case err == nil:
		s.opts.Log.Info("steam comment", "item", id, "comment", res.URL)
		writeJSON(w, http.StatusCreated, res)
	case errors.Is(err, provider.ErrNotSignedIn):
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error(), "code": "not_signed_in"})
	case errors.Is(err, provider.ErrReplyTooLong):
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error(), "code": "too_long"})
	case errors.As(err, &rl):
		errJSON(w, http.StatusTooManyRequests, err.Error())
	case errors.Is(err, steam.ErrOutcomeUnknown):
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error(), "code": "outcome_unknown"})
	default:
		s.opts.Log.Error("api: steam comment", "item", id, "err", err)
		errJSON(w, http.StatusBadGateway, "posting the comment failed: "+err.Error())
	}
}

func (s *Server) handleSteam(w http.ResponseWriter, _ *http.Request) {
	st, err := s.opts.Steam.Status()
	if err != nil {
		s.internalError(w, "steam settings", err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// steamSettingsCode names the rejected field of a Steam settings save
// (bad_steam_id | bad_api_key | bad_cookies | bad_settings) for the dashboard's text.
func steamSettingsCode(msg string) string {
	switch {
	case strings.Contains(msg, "steamId"):
		return "bad_steam_id"
	case strings.Contains(msg, "apiKey"):
		return "bad_api_key"
	case strings.Contains(msg, "cookie"):
		return "bad_cookies"
	}
	return "bad_settings"
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
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error(), "code": steamSettingsCode(err.Error())})
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
