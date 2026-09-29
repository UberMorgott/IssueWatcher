package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/UberMorgott/issuewatcher/internal/provider"
)

// Platform states (Settings › Платформы).
const (
	PlatformDisabled    = "disabled"    // switched off / not configured
	PlatformUnknown     = "unknown"     // on, not synced or checked yet
	PlatformConnected   = "connected"   // account reached
	PlatformSignedOut   = "signed_out"  // no session / author / SteamID
	PlatformRelogin     = "relogin"     // session expired or refused (Cloudflare): sign in again
	PlatformUnavailable = "unavailable" // MCP server missing or failed to start
	PlatformError       = "error"
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
	Error        string                `json:"error,omitempty"`
	Running      bool                  `json:"running"` // MCP server child alive
	Projects     int                   `json:"projects"`
	LastSync     string                `json:"lastSync,omitempty"`
	CheckedAt    string                `json:"checkedAt,omitempty"`
	Capabilities provider.Capabilities `json:"capabilities"`
}

// Platforms reports and checks the platform accounts (cmd/issuewatcher mods.go).
type Platforms interface {
	Platforms(ctx context.Context) []PlatformStatus
	Check(ctx context.Context, id string) (PlatformStatus, error)
}

// Platform endpoints:
//
//	GET  /api/platforms             every platform (GitHub first) with state and capabilities
//	POST /api/platforms/{id}/check  live account check (nexus | curseforge | steam)
func (s *Server) registerPlatforms(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/platforms", s.handlePlatforms)
	mux.HandleFunc("POST /api/platforms/{id}/check", s.handlePlatformCheck)
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
