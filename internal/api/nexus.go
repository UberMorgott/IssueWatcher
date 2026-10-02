package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/UberMorgott/issuewatcher/internal/provider/nexus"
)

// NexusKeys is the Nexus API key store (nexus.Keys).
type NexusKeys interface {
	Status() (nexus.KeyStatus, error)
	Save(ctx context.Context, key string) (nexus.KeyStatus, error)
	Check(ctx context.Context) (nexus.KeyStatus, error)
}

// Nexus API key (Phase 7 publishing; Settings › Платформы › Nexus). The key is
// write-only: responses carry only whether one is stored and its account.
//
//	GET  /api/providers/nexus        {hasApiKey, user?, userId?, checkedAt?}
//	PUT  /api/providers/nexus        {apiKey} ("" = remove) → the same; 400 {code: bad_api_key}, 502 {code: check_failed}
//	POST /api/providers/nexus/check  re-validates the stored key → the same; 409 {code: no_api_key}
func (s *Server) registerNexus(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/providers/nexus", s.handleNexus)
	mux.HandleFunc("PUT /api/providers/nexus", s.handleNexusSave)
	mux.HandleFunc("POST /api/providers/nexus/check", s.handleNexusCheck)
}

func (s *Server) handleNexus(w http.ResponseWriter, _ *http.Request) {
	st, err := s.opts.NexusKey.Status()
	if err != nil {
		s.internalError(w, "nexus settings", err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) handleNexusSave(w http.ResponseWriter, r *http.Request) {
	var req struct {
		APIKey *string `json:"apiKey"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req); err != nil || req.APIKey == nil {
		errJSON(w, http.StatusBadRequest, "bad json")
		return
	}
	st, err := s.opts.NexusKey.Save(r.Context(), *req.APIKey)
	s.nexusKeyResult(w, st, err)
}

func (s *Server) handleNexusCheck(w http.ResponseWriter, r *http.Request) {
	st, err := s.opts.NexusKey.Check(r.Context())
	s.nexusKeyResult(w, st, err)
}

// nexusKeyResult answers a save or check. Errors never carry the key.
func (s *Server) nexusKeyResult(w http.ResponseWriter, st nexus.KeyStatus, err error) {
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, st)
	case errors.Is(err, nexus.ErrBadAPIKey):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error(), "code": "bad_api_key"})
	case errors.Is(err, nexus.ErrNoAPIKey):
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error(), "code": "no_api_key"})
	case errors.Is(err, context.Canceled):
		errJSON(w, http.StatusServiceUnavailable, "cancelled")
	default:
		s.opts.Log.Warn("api: nexus key check", "err", err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error(), "code": "check_failed"})
	}
}
