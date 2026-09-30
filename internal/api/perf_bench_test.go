package api

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"testing/fstest"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/config"
	"github.com/UberMorgott/issuewatcher/internal/provider"
	"github.com/UberMorgott/issuewatcher/internal/provider/github"
	"github.com/UberMorgott/issuewatcher/internal/store"
	"github.com/UberMorgott/issuewatcher/internal/syncer"
)

// Perf regression guard for the read paths (docs/ARCHITECTURE.md → Storage &
// sync model: every GET from SQLite, < 50 ms). A store seeded with benchProjects
// GitHub projects × benchItemsPer items (2 comments each, a tenth of the
// projects mapped to a folder) behind the real handler chain (host guard,
// bearer auth, reader pool); one op = one request.
//
//	go test ./internal/api -run '^$' -bench BenchmarkReadPaths -benchtime 200x
const (
	benchProjects = 100
	benchItemsPer = 50 // 5000 items
)

func BenchmarkReadPaths(b *testing.B) {
	s := benchServer(b)
	for _, path := range []string{
		"/api/items?limit=50",
		"/api/items?limit=50&state=open",
		"/api/items?limit=50&q=crash",
		"/api/projects",
		"/api/projects?limit=50&group=1",
		"/api/stats",
		"/api/folders",
	} {
		b.Run(path, func(b *testing.B) {
			for b.Loop() {
				rec := httptest.NewRecorder()
				req := httptest.NewRequestWithContext(b.Context(), http.MethodGet, s.BaseURL()+path, nil)
				req.Header.Set("Authorization", "Bearer "+s.token)
				s.srv.Handler.ServeHTTP(rec, req)
				if rec.Code != http.StatusOK {
					b.Fatalf("%s: %d %s", path, rec.Code, rec.Body.String())
				}
			}
		})
	}
}

// benchServer builds a Server over a seeded store with a separate reader pool,
// as the app runs it. Nothing is served over the network: requests go straight
// to the handler.
func benchServer(b *testing.B) *Server {
	b.Helper()
	ctx := b.Context()
	dir := b.TempDir()
	path := filepath.Join(dir, "bench.db")
	db, err := store.Open(ctx, path)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = db.Close() })
	rd, err := store.OpenReader(path, 4)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = rd.Close() })
	st := store.NewWithReader(db, rd)
	seedBench(b, st, dir)

	cs, err := config.Open(filepath.Join(dir, "cfg"))
	if err != nil {
		b.Fatal(err)
	}
	auth := github.NewAuth(filepath.Join(dir, "secrets")) // signed out: nothing reaches GitHub
	sy := syncer.New(syncer.Options{Store: st, Provider: github.NewProvider(auth), Log: slog.New(slog.DiscardHandler)})
	s, err := New(ctx, Options{
		Assets: fstest.MapFS{"index.html": {Data: []byte(indexHTML)}},
		Log:    slog.New(slog.DiscardHandler),
		Store:  st, Sync: syncer.NewGroup(sy), Settings: cfgStore{cs},
	})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() {
		_ = s.Shutdown(context.Background())
		_ = s.ln.Close()
	})
	return s
}

func seedBench(b *testing.B, st *store.Store, dir string) {
	b.Helper()
	ctx := b.Context()
	src, err := st.UpsertSource(ctx, "github", "me")
	if err != nil {
		b.Fatal(err)
	}
	list := make([]provider.Project, benchProjects)
	for i := range list {
		name := fmt.Sprintf("octo/r%d", i)
		list[i] = provider.Project{ExternalID: name, Name: name, URL: "https://github.com/" + name}
	}
	projects, err := st.SyncProjects(ctx, src, list)
	if err != nil {
		b.Fatal(err)
	}
	t0 := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	for pi, p := range projects {
		items := make([]provider.Item, benchItemsPer)
		for i := range items {
			at := t0.Add(time.Duration(pi*benchItemsPer+i) * time.Hour)
			body := "steps to reproduce"
			if i%10 == 0 {
				body = "the game crashes on load"
			}
			it := provider.Item{
				ExternalID: fmt.Sprintf("I_%d_%d", pi, i), Number: i + 1, Title: fmt.Sprintf("issue %d of %s", i+1, p.Name),
				Body: body, URL: fmt.Sprintf("%s/issues/%d", p.URL, i+1), Author: "user", Open: i%3 != 0,
				Labels: []string{"bug"}, CreatedAt: at, UpdatedAt: at.Add(time.Minute),
			}
			if !it.Open {
				it.ClosedAt = it.UpdatedAt
			}
			for c := range 2 {
				it.Comments = append(it.Comments, provider.Comment{
					ExternalID: fmt.Sprintf("C_%d_%d_%d", pi, i, c), Author: "user", Body: "a comment",
					CreatedAt: at.Add(time.Duration(c+1) * time.Second), UpdatedAt: at.Add(time.Duration(c+1) * time.Second),
				})
			}
			items[i] = it
		}
		if _, err := st.ApplyItems(ctx, src, p.ID, items, "me"); err != nil {
			b.Fatal(err)
		}
		if pi%10 == 0 {
			if err := st.SetLocalPath(ctx, p.ID, dir); err != nil {
				b.Fatal(err)
			}
		}
	}
}
