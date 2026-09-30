// Package parity compares a native provider's read with the rows the MCP
// engine stored (Phase 6 owner-data switch): the same item and comment ids,
// authors and bodies, read from a snapshot copy of the owner's database
// opened read-only. Nothing is written and no events are raised.
package parity

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"time"

	_ "modernc.org/sqlite" // database/sql driver "sqlite"

	"github.com/UberMorgott/issuewatcher/internal/provider"
)

// Comment is a stored or read comment.
type Comment struct {
	ExternalID, Author, Body string
}

// Item is a stored or read item.
type Item struct {
	ExternalID, Kind, Author, Body string
	Open                           bool
	Comments                       []Comment
}

// Project is one stored project with its items.
type Project struct {
	Project provider.Project
	Items   []Item
}

// Snapshot is a read-only view of a database copy.
type Snapshot struct {
	db *sql.DB
}

// Open opens the database copy at path read-only.
func Open(path string) (*Snapshot, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(abs), RawQuery: "mode=ro"}
	if !strings.HasPrefix(u.Path, "/") {
		u.Path = "/" + u.Path
	}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	if err := db.PingContext(context.Background()); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("parity: open %s: %w", path, err)
	}
	return &Snapshot{db: db}, nil
}

// Close closes the database.
func (s *Snapshot) Close() error { return s.db.Close() }

// Account is the stored source account of platform ("" = none).
func (s *Snapshot) Account(ctx context.Context, platform string) (string, error) {
	var acc string
	err := s.db.QueryRowContext(ctx, `SELECT account FROM sources WHERE platform = ? ORDER BY id LIMIT 1`, platform).Scan(&acc)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return acc, err
}

// each runs q and calls scan for every row.
func (s *Snapshot) each(ctx context.Context, q string, scan func(*sql.Rows) error, args ...any) error {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		if err := scan(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

// Projects returns platform's active projects with their stored items.
func (s *Snapshot) Projects(ctx context.Context, platform string) ([]Project, error) {
	var ids []int64
	var out []Project
	err := s.each(ctx, `SELECT p.id, p.external_id, p.name, p.url, p.game, p.code_url
		FROM projects p JOIN sources s ON s.id = p.source_id WHERE s.platform = ? AND p.active = 1 ORDER BY p.external_id`,
		func(rows *sql.Rows) error {
			var id int64
			var p provider.Project
			if err := rows.Scan(&id, &p.ExternalID, &p.Name, &p.URL, &p.Game, &p.CodeURL); err != nil {
				return err
			}
			ids = append(ids, id)
			out = append(out, Project{Project: p})
			return nil
		}, platform)
	if err != nil {
		return nil, err
	}
	for i, id := range ids {
		if out[i].Items, err = s.items(ctx, id); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (s *Snapshot) items(ctx context.Context, project int64) ([]Item, error) {
	var ids []int64
	var out []Item
	err := s.each(ctx, `SELECT id, external_id, kind, author, body, status FROM items WHERE project_id = ? ORDER BY external_id`,
		func(rows *sql.Rows) error {
			var id int64
			var it Item
			var status string
			if err := rows.Scan(&id, &it.ExternalID, &it.Kind, &it.Author, &it.Body, &status); err != nil {
				return err
			}
			it.Open = status == "open"
			ids = append(ids, id)
			out = append(out, it)
			return nil
		}, project)
	if err != nil {
		return nil, err
	}
	for i, id := range ids {
		err := s.each(ctx, `SELECT external_id, author, body FROM comments WHERE item_id = ? ORDER BY created_at, id`,
			func(rows *sql.Rows) error {
				var c Comment
				if err := rows.Scan(&c.ExternalID, &c.Author, &c.Body); err != nil {
					return err
				}
				out[i].Comments = append(out[i].Comments, c)
				return nil
			}, id)
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// FromProvider converts a provider read.
func FromProvider(items []provider.Item) []Item {
	out := make([]Item, 0, len(items))
	for _, it := range items {
		kind := it.Kind
		if kind == "" {
			kind = "issue"
		}
		x := Item{ExternalID: it.ExternalID, Kind: kind, Author: it.Author, Body: it.Body, Open: it.Open}
		for _, c := range it.Comments {
			x.Comments = append(x.Comments, Comment{ExternalID: c.ExternalID, Author: c.Author, Body: c.Body})
		}
		out = append(out, x)
	}
	return out
}

// Diff is one difference.
type Diff struct {
	Item    string `json:"item"`
	Comment string `json:"comment,omitempty"`
	Field   string `json:"field"` // missing_native | missing_stored | author | body | body_whitespace | kind | open
	Stored  string `json:"stored,omitempty"`
	Native  string `json:"native,omitempty"`
}

// Report is the comparison of one project.
type Report struct {
	Project       string `json:"project"`
	StoredItems   int    `json:"storedItems"`
	NativeItems   int    `json:"nativeItems"`
	StoredComment int    `json:"storedComments"`
	NativeComment int    `json:"nativeComments"`
	Diffs         []Diff `json:"diffs"`
}

// Compare diffs stored against native rows of one project.
func Compare(project string, stored, native []Item) Report {
	r := Report{Project: project, StoredItems: len(stored), NativeItems: len(native), Diffs: []Diff{}}
	sm := map[string]Item{}
	for _, it := range stored {
		sm[it.ExternalID] = it
		r.StoredComment += len(it.Comments)
	}
	nm := map[string]Item{}
	for _, it := range native {
		nm[it.ExternalID] = it
		r.NativeComment += len(it.Comments)
	}
	for _, id := range sortedKeys(sm, nm) {
		s, sok := sm[id]
		n, nok := nm[id]
		switch {
		case !nok:
			r.Diffs = append(r.Diffs, Diff{Item: id, Field: "missing_native"})
			continue
		case !sok:
			r.Diffs = append(r.Diffs, Diff{Item: id, Field: "missing_stored"})
			continue
		}
		r.Diffs = append(r.Diffs, fields(id, "", s.Author, n.Author, s.Body, n.Body)...)
		if s.Kind != n.Kind {
			r.Diffs = append(r.Diffs, Diff{Item: id, Field: "kind", Stored: s.Kind, Native: n.Kind})
		}
		if s.Open != n.Open {
			r.Diffs = append(r.Diffs, Diff{Item: id, Field: "open", Stored: fmt.Sprint(s.Open), Native: fmt.Sprint(n.Open)})
		}
		sc := map[string]Comment{}
		for _, c := range s.Comments {
			sc[c.ExternalID] = c
		}
		nc := map[string]Comment{}
		for _, c := range n.Comments {
			nc[c.ExternalID] = c
		}
		for _, cid := range sortedKeys(sc, nc) {
			a, aok := sc[cid]
			b, bok := nc[cid]
			switch {
			case !bok:
				r.Diffs = append(r.Diffs, Diff{Item: id, Comment: cid, Field: "missing_native"})
			case !aok:
				r.Diffs = append(r.Diffs, Diff{Item: id, Comment: cid, Field: "missing_stored"})
			default:
				r.Diffs = append(r.Diffs, fields(id, cid, a.Author, b.Author, a.Body, b.Body)...)
			}
		}
	}
	return r
}

func fields(item, comment, sa, na, sb, nb string) []Diff {
	var out []Diff
	if sa != na {
		out = append(out, Diff{Item: item, Comment: comment, Field: "author", Stored: sa, Native: na})
	}
	if sb != nb {
		f := "body"
		if strings.Join(strings.Fields(sb), " ") == strings.Join(strings.Fields(nb), " ") {
			f = "body_whitespace"
		}
		out = append(out, Diff{Item: item, Comment: comment, Field: f, Stored: clip(sb), Native: clip(nb)})
	}
	return out
}

func clip(s string) string {
	if r := []rune(s); len(r) > 160 {
		return string(r[:160]) + "…"
	}
	return s
}

func sortedKeys[V any](a, b map[string]V) []string {
	keys := make([]string, 0, len(a)+len(b))
	for k := range a {
		keys = append(keys, k)
	}
	for k := range b {
		if _, ok := a[k]; !ok {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	return keys
}

// Run reads every stored project of platform with prov and compares.
func Run(ctx context.Context, snap *Snapshot, platform string, prov provider.Provider, only string) ([]Report, error) {
	projects, err := snap.Projects(ctx, platform)
	if err != nil {
		return nil, err
	}
	var out []Report
	for _, p := range projects {
		if only != "" && p.Project.ExternalID != only {
			continue
		}
		items, err := prov.SyncItems(ctx, p.Project, time.Time{})
		if err != nil {
			return out, fmt.Errorf("parity: %s %s: %w", platform, p.Project.ExternalID, err)
		}
		out = append(out, Compare(p.Project.ExternalID, p.Items, FromProvider(items)))
	}
	return out, nil
}
