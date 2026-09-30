package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/UberMorgott/issuewatcher/internal/provider"
)

// Platform states (Settings › Платформы).
const (
	PlatformDisabled  = "disabled"   // switched off / not configured
	PlatformUnknown   = "unknown"    // on, not synced or checked yet
	PlatformConnected = "connected"  // account reached
	PlatformSignedOut = "signed_out" // no session / author / SteamID
	PlatformRelogin   = "relogin"    // session expired or refused (Cloudflare): sign in again
	PlatformError     = "error"
)

// Where a platform's web session came from (PlatformStatus.Session).
const (
	SessionNone    = "none"    // no session: public reads only
	SessionBrowser = "browser" // imported from an installed browser
	SessionWindow  = "window"  // signed in in the sign-in window
	SessionManual  = "manual"  // cookies pasted by hand
	SessionQR      = "qr"      // Steam QR sign-in (renews itself)
	SessionStored  = "stored"  // a session of unknown origin
	SessionProfile = "profile" // native: IssueWatcher's own browser profile was already signed in
)

// ErrUnknownPlatform: Check of a platform that has no account check.
var ErrUnknownPlatform = errors.New("api: unknown platform")

// PlatformStatus is one platform's card in Settings › Платформы.
type PlatformStatus struct {
	ID           string                `json:"id"`
	Name         string                `json:"name"`
	Enabled      bool                  `json:"enabled"`
	State        string                `json:"state"`
	Account      string                `json:"account,omitempty"`
	AccountName  string                `json:"accountName,omitempty"` // display name (Steam persona) when Account is an id
	Session      string                `json:"session,omitempty"`     // Session* below; "" = not known yet
	Browser      string                `json:"browser,omitempty"`     // SessionBrowser: which browser
	Error        string                `json:"error,omitempty"`
	Projects     int                   `json:"projects"`
	LastSync     string                `json:"lastSync,omitempty"`
	CheckedAt    string                `json:"checkedAt,omitempty"`
	Capabilities provider.Capabilities `json:"capabilities"`
}

// Sign-in states (POST/GET /api/platforms/{id}/login).
const (
	LoginIdle      = "idle"      // nothing running
	LoginQR        = "qr"        // Steam: show challengeUrl as a QR code, scan it in the Steam app
	LoginScanned   = "scanned"   // Steam: scanned, approve in the app
	LoginWindow    = "window"    // Nexus / CurseForge: the sign-in window is open
	LoginConnected = "connected" // signed in; account detected
	LoginExpired   = "expired"   // the QR code timed out or was declined
	LoginFailed    = "failed"
)

// LoginStatus is the progress of «Подключить» on one platform.
type LoginStatus struct {
	Platform     string `json:"platform"`
	State        string `json:"state"`
	ChallengeURL string `json:"challengeUrl,omitempty"`
	Account      string `json:"account,omitempty"`
	Error        string `json:"error,omitempty"`
	// Via (state window): "default-browser" = the login page opened in the
	// user's default browser (Browser names it), "window" = the server's own.
	Via     string `json:"via,omitempty"`
	Browser string `json:"browser,omitempty"`
}

// Platforms reports, checks and signs in the platform accounts (cmd/issuewatcher mods.go).
type Platforms interface {
	Platforms(ctx context.Context) []PlatformStatus
	Check(ctx context.Context, id string) (PlatformStatus, error)
	Login(ctx context.Context, id string) (LoginStatus, error)
	LoginStatus(ctx context.Context, id string) (LoginStatus, error)
	CancelLogin(id string) error
	// Logout drops the platform's web session («Выйти»); forget also clears the
	// detected identity (SteamID, uploader account) and switches the platform
	// off («Отключить» → «Не подключено»).
	Logout(ctx context.Context, id string, forget bool) (PlatformStatus, error)
}

// Platform endpoints:
//
//	GET  /api/platforms             every platform (GitHub first) with state and capabilities
//	POST /api/platforms/{id}/check  live account check (nexus | curseforge | steam)
//	POST /api/platforms/{id}/login  «Подключить»: Steam QR / Nexus, CurseForge sign-in window (switches the platform on)
//	GET  /api/platforms/{id}/login  sign-in progress (polled by the dashboard)
//	DELETE /api/platforms/{id}/login  cancel a Steam QR sign-in
//	POST /api/platforms/{id}/logout  «Выйти»: drop the session (?forget=1 «Отключить»: also the identity)
func (s *Server) registerPlatforms(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/platforms", s.handlePlatforms)
	mux.HandleFunc("POST /api/platforms/{id}/check", s.handlePlatformCheck)
	mux.HandleFunc("POST /api/platforms/{id}/login", s.handlePlatformLogin)
	mux.HandleFunc("GET /api/platforms/{id}/login", s.handlePlatformLogin)
	mux.HandleFunc("DELETE /api/platforms/{id}/login", s.handlePlatformLogin)
	mux.HandleFunc("POST /api/platforms/{id}/logout", s.handlePlatformLogout)
}

func (s *Server) handlePlatformLogout(w http.ResponseWriter, r *http.Request) {
	forget := r.URL.Query().Get("forget") == "1"
	st, err := s.opts.Platforms.Logout(r.Context(), r.PathValue("id"), forget)
	switch {
	case errors.Is(err, ErrUnknownPlatform):
		errJSON(w, http.StatusNotFound, "unknown platform")
	case err != nil:
		errJSON(w, http.StatusBadGateway, err.Error())
	default:
		writeJSON(w, http.StatusOK, st)
	}
}

func (s *Server) handlePlatformLogin(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var (
		st  LoginStatus
		err error
	)
	switch r.Method {
	case http.MethodPost:
		st, err = s.opts.Platforms.Login(r.Context(), id)
	case http.MethodDelete:
		if err = s.opts.Platforms.CancelLogin(id); err == nil {
			st = LoginStatus{Platform: id, State: LoginIdle}
		}
	default:
		st, err = s.opts.Platforms.LoginStatus(r.Context(), id)
	}
	switch {
	case errors.Is(err, ErrUnknownPlatform):
		errJSON(w, http.StatusNotFound, "unknown platform")
	case err != nil:
		// A failed start (server missing, Steam unreachable) is a state, not a 500.
		writeJSON(w, http.StatusOK, LoginStatus{Platform: id, State: LoginFailed, Error: err.Error()})
	default:
		writeJSON(w, http.StatusOK, st)
	}
}

func (s *Server) handlePlatforms(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.opts.Platforms.Platforms(r.Context()))
}

func (s *Server) handlePlatformCheck(w http.ResponseWriter, r *http.Request) {
	st, err := s.opts.Platforms.Check(r.Context(), r.PathValue("id"))
	if errors.Is(err, ErrUnknownPlatform) {
		errJSON(w, http.StatusNotFound, "unknown platform")
		return
	}
	if err != nil {
		s.internalError(w, "platform check", err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}
