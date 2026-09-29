package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Project links (migration 009): a mod page (a project of a non-GitHub source)
// linked to the code project (a GitHub repo) that holds its code.

// CodePlatform is the platform of code projects; every other platform's
// projects are mod pages.
const CodePlatform = "github"

// ErrBadLink means a link that is not mod page → code project.
var ErrBadLink = errors.New("store: a mod page can only be linked to a code project (GitHub repository)")

// linkJoin adds the linked code project cp (if any) of project p.
const linkJoin = ` LEFT JOIN project_links pl ON pl.mod_project_id = p.id LEFT JOIN projects cp ON cp.id = pl.code_project_id`

// folderCols are the folder and remote URL a fix of project p's items uses:
// the linked code project's for a mod page, else p's own (needs linkJoin).
const folderCols = `coalesce(cp.local_path, p.local_path), coalesce(cp.url, p.url)`

// LinkRef is one end of a project link.
type LinkRef struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	URL      string `json:"url"`
	Platform string `json:"platform"`
}

// ProjectLinks is a project's link state: a mod page's code project, or a code
// project's mod pages.
type ProjectLinks struct {
	LinkedTo *LinkRef  `json:"linkedTo"`
	Links    []LinkRef `json:"links"`
}

func (s *Store) projectPlatform(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, id int64,
) (string, error) {
	var platform string
	err := q.QueryRowContext(ctx, `SELECT s.platform FROM projects p JOIN sources s ON s.id = p.source_id WHERE p.id = ?`, id).Scan(&platform)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("store: project platform: %w", err)
	}
	return platform, nil
}

// SetProjectLinks makes modIDs the mod pages of code project codeID: links of
// other mod pages to it are removed, a listed mod page linked elsewhere moves here.
func (s *Store) SetProjectLinks(ctx context.Context, codeID int64, modIDs []int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if p, err := s.projectPlatform(ctx, tx, codeID); err != nil {
		return err
	} else if p != CodePlatform {
		return ErrBadLink
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM project_links WHERE code_project_id = ?`, codeID); err != nil {
		return fmt.Errorf("store: clear links: %w", err)
	}
	for _, m := range modIDs {
		if p, err := s.projectPlatform(ctx, tx, m); err != nil {
			return err
		} else if p == CodePlatform {
			return ErrBadLink
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO project_links (mod_project_id, code_project_id) VALUES (?, ?)
			ON CONFLICT (mod_project_id) DO UPDATE SET code_project_id = excluded.code_project_id`, m, codeID); err != nil {
			return fmt.Errorf("store: link %d: %w", m, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit links: %w", err)
	}
	return nil
}

// UnlinkProject removes every link of project id (as mod page or code project).
func (s *Store) UnlinkProject(ctx context.Context, id int64) error {
	if _, err := s.projectPlatform(ctx, s.db, id); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM project_links WHERE mod_project_id = ?1 OR code_project_id = ?1`, id); err != nil {
		return fmt.Errorf("store: unlink: %w", err)
	}
	return nil
}

// Links returns project id's link state.
func (s *Store) Links(ctx context.Context, id int64) (ProjectLinks, error) {
	out := ProjectLinks{Links: []LinkRef{}}
	if _, err := s.projectPlatform(ctx, s.db, id); err != nil {
		return out, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT pl.mod_project_id = ?1, p.id, p.name, p.url, s.platform FROM project_links pl
		JOIN projects p ON p.id = CASE WHEN pl.mod_project_id = ?1 THEN pl.code_project_id ELSE pl.mod_project_id END
		JOIN sources s ON s.id = p.source_id
		WHERE pl.mod_project_id = ?1 OR pl.code_project_id = ?1 ORDER BY p.name COLLATE NOCASE`, id)
	if err != nil {
		return out, fmt.Errorf("store: links: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			isMod bool
			r     LinkRef
		)
		if err := rows.Scan(&isMod, &r.ID, &r.Name, &r.URL, &r.Platform); err != nil {
			return out, fmt.Errorf("store: scan link: %w", err)
		}
		if isMod {
			out.LinkedTo = &r
		} else {
			out.Links = append(out.Links, r)
		}
	}
	return out, rows.Err()
}

// linkColsAs are a project row's link columns: the code project id of a mod page
// (0 = none) and a code project's mod page ids (comma-separated).
const linkColsAs = `coalesce((SELECT code_project_id FROM project_links WHERE mod_project_id = p.id), 0) AS linked_to,
	coalesce((SELECT group_concat(mod_project_id) FROM project_links WHERE code_project_id = p.id), '') AS links`

func parseIDs(s string) []int64 {
	if s == "" {
		return nil
	}
	var out []int64
	for f := range strings.SplitSeq(s, ",") {
		if n, err := strconv.ParseInt(f, 10, 64); err == nil {
			out = append(out, n)
		}
	}
	return out
}
