package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/UberMorgott/issuewatcher/internal/folders"
)

// Project links (migration 009): a mod page (a project of a non-GitHub source)
// linked to the head of its group — the code project (a GitHub repo) that holds
// its code, or (owner 2026-09-30) another mod page: the same mod on several mod
// platforms without a repository. Groups are one level deep: a head is never
// itself linked, and a GitHub project is never a member.

// CodePlatform is the platform of code projects; every other platform's
// projects are mod pages.
const CodePlatform = "github"

// ErrBadLink means a link that is not mod page → group head (a code project or
// an unlinked mod page).
var ErrBadLink = errors.New("store: only a mod page can join a group, headed by a GitHub repository or a mod page that is not in a group itself")

// linkJoin adds the linked code project cp (if any) of project p: the group
// head when it is a GitHub repository (a mod page head has no code folder).
const linkJoin = ` LEFT JOIN project_links pl ON pl.mod_project_id = p.id LEFT JOIN projects cp ON cp.id = pl.code_project_id
	AND cp.source_id IN (SELECT id FROM sources WHERE platform = '` + CodePlatform + `')`

// folderCols are the folder and remote URL a fix of project p's items uses:
// the linked code project's for a mod page, else p's own (needs linkJoin).
const folderCols = `coalesce(cp.local_path, p.local_path), coalesce(cp.url, p.url)`

// needsLinkCol is true for a mod page p with no linked code project (needs linkJoin).
const needsLinkCol = `(cp.id IS NULL AND (SELECT platform FROM sources WHERE id = p.source_id) <> '` + CodePlatform + `')`

// Fix is where fixes of a project's items run — the one answer the UI and the
// runner share: the project's own folder, or a linked mod page's code project's.
type Fix struct {
	// ProjectID is the project whose folder is used (a mod page's code project).
	ProjectID int64 `json:"fixProjectId"`
	// Folder is that project's mapped folder ("" = none).
	Folder string `json:"fixFolder"`
	// Fixable: Folder exists (folders.Status.Exists); a fix runs there.
	Fixable bool `json:"fixable"`
	// Git: Folder is a git clone of that project (folders.StatusOK): the fix
	// commits and can be pushed / opened as a PR. Fixable without Git = the
	// agent edits the folder in place, no commit, no push.
	Git bool `json:"fixGit"`
	// NeedsLink: a mod page without a linked code project.
	NeedsLink bool   `json:"needsLink,omitempty"`
	url       string // remote URL the folder must match
}

// fixTargets returns the Fix of each project in ids, Fixable from the
// folder's status (folders.Cached: a stat and a read of .git/config per
// distinct folder at most every CacheTTL), checked after the rows are closed.
func (s *Store) fixTargets(ctx context.Context, ids []int64) (map[int64]Fix, error) {
	out := map[int64]Fix{}
	if len(ids) == 0 {
		return out, nil
	}
	nums := make([]string, 0, len(ids))
	for _, id := range ids {
		nums = append(nums, strconv.FormatInt(id, 10))
	}
	rows, err := s.rd.QueryContext(ctx, `SELECT p.id, coalesce(cp.id, p.id), `+folderCols+`, `+needsLinkCol+`
		FROM projects p`+linkJoin+` WHERE p.id IN (SELECT value FROM json_each(?))`, "["+strings.Join(nums, ",")+"]")
	if err != nil {
		return nil, fmt.Errorf("store: fix targets: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			id int64
			f  Fix
		)
		if err := rows.Scan(&id, &f.ProjectID, &f.Folder, &f.url, &f.NeedsLink); err != nil {
			return nil, fmt.Errorf("store: scan fix target: %w", err)
		}
		out[id] = f
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: fix targets: %w", err)
	}
	_ = rows.Close() // release the connection before any disk I/O
	for id, f := range out {
		st := folders.Cached(f.Folder, f.url)
		f.Fixable, f.Git = st.Exists(), st == folders.StatusOK
		out[id] = f
	}
	return out, nil
}

// fillRepoFix sets each repo's Fix.
func (s *Store) fillRepoFix(ctx context.Context, list []Repo) error {
	ids := make([]int64, len(list))
	for i := range list {
		ids[i] = list[i].ID
	}
	fx, err := s.fixTargets(ctx, ids)
	if err != nil {
		return err
	}
	for i := range list {
		list[i].Fix = fx[list[i].ID]
	}
	return nil
}

// fillIssueFix sets each issue's Fix (its project's).
func (s *Store) fillIssueFix(ctx context.Context, list []Issue) error {
	seen := map[int64]bool{}
	var ids []int64
	for i := range list {
		if !seen[list[i].RepoID] {
			seen[list[i].RepoID] = true
			ids = append(ids, list[i].RepoID)
		}
	}
	fx, err := s.fixTargets(ctx, ids)
	if err != nil {
		return err
	}
	for i := range list {
		list[i].Fix = fx[list[i].RepoID]
	}
	return nil
}

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

// SetProjectLinks makes modIDs the mod pages of group head codeID (a code
// project, or a mod page not linked itself): links of other mod pages to it
// are removed, a listed mod page linked elsewhere moves here, and a listed mod
// page that heads a group brings its members along (groups stay one level deep).
func (s *Store) SetProjectLinks(ctx context.Context, codeID int64, modIDs []int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := s.projectPlatform(ctx, tx, codeID); err != nil {
		return err
	}
	var member bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM project_links WHERE mod_project_id = ?)`, codeID).Scan(&member); err != nil {
		return fmt.Errorf("store: link head: %w", err)
	}
	if member && len(modIDs) > 0 {
		return ErrBadLink // a member of another group cannot head one
	}
	// Mod pages dropped here and those linked below were decided by hand.
	if _, err := tx.ExecContext(ctx, `INSERT INTO project_link_decisions (mod_project_id)
		SELECT mod_project_id FROM project_links WHERE code_project_id = ? ON CONFLICT DO NOTHING`, codeID); err != nil {
		return fmt.Errorf("store: link decisions: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM project_links WHERE code_project_id = ?`, codeID); err != nil {
		return fmt.Errorf("store: clear links: %w", err)
	}
	for _, m := range modIDs {
		if p, err := s.projectPlatform(ctx, tx, m); err != nil {
			return err
		} else if p == CodePlatform || m == codeID {
			return ErrBadLink
		}
		// A mod page that heads a group: its members join codeID's group too.
		if _, err := tx.ExecContext(ctx, `INSERT INTO project_link_decisions (mod_project_id)
			SELECT mod_project_id FROM project_links WHERE code_project_id = ? ON CONFLICT DO NOTHING`, m); err != nil {
			return fmt.Errorf("store: link decisions: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE project_links SET code_project_id = ? WHERE code_project_id = ?`, codeID, m); err != nil {
			return fmt.Errorf("store: move group %d: %w", m, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO project_links (mod_project_id, code_project_id) VALUES (?, ?)
			ON CONFLICT (mod_project_id) DO UPDATE SET code_project_id = excluded.code_project_id`, m, codeID); err != nil {
			return fmt.Errorf("store: link %d: %w", m, err)
		}
		if err := s.decideLink(ctx, tx, m); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit links: %w", err)
	}
	return nil
}

// UnlinkProject removes every link of project id (as mod page or code project).
func (s *Store) UnlinkProject(ctx context.Context, id int64) error {
	platform, err := s.projectPlatform(ctx, s.db, id)
	if err != nil {
		return err
	}
	// Unlinked by hand: the auto-linker must not link it again.
	decide := `INSERT INTO project_link_decisions (mod_project_id) SELECT mod_project_id FROM project_links WHERE code_project_id = ?1 ON CONFLICT DO NOTHING`
	if platform != CodePlatform {
		decide = `INSERT INTO project_link_decisions (mod_project_id) VALUES (?1) ON CONFLICT DO NOTHING`
	}
	if _, err := s.db.ExecContext(ctx, decide, id); err != nil {
		return fmt.Errorf("store: link decision: %w", err)
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
	rows, err := s.rd.QueryContext(ctx, `SELECT pl.mod_project_id = ?1, p.id, p.name, p.url, s.platform FROM project_links pl
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
