package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/provider"
)

// With a reader pool, page reads do not queue behind an open write
// transaction on the single writer connection (a sync applying a batch).
func TestReadsDoNotWaitForAWrite(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "t.db")
	db, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	rd, err := OpenReader(path, 4)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rd.Close() })
	s := NewWithReader(db, rd)
	src, err := s.UpsertSource(ctx, "github", "me")
	if err != nil {
		t.Fatal(err)
	}
	projects, err := s.SyncProjects(ctx, src, []provider.Project{{ExternalID: "o/app", Name: "o/app"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyItems(ctx, src, projects[0].ID, []provider.Item{item("a", 1, true, t0)}, "me"); err != nil {
		t.Fatal(err)
	}

	tx, err := db.BeginTx(ctx, nil) // holds the only writer connection
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `UPDATE items SET title = 'being written'`); err != nil {
		t.Fatal(err)
	}
	// A read that queued behind the writer would wait until the transaction ends
	// (never, here): the deadline turns that into a failure. No wall-clock bound
	// on the reads themselves, which vary with -race and machine load.
	rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	chunk, err := s.Issues(rctx, IssueFilter{})
	if err != nil || len(chunk.Items) != 1 || chunk.Items[0].Title != "title a" {
		t.Fatalf("issues during a write: %+v %v", chunk, err)
	}
	if _, err := s.ReposChunk(rctx, RepoQuery{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Stats(rctx, 0, 4, t0); err != nil {
		t.Fatal(err)
	}
	if _, err := rd.ExecContext(ctx, `UPDATE items SET title = 'x'`); err == nil {
		t.Fatal("the reader pool accepts writes")
	}
}

func TestProjectLabelsStored(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	src, err := s.UpsertSource(ctx, "github", "me")
	if err != nil {
		t.Fatal(err)
	}
	projects, err := s.SyncProjects(ctx, src, []provider.Project{{ExternalID: "o/app", Name: "o/app"}})
	if err != nil {
		t.Fatal(err)
	}
	id := projects[0].ID
	if l, at, err := s.ProjectLabels(ctx, id); err != nil || len(l) != 0 || !at.IsZero() {
		t.Fatalf("never fetched: %v %v %v", l, at, err)
	}
	want := []provider.Label{{Name: "zeta", Color: "ff0000"}, {Name: "alpha", Description: "d"}}
	if err := s.SetProjectLabels(ctx, id, want, t0); err != nil {
		t.Fatal(err)
	}
	if err := s.SetProjectLabels(ctx, id, want, t0); err != nil { // replace, not append
		t.Fatal(err)
	}
	l, at, err := s.ProjectLabels(ctx, id)
	if err != nil || len(l) != 2 || l[0] != want[0] || l[1] != want[1] || !at.Equal(t0) {
		t.Fatalf("stored labels %+v %v %v", l, at, err)
	}
	if _, _, err := s.ProjectLabels(ctx, 9999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("labels of no project: %v", err)
	}
}
