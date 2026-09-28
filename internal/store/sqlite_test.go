package store

import (
	"context"
	"path/filepath"
	"testing"
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
	if version != 4 {
		t.Fatalf("user_version = %d, want 4", version)
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
