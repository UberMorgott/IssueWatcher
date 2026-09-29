package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// Project groups (owner 2026-09-29): a code project and the mod pages linked
// to it are one project — one row in the project list, one issue scope, one
// triage, one sync. An unlinked mod page is a group of its own.

// Project scopes: "the column is the project (bound placeholder) or one of its
// linked mod pages".
const (
	scopeItemQ   = `i.project_id IN (SELECT ? UNION ALL SELECT mod_project_id FROM project_links WHERE code_project_id = ?)` // id bound twice
	scopeItem1   = `i.project_id IN (SELECT ?1 UNION ALL SELECT mod_project_id FROM project_links WHERE code_project_id = ?1)`
	scopeItem2   = `i.project_id IN (SELECT ?2 UNION ALL SELECT mod_project_id FROM project_links WHERE code_project_id = ?2)`
	scopeProj1   = `p.id IN (SELECT ?1 UNION ALL SELECT mod_project_id FROM project_links WHERE code_project_id = ?1)`
	scopeJobProj = `project_id IN (SELECT ?1 UNION ALL SELECT mod_project_id FROM project_links WHERE code_project_id = ?1)`
)

// groupHead is the project heading id's group: its linked code project, else id.
func groupHead(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, id int64,
) int64 {
	head := id
	_ = q.QueryRowContext(ctx, `SELECT code_project_id FROM project_links WHERE mod_project_id = ?`, id).Scan(&head) // none: id
	return head
}

// groupMembersWith defines m(gid, pid): every active project is a member of
// its own group; an active mod page linked to an active code project is also a
// member of that project's group.
const groupMembersWith = `WITH m(gid, pid) AS (SELECT id, id FROM projects WHERE active = 1
	UNION ALL SELECT pl.code_project_id, pl.mod_project_id FROM project_links pl
	JOIN projects mp ON mp.id = pl.mod_project_id AND mp.active = 1
	JOIN projects cp ON cp.id = pl.code_project_id AND cp.active = 1) `

// groupRows are the active projects that head a group (not a linked mod page);
// ?1 = name LIKE pattern over any member ("" = all). Needs groupMembersWith.
const groupRows = `p.active = 1 AND p.id NOT IN (SELECT pid FROM m WHERE gid <> pid)
	AND (?1 = '' OR EXISTS (SELECT 1 FROM m g JOIN projects x ON x.id = g.pid WHERE g.gid = p.id AND x.name LIKE ?1 ESCAPE '\'))`

// groupedRepos is ReposChunk's inner query in group mode: counts summed over
// the members, last sync = the stalest member's.
const groupedRepos = `SELECT p.id, p.name, p.url, s.platform, s.platform || ':' || p.external_id AS key, p.local_path,
	(SELECT min(x.synced_at) FROM m g JOIN projects x ON x.id = g.pid WHERE g.gid = p.id) AS synced_at, ` + linkColsAs + `,
	count(i.id) FILTER (WHERE i.status = 'open') AS open,
	count(i.id) FILTER (WHERE i.status = 'closed') AS closed,
	count(i.id) FILTER (WHERE i.unread = 1) AS unread
	FROM projects p JOIN sources s ON s.id = p.source_id
	JOIN m ON m.gid = p.id
	LEFT JOIN items i ON i.project_id = m.pid
	WHERE ` + groupRows + ` GROUP BY p.id`

const groupedCount = `SELECT count(*) FROM projects p WHERE ` + groupRows

// Integration is one channel of a grouped project row: the code project itself
// or a linked mod page, with its own counts (Issues filter: project=ID&source=Platform).
type Integration struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	URL      string `json:"url"`
	Platform string `json:"platform"`
	Open     int    `json:"open"`
	Closed   int    `json:"closed"`
	Unread   int    `json:"unread"`
	LastSync string `json:"lastSync"`
}

// fillIntegrations sets rows' Integrations in one query: own project first,
// then the linked mod pages by platform and name.
func (s *Store) fillIntegrations(ctx context.Context, rows []Repo) error {
	if len(rows) == 0 {
		return nil
	}
	ids := make([]int64, len(rows))
	at := make(map[int64]int, len(rows))
	for i, r := range rows {
		ids[i], at[r.ID] = r.ID, i
		rows[i].Integrations = []Integration{}
	}
	b, err := json.Marshal(ids)
	if err != nil {
		return err
	}
	q, err := s.rd.QueryContext(ctx, groupMembersWith+`SELECT m.gid, x.id, x.name, x.url, s.platform, x.synced_at,
		count(i.id) FILTER (WHERE i.status = 'open'),
		count(i.id) FILTER (WHERE i.status = 'closed'),
		count(i.id) FILTER (WHERE i.unread = 1)
		FROM m JOIN projects x ON x.id = m.pid JOIN sources s ON s.id = x.source_id
		LEFT JOIN items i ON i.project_id = x.id
		WHERE m.gid IN (SELECT value FROM json_each(?))
		GROUP BY m.gid, x.id ORDER BY m.gid, x.id <> m.gid, s.platform, x.name COLLATE NOCASE`, string(b))
	if err != nil {
		return fmt.Errorf("store: integrations: %w", err)
	}
	defer func() { _ = q.Close() }()
	for q.Next() {
		var (
			gid int64
			in  Integration
		)
		if err := q.Scan(&gid, &in.ID, &in.Name, &in.URL, &in.Platform, &in.LastSync, &in.Open, &in.Closed, &in.Unread); err != nil {
			return fmt.Errorf("store: scan integration: %w", err)
		}
		if i, ok := at[gid]; ok {
			rows[i].Integrations = append(rows[i].Integrations, in)
		}
	}
	return q.Err()
}

// SyncTarget is one project of a group to sync, with the source that serves it.
type SyncTarget struct {
	Project
	SourceID int64
	Platform string
}

// GroupTargets returns project id and, for a code project, its linked mod
// pages — active ones only, id first. ErrNotFound when id is unknown.
func (s *Store) GroupTargets(ctx context.Context, id int64) ([]SyncTarget, error) {
	if _, err := s.projectPlatform(ctx, s.db, id); err != nil {
		return nil, err
	}
	rows, err := s.rd.QueryContext(ctx, `SELECT p.id, p.external_id, p.name, p.url, p.sync_cursor, p.source_id, s.platform
		FROM projects p JOIN sources s ON s.id = p.source_id
		WHERE p.active = 1 AND `+scopeProj1+` ORDER BY p.id <> ?1, p.id`, id)
	if err != nil {
		return nil, fmt.Errorf("store: group targets: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []SyncTarget{}
	for rows.Next() {
		var (
			t      SyncTarget
			cursor string
		)
		if err := rows.Scan(&t.ID, &t.ExternalID, &t.Name, &t.URL, &cursor, &t.SourceID, &t.Platform); err != nil {
			return nil, fmt.Errorf("store: scan group target: %w", err)
		}
		t.Cursor, _ = time.Parse(timeFormat, cursor)
		out = append(out, t)
	}
	return out, rows.Err()
}
