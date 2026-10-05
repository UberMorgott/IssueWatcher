package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/UberMorgott/issuewatcher/internal/tools"
)

// Tool is a helper the app provisions into data\tools (steamcmd,
// steam_api64.dll): set up automatically at startup, retried from Settings.
type Tool interface {
	Tool() tools.Status
	Provision(ctx context.Context) (tools.Status, error)
}

// Helpers next to the portable binary (Settings › Платформы).
//
//	GET  /api/tools                   {tools: [{name, state: missing|working|ready|error, path?, source?, error?, at?}]}
//	POST /api/tools/{name}/provision  set it up now → the tool; 404 unknown, 409 {code: busy}, 502 {code: failed, error}
func (s *Server) registerTools(mux *http.ServeMux) {
	if len(s.opts.Tools) == 0 {
		return
	}
	mux.HandleFunc("GET /api/tools", s.handleTools)
	mux.HandleFunc("POST /api/tools/{name}/provision", s.handleToolProvision)
}

func (s *Server) handleTools(w http.ResponseWriter, _ *http.Request) {
	out := make([]tools.Status, 0, len(s.opts.Tools))
	for _, t := range s.opts.Tools {
		out = append(out, t.Tool())
	}
	writeJSON(w, http.StatusOK, map[string]any{"tools": out})
}

func (s *Server) handleToolProvision(w http.ResponseWriter, r *http.Request) {
	if s.refuseAgent(w, r) {
		return
	}
	name := r.PathValue("name")
	for _, t := range s.opts.Tools {
		if t.Tool().Name != name {
			continue
		}
		st, err := t.Provision(r.Context())
		switch {
		case err == nil:
			s.opts.Log.Info("tool ready", "tool", name, "path", st.Path)
			writeJSON(w, http.StatusOK, st)
		case errors.Is(err, tools.ErrBusy):
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error(), "code": "busy"})
		default:
			s.opts.Log.Warn("api: tool provisioning", "tool", name, "err", err)
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error(), "code": "failed"})
		}
		return
	}
	errJSON(w, http.StatusNotFound, "unknown tool")
}
