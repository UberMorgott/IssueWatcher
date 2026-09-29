package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"github.com/UberMorgott/issuewatcher/internal/provider"
)

func TestOpenAppliesMigrationsIdempotently(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "sub", "issuewatcher.db")

	db, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	var version int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != 6 {
		t.Fatalf("user_version = %d, want 6", version)
	}
	for _, table := range []string{"sources", "projects", "items", "comments", "jobs"} {
		var n int
		err := db.QueryRowContext(ctx,
			"SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = ?", table).Scan(&n)
		if err != nil || n != 1 {
			t.Fatalf("table %s missing (n=%d, err=%v)", table, n, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen: migrations must not re-run.
	db, err = Open(ctx, path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = db.Close() }()
}

// Migration 006 starts jobs fresh (owner: no row-preserving migration): old
// rows go, everything else stays, new ids continue after the old maximum.
func TestMigration006FreshJobs(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	if err := migrateTo(ctx, db, 5); err != nil {
		t.Fatal(err)
	}
	s := New(db)
	src, err := s.UpsertSource(ctx, "github", "me")
	if err != nil {
		t.Fatal(err)
	}
	projects, err := s.SyncProjects(ctx, src, []provider.Project{{ExternalID: "o/app", Name: "o/app", URL: "u"}})
	if err != nil {
		t.Fatal(err)
	}
	pid := projects[0].ID
	if _, err := s.ApplyItems(ctx, src, pid, []provider.Item{item("a", 1, true, t0)}, "me"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO jobs (id, item_id, project_id, flow) SELECT 10, id, project_id, 'fix' FROM items`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var jobs, items int
	if err := db.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM jobs), (SELECT count(*) FROM items)`).Scan(&jobs, &items); err != nil ||
		jobs != 0 || items != 1 {
		t.Fatalf("after 006: jobs %d items %d %v", jobs, items, err)
	}
	s = New(db)
	var a int64
	if err := db.QueryRowContext(ctx, `SELECT id FROM items WHERE external_id = 'a'`).Scan(&a); err != nil {
		t.Fatal(err)
	}
	j, err := s.CreateJob(ctx, a, "label", "claude", OriginRule, "r1")
	if err != nil || j.ID != 11 || j.Flow != "label" || j.Origin != OriginRule || j.RuleID != "r1" {
		t.Fatalf("rule label job: %+v %v", j, err)
	}
	if m, err := s.CreateJob(ctx, a, "fix", "claude", "", ""); err != nil || m.Origin != OriginManual || m.ID != 12 {
		t.Fatalf("manual fix job: %+v %v", m, err)
	}
	if dup, err := s.CreateJob(ctx, a, "label", "claude", "", ""); !errors.Is(err, ErrJobExists) || dup.ID != j.ID {
		t.Fatalf("second active label: %+v %v", dup, err)
	}
	for _, bad := range [][2]string{{"label", "bogus"}, {"verify", "manual"}} {
		if _, err := db.ExecContext(ctx, `INSERT INTO jobs (item_id, project_id, flow, origin) VALUES (?, ?, ?, ?)`, a, pid, bad[0], bad[1]); err == nil {
			t.Fatalf("CHECK accepts %v", bad)
		}
	}
	chunk, err := s.Jobs(ctx, JobFilter{Origin: OriginRule})
	if err != nil || len(chunk.Items) != 1 || chunk.Items[0].ID != j.ID {
		t.Fatalf("origin filter: %+v %v", chunk, err)
	}
}

func TestForeignKeysEnforced(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "fk.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	_, err = db.ExecContext(ctx,
		"INSERT INTO jobs (item_id, flow) VALUES (999, 'fix')")
	if err == nil {
		t.Fatal("insert with dangling item_id succeeded; foreign_keys pragma not applied")
	}
}
