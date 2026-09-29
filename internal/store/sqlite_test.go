package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strconv"
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

// Migration 006 rebuilds jobs: every row of a 005 database must survive intact.
func TestMigration006KeepsJobs(t *testing.T) {
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
	if _, err := s.ApplyItems(ctx, src, pid, []provider.Item{item("a", 1, true, t0), item("b", 2, true, t0), item("c", 3, true, t0)}, "me"); err != nil {
		t.Fatal(err)
	}
	// One job in every state (distinct item+flow so jobs_active allows the unfinished ones).
	rows := []struct {
		ext, flow, state string
	}{{"a", "fix", "queued"}, {"b", "fix", "running"}, {"c", "fix", "needs_review"}, {"a", "reply", "done"}, {"b", "reply", "failed"}, {"c", "reply", "cancelled"}}
	for i, r := range rows {
		if _, err := db.ExecContext(ctx, `INSERT INTO jobs (id, item_id, project_id, flow, state, profile_id, attempt, phase, branch,
			worktree, base_sha, error, result, created_at, started_at, finished_at, updated_at)
			SELECT ?, id, project_id, ?, ?, 'claude', ?, 'agent', 'iw/x', 'wt', 'sha', 'err', ?, 'c', 's', 'f', 'u' FROM items WHERE external_id = ?`,
			10+i, r.flow, r.state, i+1, `{"n":`+strconv.Itoa(i)+`}`, r.ext); err != nil {
			t.Fatal(err)
		}
	}
	const dump = `SELECT json_group_array(json_array(id, item_id, project_id, flow, state, profile_id, attempt, phase, branch,
		worktree, base_sha, error, result, created_at, started_at, finished_at, updated_at)) FROM (SELECT * FROM jobs ORDER BY id)`
	var before string
	if err := db.QueryRowContext(ctx, dump).Scan(&before); err != nil {
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
	var after string
	if err := db.QueryRowContext(ctx, dump).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("rows changed:\nbefore %s\nafter  %s", before, after)
	}
	var odd int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM jobs WHERE origin != 'manual' OR rule_id != ''`).Scan(&odd); err != nil || odd != 0 {
		t.Fatalf("origin defaults: %d %v", odd, err)
	}
	for _, idx := range []string{"jobs_item", "jobs_state", "jobs_active"} {
		var n int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type = 'index' AND name = ?`, idx).Scan(&n); err != nil || n != 1 {
			t.Fatalf("index %s: %d %v", idx, n, err)
		}
	}
	s = New(db)
	var a int64
	if err := db.QueryRowContext(ctx, `SELECT id FROM items WHERE external_id = 'a'`).Scan(&a); err != nil {
		t.Fatal(err)
	}
	// jobs_active still holds: item a has a queued fix job.
	if j, err := s.CreateJob(ctx, a, "fix", "claude", "", ""); !errors.Is(err, ErrJobExists) || j.ID != 10 {
		t.Fatalf("second active fix: %+v %v", j, err)
	}
	j, err := s.CreateJob(ctx, a, "verify", "claude", OriginRule, "r1")
	if err != nil || j.Flow != "verify" || j.Origin != OriginRule || j.RuleID != "r1" {
		t.Fatalf("rule verify job: %+v %v", j, err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO jobs (item_id, project_id, flow, origin) VALUES (?, ?, 'label', 'bogus')`, a, pid); err == nil {
		t.Fatal("origin CHECK not enforced")
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
