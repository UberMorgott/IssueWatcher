// Package store owns the SQLite database and its embedded migrations.
package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	_ "modernc.org/sqlite" // pure-Go driver, no cgo
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// Open opens (creating if needed) the database at path and applies pending migrations.
func Open(ctx context.Context, path string) (*sql.DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("store: create dir for %s: %w", path, err)
	}

	dsn := path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open: %w", err)
	}
	// One writer: SQLite serializes writes anyway and the load here is tiny.
	db.SetMaxOpenConns(1)

	if err := migrate(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

// migrate applies migrations newer than PRAGMA user_version, one transaction each.
func migrate(ctx context.Context, db *sql.DB) error {
	entries, err := migrationFiles.ReadDir("migrations")
	if err != nil {
		return fmt.Errorf("store: read migrations: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)

	var current int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&current); err != nil {
		return fmt.Errorf("store: read user_version: %w", err)
	}

	for i, name := range names {
		version := i + 1
		if version <= current {
			continue
		}
		if err := applyMigration(ctx, db, name, version); err != nil {
			return err
		}
	}
	return nil
}

func applyMigration(ctx context.Context, db *sql.DB, name string, version int) error {
	body, err := migrationFiles.ReadFile("migrations/" + name)
	if err != nil {
		return fmt.Errorf("store: read %s: %w", name, err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin %s: %w", name, err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, string(body)); err != nil {
		return fmt.Errorf("store: migration %s: %w", name, err)
	}
	// PRAGMA takes no bind parameters; version is an int we control.
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", version)); err != nil {
		return fmt.Errorf("store: set user_version after %s: %w", name, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit %s: %w", name, err)
	}
	return nil
}
