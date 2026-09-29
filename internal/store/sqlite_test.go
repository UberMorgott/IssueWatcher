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
	if version != 11 {
		t.Fatalf("user_version = %d, want 11", version)
	}
	for _, table := range []string{"sources", "projects", "items", "comments", "jobs", "automation_log"} {
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
	projects, err := seedProjects(ctx, db, src, []provider.Project{{ExternalID: "o/app", Name: "o/app", URL: "u"}})
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

// Migration 008 (triage) starts jobs fresh again; ids continue after the old
// sequence even when no job row is left. A triage job has no item, one per
// project may be unfinished, and only triage may lack an item.
func TestMigration008TriageJobs(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	if err := migrateTo(ctx, db, 7); err != nil {
		t.Fatal(err)
	}
	s := New(db)
	src, err := s.UpsertSource(ctx, "github", "me")
	if err != nil {
		t.Fatal(err)
	}
	projects, err := seedProjects(ctx, db, src, []provider.Project{{ExternalID: "o/app", Name: "o/app", URL: "u"}, {ExternalID: "o/b", Name: "o/b", URL: "u2"}})
	if err != nil {
		t.Fatal(err)
	}
	pid := projects[0].ID
	if _, err := s.ApplyItems(ctx, src, pid, []provider.Item{item("a", 1, true, t0), item("b", 2, false, t0)}, "me"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO jobs (id, item_id, project_id, flow) SELECT 20, id, project_id, 'fix' FROM items WHERE external_id = 'a'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM jobs`); err != nil {
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
	s = New(db)
	tj, err := s.CreateProjectJob(ctx, pid, FlowTriage, "codex", "", "")
	if err != nil || tj.ID != 21 || tj.ItemID != 0 || tj.ProjectID != pid || tj.Repo != "o/app" || tj.Number != 0 || tj.Origin != OriginManual {
		t.Fatalf("triage job: %+v %v", tj, err)
	}
	if dup, err := s.CreateProjectJob(ctx, pid, FlowTriage, "codex", "", ""); !errors.Is(err, ErrJobExists) || dup.ID != tj.ID {
		t.Fatalf("second triage: %+v %v", dup, err)
	}
	if other, err := s.CreateProjectJob(ctx, projects[1].ID, FlowTriage, "codex", "", ""); err != nil || other.ID == tj.ID {
		t.Fatalf("triage of another project: %+v %v", other, err)
	}
	if _, err := s.CreateProjectJob(ctx, 9999, FlowTriage, "codex", "", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("triage of no project: %v", err)
	}
	if _, err := s.CreateProjectJob(ctx, pid, "fix", "claude", "", ""); err == nil {
		t.Fatal("a fix job without an item was accepted")
	}
	var a int64
	if err := db.QueryRowContext(ctx, `SELECT id FROM items WHERE external_id = 'a'`).Scan(&a); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO jobs (item_id, project_id, flow) VALUES (?, ?, 'triage')`, a, pid); err == nil {
		t.Fatal("a triage job with an item was accepted")
	}
	if fj, err := s.CreateJob(ctx, a, "fix", "claude", "", ""); err != nil || fj.Number != 1 {
		t.Fatalf("fix job: %+v %v", fj, err)
	}
	chunk, err := s.Jobs(ctx, JobFilter{ProjectID: pid})
	if err != nil || len(chunk.Items) != 2 || chunk.Items[1].ID != tj.ID {
		t.Fatalf("project jobs: %+v %v", chunk, err)
	}
	if q, err := s.JobsInState(ctx, JobQueued); err != nil || len(q) != 3 {
		t.Fatalf("queue: %d %v", len(q), err)
	}

	in, err := s.TriageInput(ctx, pid, 10, 5)
	if err != nil || in.ProjectName != "o/app" || len(in.Issues) != 1 || in.Issues[0].Number != 1 || len([]rune(in.Issues[0].Body)) > 5 || in.More {
		t.Fatalf("triage input: %+v %v", in, err)
	}
	if _, err := s.TriageInput(ctx, 9999, 10, 5); !errors.Is(err, ErrNotFound) {
		t.Fatalf("triage input of no project: %v", err)
	}

	// Triage picks are checked against the issues open now; a fix job for a
	// pick is only created while its issue is still open.
	open, err := s.OpenIssues(ctx, pid, []int{1, 2, 7})
	if err != nil || len(open) != 1 || open[1].ItemID != a || open[1].Number != 1 {
		t.Fatalf("open issues: %+v %v", open, err)
	}
	if open, err := s.OpenIssues(ctx, projects[1].ID, []int{1}); err != nil || len(open) != 0 {
		t.Fatalf("open issues of another project: %+v %v", open, err)
	}
	var b int64
	if err := db.QueryRowContext(ctx, `SELECT id FROM items WHERE external_id = 'b'`).Scan(&b); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateOpenJob(ctx, b, "fix", "claude", "", ""); !errors.Is(err, ErrNotOpen) {
		t.Fatalf("fix job for a closed issue: %v", err)
	}
	if _, err := s.CreateOpenJob(ctx, a, "fix", "claude", "", ""); !errors.Is(err, ErrJobExists) {
		t.Fatalf("second fix job: %v", err)
	}
	if _, err := s.CreateOpenJob(ctx, 9999, "fix", "claude", "", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("fix job for no item: %v", err)
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

// seedProjects inserts projects with the columns every schema version has
// (SyncProjects writes columns of the latest migration).
func seedProjects(ctx context.Context, db *sql.DB, sourceID int64, list []provider.Project) ([]Project, error) {
	out := make([]Project, 0, len(list))
	for _, p := range list {
		row := Project{ExternalID: p.ExternalID, Name: p.Name, URL: p.URL}
		if err := db.QueryRowContext(ctx, `INSERT INTO projects (source_id, external_id, name, url, active) VALUES (?, ?, ?, ?, 1) RETURNING id`,
			sourceID, p.ExternalID, p.Name, p.URL).Scan(&row.ID); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, nil
}
