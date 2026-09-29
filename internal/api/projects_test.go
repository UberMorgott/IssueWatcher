package api

import (
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/store"
	"github.com/UberMorgott/issuewatcher/internal/syncer"
)

// GET /api/projects?group=1 folds linked mod pages into their code project's
// row (integrations with per-channel counts); POST /api/projects/{id}/sync
// syncs the project through its sources.
func TestProjectsGroupedAndSync(t *testing.T) {
	e := syncedEnv(t)
	var chunk store.RepoChunk
	if code := e.call(t, http.MethodGet, "/api/projects?limit=10&group=1&sort=unread&dir=desc", "", &chunk); code != http.StatusOK ||
		chunk.Total != 1 || len(chunk.Items) != 1 || len(chunk.Items[0].Integrations) != 1 {
		t.Fatalf("grouped %d %+v", code, chunk)
	}
	row := chunk.Items[0]
	if in := row.Integrations[0]; in.ID != row.ID || in.Platform != "github" || in.Open != 1 || in.Closed != 1 || in.Unread != 1 {
		t.Fatalf("integration %+v", in)
	}
	var flat store.RepoChunk
	if e.call(t, http.MethodGet, "/api/projects?limit=10", "", &flat); len(flat.Items) != 1 || flat.Items[0].Integrations != nil {
		t.Fatalf("flat %+v", flat)
	}

	done := make(chan syncer.Progress, 4)
	e.sync.OnProgress(func(p syncer.Progress) {
		if p.State == syncer.ProgressDone || p.State == syncer.ProgressError {
			done <- p
		}
	})
	var res map[string][]string
	if code := e.call(t, http.MethodPost, "/api/projects/"+strconv.FormatInt(row.ID, 10)+"/sync", "", &res); code != http.StatusAccepted ||
		len(res["started"]) != 1 || res["started"][0] != "github" || len(res["missing"]) != 0 {
		t.Fatalf("project sync %d %+v", code, res)
	}
	select {
	case p := <-done:
		if p.State != syncer.ProgressDone || p.Repo != "octo/app" {
			t.Fatalf("project sync progress %+v", p)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("project sync did not finish")
	}
	for path, want := range map[string]int{"/api/projects/9999/sync": http.StatusNotFound, "/api/projects/x/sync": http.StatusBadRequest} {
		if code := e.call(t, http.MethodPost, path, "", nil); code != want {
			t.Errorf("%s: %d, want %d", path, code, want)
		}
	}
}
