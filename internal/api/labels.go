package api

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/provider"
	"github.com/UberMorgott/issuewatcher/internal/store"
)

// Project labels are read from SQLite (store.ProjectLabels): GET never calls
// GitHub. A missing or stale list (older than labelsTTL, or ?refresh=1) is
// fetched in the background; when it lands (or fails) the tabs get
// data.changed{reason:"labels", repo} and refetch.

// labelsTTL is how long a stored label list counts as fresh.
const labelsTTL = 24 * time.Hour

// labelsRetry is the pause after a failed refresh before GET starts another.
const labelsRetry = time.Minute

// ProjectLabels is the GET /api/projects/{id}/labels answer.
type ProjectLabels struct {
	Labels     []provider.Label `json:"labels"`
	FetchedAt  string           `json:"fetchedAt"`  // "" = never fetched
	Refreshing bool             `json:"refreshing"` // a background fetch is running
	Error      string           `json:"error,omitempty"`
}

// labelRefresh tracks the background label fetches.
type labelRefresh struct {
	mu       sync.Mutex
	inflight map[int64]bool
	failed   map[int64]labelFail
}

type labelFail struct {
	err string
	at  time.Time
}

func (s *Server) handleProjectLabels(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	p, err := s.opts.Store.Repo(r.Context(), id)
	if s.jobError(w, err) {
		return
	}
	labels, at, err := s.opts.Store.ProjectLabels(r.Context(), id)
	if s.jobError(w, err) {
		return
	}
	out := ProjectLabels{Labels: labels}
	if !at.IsZero() {
		out.FetchedAt = at.UTC().Format(time.RFC3339)
	}
	if p.Platform == store.CodePlatform && s.opts.Runner != nil {
		stale := at.IsZero() || time.Since(at) > labelsTTL || r.URL.Query().Get("refresh") == "1"
		out.Refreshing, out.Error = s.refreshLabels(id, p.Name, stale) //nolint:contextcheck // the fetch outlives the request: server background context
	}
	writeJSON(w, http.StatusOK, out)
}

// refreshLabels starts a background fetch of project id's labels when want
// and none runs or failed within labelsRetry; it reports whether one is
// running and the last failure.
func (s *Server) refreshLabels(id int64, name string, want bool) (running bool, lastErr string) {
	lr := &s.labels
	lr.mu.Lock()
	defer lr.mu.Unlock()
	if lr.inflight == nil {
		lr.inflight, lr.failed = map[int64]bool{}, map[int64]labelFail{}
	}
	f := lr.failed[id]
	if lr.inflight[id] || !want || (!f.at.IsZero() && time.Since(f.at) < labelsRetry) {
		return lr.inflight[id], f.err
	}
	lr.inflight[id] = true
	go s.fetchLabels(id, name)
	return true, f.err
}

func (s *Server) fetchLabels(id int64, name string) {
	ctx, cancel := context.WithTimeout(s.bg, 30*time.Second)
	defer cancel()
	labels, err := s.opts.Runner.RepoLabels(ctx, name)
	if err == nil {
		err = s.opts.Store.SetProjectLabels(ctx, id, labels, time.Now())
	}
	lr := &s.labels
	lr.mu.Lock()
	delete(lr.inflight, id)
	if err != nil {
		lr.failed[id] = labelFail{err: err.Error(), at: time.Now()}
		s.opts.Log.Warn("api: refresh project labels", "project", name, "err", err)
	} else {
		delete(lr.failed, id)
	}
	lr.mu.Unlock()
	if s.bg.Err() == nil {
		s.Publish(EventDataChanged, DataChange{Reason: "labels", Repo: name})
	}
}
