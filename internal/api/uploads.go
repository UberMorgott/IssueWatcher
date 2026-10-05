package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/UberMorgott/issuewatcher/internal/provider"
	"github.com/UberMorgott/issuewatcher/internal/provider/curseforge"
	"github.com/UberMorgott/issuewatcher/internal/provider/steamcmd"
	"github.com/UberMorgott/issuewatcher/internal/tools"
)

// CurseForgeUpload is the CurseForge upload token store (curseforge.Uploader).
type CurseForgeUpload interface {
	Status() (curseforge.TokenStatus, error)
	Save(ctx context.Context, token string) (curseforge.TokenStatus, error)
	Check(ctx context.Context) (curseforge.TokenStatus, error)
}

// SteamUpload is the steamcmd sign-in for Workshop uploads (steamcmd.Workshop).
type SteamUpload interface {
	Status() (steamcmd.Status, error)
	StartLogin(user, password, code string) (steamcmd.LoginState, error)
	SubmitCode(code string) (steamcmd.LoginState, error)
	CancelLogin()
	CheckSession(ctx context.Context) (steamcmd.Status, error)
	Install(ctx context.Context) (steamcmd.Status, error)
	Forget() (steamcmd.Status, error)
}

// Upload credentials (Phase 5 publishing; Settings › Платформы). Secrets are
// write-only: no response carries them. Changing them is refused for agent
// runs (refuseAgent).
//
//	GET    /api/providers/curseforge/upload        {hasToken, checkedAt?}
//	PUT    /api/providers/curseforge/upload        {token} ("" = remove) → the same; 400 {code: bad_token}, 502 {code: check_failed}
//	POST   /api/providers/curseforge/upload/check  → the same; 409 {code: no_token}
//	GET    /api/providers/steam/upload             {steamcmd?, source?, user?, loggedIn, expired?, loggedInAt?, checkedAt?, login{state, error?}}
//	POST   /api/providers/steam/upload/login       {user, password, code?} → login state (poll GET); 400 {code: bad_login}, 409 {code: busy|no_steamcmd}
//	POST   /api/providers/steam/upload/code        {code} → login state
//	POST   /api/providers/steam/upload/cancel      → 204
//	POST   /api/providers/steam/upload/check       cached sign-in → status; 409 {code: relogin|no_login|no_steamcmd|busy}
//	POST   /api/providers/steam/upload/install     sets steamcmd up in data\tools\steamcmd (Valve's steamcmd.zip) → status
//	DELETE /api/providers/steam/upload             forget the account → status
func (s *Server) registerUploads(mux *http.ServeMux) {
	if s.opts.CurseForgeUpload != nil {
		mux.HandleFunc("GET /api/providers/curseforge/upload", s.handleCFUpload)
		mux.HandleFunc("PUT /api/providers/curseforge/upload", s.handleCFUploadSave)
		mux.HandleFunc("POST /api/providers/curseforge/upload/check", s.handleCFUploadCheck)
	}
	if s.opts.SteamUpload != nil {
		mux.HandleFunc("GET /api/providers/steam/upload", s.handleSteamUpload)
		mux.HandleFunc("POST /api/providers/steam/upload/login", s.handleSteamUploadLogin)
		mux.HandleFunc("POST /api/providers/steam/upload/code", s.handleSteamUploadCode)
		mux.HandleFunc("POST /api/providers/steam/upload/cancel", s.handleSteamUploadCancel)
		mux.HandleFunc("POST /api/providers/steam/upload/check", s.handleSteamUploadCheck)
		mux.HandleFunc("POST /api/providers/steam/upload/install", s.handleSteamUploadInstall)
		mux.HandleFunc("DELETE /api/providers/steam/upload", s.handleSteamUploadForget)
	}
}

func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(v); err != nil {
		errJSON(w, http.StatusBadRequest, "bad json")
		return false
	}
	return true
}

func (s *Server) handleCFUpload(w http.ResponseWriter, _ *http.Request) {
	st, err := s.opts.CurseForgeUpload.Status()
	if err != nil {
		s.internalError(w, "curseforge upload token", err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) handleCFUploadSave(w http.ResponseWriter, r *http.Request) {
	if s.refuseAgent(w, r) {
		return
	}
	var req struct {
		Token *string `json:"token"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if req.Token == nil {
		errJSON(w, http.StatusBadRequest, "token missing")
		return
	}
	st, err := s.opts.CurseForgeUpload.Save(r.Context(), *req.Token)
	s.cfUploadResult(w, st, err)
}

func (s *Server) handleCFUploadCheck(w http.ResponseWriter, r *http.Request) {
	st, err := s.opts.CurseForgeUpload.Check(r.Context())
	s.cfUploadResult(w, st, err)
}

// cfUploadResult answers a save or check. Errors never carry the token.
func (s *Server) cfUploadResult(w http.ResponseWriter, st curseforge.TokenStatus, err error) {
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, st)
	case errors.Is(err, curseforge.ErrBadToken):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error(), "code": "bad_token"})
	case errors.Is(err, curseforge.ErrNoToken):
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error(), "code": "no_token"})
	case errors.Is(err, context.Canceled):
		errJSON(w, http.StatusServiceUnavailable, "cancelled")
	default:
		s.opts.Log.Warn("api: curseforge token check", "err", err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error(), "code": "check_failed"})
	}
}

func (s *Server) handleSteamUpload(w http.ResponseWriter, _ *http.Request) {
	st, err := s.opts.SteamUpload.Status()
	if err != nil {
		s.internalError(w, "steam upload status", err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// steamUploadError answers a failed steamcmd call. Messages never carry the
// password or the code (the publisher scrubs them).
func (s *Server) steamUploadError(w http.ResponseWriter, err error) {
	code, status := "error", http.StatusBadGateway
	switch {
	case errors.Is(err, steamcmd.ErrBadLogin), errors.Is(err, provider.ErrBadPublish):
		code, status = "bad_login", http.StatusBadRequest
	case errors.Is(err, steamcmd.ErrBusy), errors.Is(err, tools.ErrBusy):
		code, status = "busy", http.StatusConflict
	case errors.Is(err, steamcmd.ErrNoSteamCMD):
		code, status = "no_steamcmd", http.StatusConflict
	case errors.Is(err, steamcmd.ErrNoLogin):
		code, status = "no_login", http.StatusConflict
	case errors.Is(err, steamcmd.ErrRelogin):
		code, status = "relogin", http.StatusConflict
	case errors.Is(err, context.Canceled):
		code, status = "cancelled", http.StatusServiceUnavailable
	}
	if status == http.StatusBadGateway {
		s.opts.Log.Warn("api: steamcmd", "err", err)
	}
	writeJSON(w, status, map[string]string{"error": err.Error(), "code": code})
}

func (s *Server) handleSteamUploadLogin(w http.ResponseWriter, r *http.Request) {
	if s.refuseAgent(w, r) {
		return
	}
	var req struct {
		User     string `json:"user"`
		Password string `json:"password"`
		Code     string `json:"code"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	st, err := s.opts.SteamUpload.StartLogin(req.User, req.Password, req.Code)
	if err != nil {
		s.steamUploadError(w, err)
		return
	}
	s.opts.Log.Info("steamcmd sign-in started")
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) handleSteamUploadCode(w http.ResponseWriter, r *http.Request) {
	if s.refuseAgent(w, r) {
		return
	}
	var req struct {
		Code string `json:"code"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	st, err := s.opts.SteamUpload.SubmitCode(req.Code)
	if err != nil {
		s.steamUploadError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) handleSteamUploadCancel(w http.ResponseWriter, _ *http.Request) {
	s.opts.SteamUpload.CancelLogin()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleSteamUploadCheck(w http.ResponseWriter, r *http.Request) {
	st, err := s.opts.SteamUpload.CheckSession(r.Context())
	if err != nil {
		s.steamUploadError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) handleSteamUploadInstall(w http.ResponseWriter, r *http.Request) {
	if s.refuseAgent(w, r) {
		return
	}
	st, err := s.opts.SteamUpload.Install(r.Context())
	if err != nil {
		s.steamUploadError(w, err)
		return
	}
	s.opts.Log.Info("steamcmd installed", "path", st.SteamCMD)
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) handleSteamUploadForget(w http.ResponseWriter, r *http.Request) {
	if s.refuseAgent(w, r) {
		return
	}
	st, err := s.opts.SteamUpload.Forget()
	if err != nil {
		s.steamUploadError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}
