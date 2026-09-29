package store

import (
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
	start := time.Now()
	chunk, err := s.Issues(ctx, IssueFilter{})
	if err != nil || len(chunk.Items) != 1 || chunk.Items[0].Title != "title a" {
		t.Fatalf("issues during a write: %+v %v", chunk, err)
	}
	if _, err := s.ReposChunk(ctx, RepoQuery{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Stats(ctx, 0, 4, t0); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 50*time.Millisecond {
		t.Fatalf("reads took %v during a write transaction", d)
	}
	if _, err := rd.ExecContext(ctx, `UPDATE items SET title = 'x'`); err == nil {
		t.Fatal("the reader pool accepts writes")
	}
}
