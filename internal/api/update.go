package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/UberMorgott/issuewatcher/internal/selfupdate"
)

// EventUpdateStatus carries selfupdate.Status on every change of the updater.
const EventUpdateStatus = "update.status"

// Updater is the self-updater (internal/selfupdate.Updater).
type Updater interface {
	Status() selfupdate.Status
	Check(ctx context.Context) (selfupdate.Status, error)
	Install() error
}

func (s *Server) registerUpdate(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/update", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, s.opts.Updates.Status())
	})
	// Check: 200 with the status; a failed check is reported in status.error too.
	mux.HandleFunc("POST /api/update/check", func(w http.ResponseWriter, r *http.Request) {
		st, err := s.opts.Updates.Check(r.Context())
		if err != nil && !errors.Is(err, selfupdate.ErrBusy) {
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error(), "status": st})
			return
		}
		writeJSON(w, http.StatusOK, st)
	})
	// Install: 202, progress and the outcome arrive as update.status events.
	mux.HandleFunc("POST /api/update/install", func(w http.ResponseWriter, _ *http.Request) {
		if err := s.opts.Updates.Install(); err != nil {
			status := http.StatusConflict
			if errors.Is(err, selfupdate.ErrDevBuild) {
				status = http.StatusBadRequest
			}
			writeJSON(w, status, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusAccepted, s.opts.Updates.Status())
	})
}
