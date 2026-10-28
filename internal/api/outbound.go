package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/UberMorgott/issuewatcher/internal/outbound"
)

// Outbound runs URL-addressed post / reply / read calls (outbound.Service).
type Outbound interface {
	Do(ctx context.Context, req outbound.Request) (outbound.Result, error)
}

// handleOutbound: POST /api/outbound/{verb} {url, parent?, body?, dryRun?}
// (verb post | reply | read | download) addresses the target by its URL, no
// prior item sync. 200 read / dry run, 201 posted; errors {ok:false, code,
// error}: 400 bad_url / empty_body, 403 agent_caller | caller_unknown (a
// non-dry-run write from an agent run), 404 not_found, 409 unsupported /
// needs_parent / reply_off / no_provider / not_signed_in / relogin / too_long,
// 429 rate_limited, 502 outcome_unknown / platform_error.
func (s *Server) handleOutbound(w http.ResponseWriter, r *http.Request) {
	var req outbound.Request
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxCommentBytes)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "bad json", "code": "bad_request"})
		return
	}
	req.Verb = r.PathValue("verb")
	write := req.Verb == outbound.VerbPost || req.Verb == outbound.VerbReply
	if write && !req.DryRun && s.refuseAgent(w, r) {
		return
	}
	res, err := s.opts.Outbound.Do(r.Context(), req)
	if err != nil {
		var e *outbound.Error
		if !errors.As(err, &e) {
			e = &outbound.Error{Code: outbound.CodePlatformError, Msg: err.Error()}
		}
		if e.Code == outbound.CodePlatformError || e.Code == outbound.CodeOutcomeUnknown {
			s.opts.Log.Warn("api: outbound", "verb", req.Verb, "url", req.URL, "code", e.Code, "err", e.Msg)
		}
		writeJSON(w, outboundStatus(e.Code), map[string]any{"ok": false, "error": e.Msg, "code": e.Code, "verb": req.Verb})
		return
	}
	status := http.StatusOK
	if res.Code == outbound.CodePosted {
		status = http.StatusCreated
		s.opts.Log.Info("outbound post", "verb", req.Verb, "provider", res.Provider, "url", res.URL)
	}
	writeJSON(w, status, res)
}

func outboundStatus(code string) int {
	switch code {
	case outbound.CodeBadURL, outbound.CodeEmptyBody:
		return http.StatusBadRequest
	case outbound.CodeNotFound:
		return http.StatusNotFound
	case outbound.CodeRateLimited:
		return http.StatusTooManyRequests
	case outbound.CodeOutcomeUnknown, outbound.CodePlatformError:
		return http.StatusBadGateway
	}
	return http.StatusConflict
}
