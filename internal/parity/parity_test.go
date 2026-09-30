package parity

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/provider"
)

type fakeProv struct{ items []provider.Item }

func (f fakeProv) Platform() string                        { return "nexus" }
func (f fakeProv) Capabilities() provider.Capabilities     { return provider.Capabilities{} }
func (f fakeProv) Account(context.Context) (string, error) { return "UberMorgott", nil }
func (f fakeProv) ListProjects(context.Context) ([]provider.Project, error) {
	return nil, nil
}
func (f fakeProv) SyncItems(context.Context, provider.Project, time.Time) ([]provider.Item, error) {
	return f.items, nil
}
func (f fakeProv) Reply(context.Context, string, string) (provider.Comment, error) {
	return provider.Comment{}, nil
}

func fixture(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "copy.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	for _, q := range []string{
		`CREATE TABLE sources (id INTEGER PRIMARY KEY, platform TEXT, account TEXT)`,
		`CREATE TABLE projects (id INTEGER PRIMARY KEY, source_id INTEGER, external_id TEXT, name TEXT, url TEXT, game TEXT DEFAULT '', code_url TEXT DEFAULT '', active INTEGER DEFAULT 1)`,
		`CREATE TABLE items (id INTEGER PRIMARY KEY, project_id INTEGER, external_id TEXT, kind TEXT, author TEXT, body TEXT, status TEXT)`,
		`CREATE TABLE comments (id INTEGER PRIMARY KEY, item_id INTEGER, external_id TEXT, author TEXT, body TEXT, created_at TEXT)`,
		`INSERT INTO sources VALUES (3, 'nexus', 'UberMorgott')`,
		`INSERT INTO projects (id, source_id, external_id, name, url) VALUES (1, 3, 'windrose/147', 'ShareShip', 'https://www.nexusmods.com/windrose/mods/147')`,
		`INSERT INTO items VALUES (1, 1, 'comment:windrose/147/1', 'comment', 'A', 'hello world', 'open')`,
		`INSERT INTO items VALUES (2, 1, 'comment:windrose/147/2', 'comment', 'B', 'gone upstream', 'open')`,
		`INSERT INTO comments VALUES (1, 1, '11', 'UberMorgott', 'thanks', '2026-01-01')`,
		`INSERT INTO comments VALUES (2, 1, '12', 'A', 'line one
line two', '2026-01-02')`,
	} {
		if _, err := db.ExecContext(context.Background(), q); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

func TestRunReportsPlantedDiffs(t *testing.T) {
	snap, err := Open(fixture(t))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = snap.Close() }()
	if acc, _ := snap.Account(context.Background(), "nexus"); acc != "UberMorgott" {
		t.Fatalf("account = %q", acc)
	}
	prov := fakeProv{items: []provider.Item{
		{ExternalID: "comment:windrose/147/1", Kind: "comment", Author: "A", Body: "hello world", Open: true, Comments: []provider.Comment{
			{ExternalID: "11", Author: "UberMorgott", Body: "thanks!"},    // planted body diff
			{ExternalID: "12", Author: "A", Body: "line one\n\nline two"}, // whitespace only
			{ExternalID: "13", Author: "C", Body: "new"},                  // not stored yet
		}},
		{ExternalID: "comment:windrose/147/3", Kind: "comment", Author: "C", Body: "x", Open: true},
	}}
	reps, err := Run(context.Background(), snap, "nexus", prov, "")
	if err != nil || len(reps) != 1 {
		t.Fatalf("run = %v, %v", reps, err)
	}
	got := map[string]string{}
	for _, d := range reps[0].Diffs {
		got[d.Item+"#"+d.Comment] = d.Field
	}
	want := map[string]string{
		"comment:windrose/147/1#11": "body",
		"comment:windrose/147/1#12": "body_whitespace",
		"comment:windrose/147/1#13": "missing_stored",
		"comment:windrose/147/2#":   "missing_native",
		"comment:windrose/147/3#":   "missing_stored",
	}
	if len(got) != len(want) {
		t.Fatalf("diffs = %v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("diff %s = %q, want %q (all %v)", k, got[k], v, got)
		}
	}
	// Same rows → no diff.
	same := fakeProv{items: []provider.Item{
		{ExternalID: "comment:windrose/147/1", Kind: "comment", Author: "A", Body: "hello world", Open: true, Comments: []provider.Comment{
			{ExternalID: "11", Author: "UberMorgott", Body: "thanks"}, {ExternalID: "12", Author: "A", Body: "line one\nline two"}}},
		{ExternalID: "comment:windrose/147/2", Kind: "comment", Author: "B", Body: "gone upstream", Open: true},
	}}
	reps, _ = Run(context.Background(), snap, "nexus", same, "windrose/147")
	if len(reps) != 1 || len(reps[0].Diffs) != 0 {
		t.Fatalf("equal rows: %+v", reps)
	}
}
