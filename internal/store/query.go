package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Repo is a project with issue counts (GET /api/projects).
type Repo struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	URL       string `json:"url"`
	Platform  string `json:"platform"`
	Key       string `json:"key"` // settings key platform:external_id (agents.projects, rules)
	Open      int    `json:"open"`
	Closed    int    `json:"closed"`
	Unread    int    `json:"unread"`
	LocalPath string `json:"localPath"` // mapped local folder, "" = none
	LastSync  string `json:"lastSync"`
	SyncedAt  string `json:"syncedAt"` // legacy alias of lastSync
	// LinkedTo is a mod page's code project id (0 = none); Links a code project's mod page ids.
	LinkedTo int64   `json:"linkedTo,omitempty"`
	Links    []int64 `json:"links,omitempty"`
	// Suggest are code projects an unlinked mod page may belong to (name match,
	// not certain enough to link automatically): one click links it.
	Suggest []int64 `json:"suggest,omitempty"`
}

// Repos lists active projects with counts, by name.
func (s *Store) Repos(ctx context.Context) ([]Repo, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT p.id, p.name, p.url, s.platform, s.platform || ':' || p.external_id, p.local_path, p.synced_at, `+linkColsAs+`,
		count(i.id) FILTER (WHERE i.status = 'open'),
		count(i.id) FILTER (WHERE i.status = 'closed'),
		count(i.id) FILTER (WHERE i.unread = 1)
		FROM projects p JOIN sources s ON s.id = p.source_id
		LEFT JOIN items i ON i.project_id = p.id
		WHERE p.active = 1 GROUP BY p.id ORDER BY p.name COLLATE NOCASE`)
	if err != nil {
		return nil, fmt.Errorf("store: repos: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []Repo{}
	for rows.Next() {
		var (
			r     Repo
			links string
		)
		if err := rows.Scan(&r.ID, &r.Name, &r.URL, &r.Platform, &r.Key, &r.LocalPath, &r.LastSync, &r.LinkedTo, &links, &r.Open, &r.Closed, &r.Unread); err != nil {
			return nil, fmt.Errorf("store: scan repo: %w", err)
		}
		r.Links = parseIDs(links)
		r.SyncedAt = r.LastSync
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: repos: %w", err)
	}
	if err := s.fillSuggestions(ctx, out); err != nil {
		return nil, err
	}
	return out, nil
}

// RepoSorts maps GET /api/projects?sort= to the keyset sort expression.
var RepoSorts = map[string]string{
	"name":     "lower(r.name)",
	"open":     "r.open",
	"closed":   "r.closed",
	"unread":   "r.unread",
	"lastSync": "r.synced_at",
}

// RepoQuery selects a chunk of the project list.
type RepoQuery struct {
	Sort   string // key of RepoSorts, default name
	Desc   bool
	Text   string // name substring
	Cursor string
	Limit  int
}

// RepoChunk is one slice of the project list.
type RepoChunk struct {
	Items      []Repo `json:"items"`
	NextCursor string `json:"nextCursor"`
	More       bool   `json:"more"`
	Total      int    `json:"total"`
}

// ReposChunk returns active projects sorted by q.Sort (id breaks ties), a chunk at a time.
func (s *Store) ReposChunk(ctx context.Context, q RepoQuery) (RepoChunk, error) {
	limit := clampLimit(q.Limit)
	key, ok := RepoSorts[q.Sort]
	if !ok {
		key = RepoSorts["name"]
	}
	cmp, dir := ">", "ASC"
	if q.Desc {
		cmp, dir = "<", "DESC"
	}
	inner := `SELECT p.id, p.name, p.url, s.platform, s.platform || ':' || p.external_id AS key, p.local_path, p.synced_at, ` + linkColsAs + `,
		count(i.id) FILTER (WHERE i.status = 'open') AS open,
		count(i.id) FILTER (WHERE i.status = 'closed') AS closed,
		count(i.id) FILTER (WHERE i.unread = 1) AS unread
		FROM projects p JOIN sources s ON s.id = p.source_id
		LEFT JOIN items i ON i.project_id = p.id
		WHERE p.active = 1 AND (?1 = '' OR p.name LIKE ?1 ESCAPE '\') GROUP BY p.id`
	chunk := RepoChunk{Items: []Repo{}}
	like := ""
	if t := strings.TrimSpace(q.Text); t != "" {
		like = "%" + likeEscape(t) + "%"
	}
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM projects p WHERE p.active = 1 AND (?1 = '' OR p.name LIKE ?1 ESCAPE '\')`, like).Scan(&chunk.Total); err != nil {
		return chunk, fmt.Errorf("store: count repos: %w", err)
	}
	where, args := "", []any{like}
	if q.Cursor != "" {
		k, err := decodeCursor(q.Cursor)
		if err != nil {
			return chunk, err
		}
		where = " WHERE (" + key + " " + cmp + " ?2 OR (" + key + " = ?2 AND r.id " + cmp + " ?3))"
		args = append(args, k.Value, k.ID)
	}
	rows, err := s.db.QueryContext(ctx, "SELECT r.id, r.name, r.url, r.platform, r.key, r.local_path, r.synced_at, r.linked_to, r.links, r.open, r.closed, r.unread, "+key+ // sort key from the fixed RepoSorts map; values are bound args
		" FROM ("+inner+") r"+where+" ORDER BY "+key+" "+dir+", r.id "+dir+" LIMIT ?", append(args, limit+1)...)
	if err != nil {
		return chunk, fmt.Errorf("store: repos: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var sortVal any
	for rows.Next() {
		var (
			r     Repo
			links string
		)
		if err := rows.Scan(&r.ID, &r.Name, &r.URL, &r.Platform, &r.Key, &r.LocalPath, &r.LastSync, &r.LinkedTo, &links, &r.Open, &r.Closed, &r.Unread, &sortVal); err != nil {
			return chunk, fmt.Errorf("store: scan repo: %w", err)
		}
		r.Links = parseIDs(links)
		if len(chunk.Items) == limit {
			chunk.More = true
			break
		}
		r.SyncedAt = r.LastSync
		chunk.Items = append(chunk.Items, r)
		chunk.NextCursor = encodeCursor(sortVal, r.ID)
	}
	if err := rows.Err(); err != nil {
		return chunk, fmt.Errorf("store: repos: %w", err)
	}
	return chunk, nil
}

// IssueFilter selects issues for the dashboard table (keyset pagination: newest
// update first, id breaks ties).
type IssueFilter struct {
	Platform string // source platform (github, ...), "" = all
	Kind     string // item kind (issue | comment | bug), "" = all
	RepoID   int64  // 0 = all
	State    string // open | closed | "" (all)
	Label    string // exact label name
	Text     string // substring of title/body, or #number
	Unread   bool
	IDs      []int64 // only these items, still matching the filter (live patching; max 500)
	Cursor   string  // rows after this one (older): the next chunk
	After    string  // rows before this one (newer): head refresh
	Limit    int     // chunk size, default 50, max 200
}

// Issue is a table row.
type Issue struct {
	ID        int64     `json:"id"`
	RepoID    int64     `json:"repoId"`
	Repo      string    `json:"repo"`
	Number    int       `json:"number"`
	Kind      string    `json:"kind"`     // issue | comment | bug
	Platform  string    `json:"platform"` // the project's platform (github, nexus, curseforge, steam)
	Title     string    `json:"title"`
	URL       string    `json:"url"`
	Author    string    `json:"author"`
	State     string    `json:"state"`
	RawStatus string    `json:"rawStatus"`
	Labels    []string  `json:"labels"`
	Comments  int       `json:"comments"`
	Unread    bool      `json:"unread"`
	CreatedAt string    `json:"createdAt"`
	UpdatedAt string    `json:"updatedAt"`
	ClosedAt  string    `json:"closedAt"`
	Job       *JobBadge `json:"job,omitempty"` // newest agent job of the item
}

// IssueChunk is one slice of the issue list.
type IssueChunk struct {
	Items      []Issue `json:"items"`
	HeadCursor string  `json:"headCursor"`      // cursor of the first row (After for a later head refresh), "" when empty
	NextCursor string  `json:"nextCursor"`      // cursor of the last row (Cursor for the next chunk), "" when empty
	More       bool    `json:"more"`            // Cursor/first chunk: older rows follow; After: more than Limit newer rows
	Total      *int    `json:"total,omitempty"` // rows matching the filter (first chunk and After only)
}

// MaxIDs bounds IssueFilter.IDs.
const MaxIDs = 500
const issueColumns = `i.id, i.project_id, p.name, i.number, i.title, i.url, i.author, i.status, i.raw_status,
	i.labels, (SELECT count(*) FROM comments c WHERE c.item_id = i.id), i.unread, i.created_at, i.updated_at, i.closed_at,
	i.kind, (SELECT platform FROM sources WHERE id = p.source_id)`

func scanIssue(sc interface{ Scan(...any) error }) (Issue, error) {
	var (
		is     Issue
		labels string
	)
	err := sc.Scan(&is.ID, &is.RepoID, &is.Repo, &is.Number, &is.Title, &is.URL, &is.Author, &is.State,
		&is.RawStatus, &labels, &is.Comments, &is.Unread, &is.CreatedAt, &is.UpdatedAt, &is.ClosedAt, &is.Kind, &is.Platform)
	if err != nil {
		return is, err
	}
	if err := json.Unmarshal([]byte(labels), &is.Labels); err != nil || is.Labels == nil {
		is.Labels = []string{}
	}
	return is, nil
}

// Issues returns a chunk of the filtered list, most recently updated first.
func (s *Store) Issues(ctx context.Context, f IssueFilter) (IssueChunk, error) {
	limit := clampLimit(f.Limit)
	where := []string{"p.active = 1"}
	var args []any
	if f.RepoID != 0 {
		where, args = append(where, "i.project_id = ?"), append(args, f.RepoID)
	}
	if f.Platform != "" {
		where, args = append(where, "i.source_id IN (SELECT id FROM sources WHERE platform = ?)"), append(args, f.Platform)
	}
	if f.Kind != "" {
		where, args = append(where, "i.kind = ?"), append(args, f.Kind)
	}
	if f.State == "open" || f.State == "closed" {
		where, args = append(where, "i.status = ?"), append(args, f.State)
	}
	if f.Label != "" {
		where, args = append(where, "EXISTS (SELECT 1 FROM json_each(i.labels) WHERE value = ?)"), append(args, f.Label)
	}
	if f.Unread {
		where = append(where, "i.unread = 1")
	}
	if t := strings.TrimSpace(f.Text); t != "" {
		if n, err := strconv.Atoi(strings.TrimPrefix(t, "#")); err == nil {
			where, args = append(where, "i.number = ?"), append(args, n)
		} else {
			like := "%" + likeEscape(t) + "%"
			where = append(where, `(i.title LIKE ? ESCAPE '\' OR i.body LIKE ? ESCAPE '\')`)
			args = append(args, like, like)
		}
	}
	chunk := IssueChunk{Items: []Issue{}}
	if len(f.IDs) > 0 {
		ids := f.IDs[:min(len(f.IDs), MaxIDs)]
		where = append(where, "i.id IN (?"+strings.Repeat(",?", len(ids)-1)+")")
		for _, id := range ids {
			args = append(args, id)
		}
		limit = len(ids)
	}
	from := " FROM items i JOIN projects p ON p.id = i.project_id WHERE " + strings.Join(where, " AND ")
	if len(f.IDs) == 0 && f.Cursor == "" {
		var n int
		if err := s.db.QueryRowContext(ctx, "SELECT count(*)"+from, args...).Scan(&n); err != nil {
			return chunk, fmt.Errorf("store: count issues: %w", err)
		}
		chunk.Total = &n
	}
	keyset := ""
	switch {
	case f.Cursor != "":
		k, err := decodeCursor(f.Cursor)
		if err != nil {
			return chunk, err
		}
		keyset = " AND (i.updated_at < ? OR (i.updated_at = ? AND i.id < ?))"
		args = append(args, k.Value, k.Value, k.ID)
	case f.After != "":
		k, err := decodeCursor(f.After)
		if err != nil {
			return chunk, err
		}
		keyset = " AND (i.updated_at > ? OR (i.updated_at = ? AND i.id > ?))"
		args = append(args, k.Value, k.Value, k.ID)
	}
	rows, err := s.db.QueryContext(ctx, "SELECT "+issueColumns+from+keyset+" ORDER BY i.updated_at DESC, i.id DESC LIMIT ?", //nolint:gosec // G202: WHERE built from constant fragments; all values are bound args
		append(args, limit+1)...)
	if err != nil {
		return chunk, fmt.Errorf("store: issues: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		is, err := scanIssue(rows)
		if err != nil {
			return chunk, fmt.Errorf("store: scan issue: %w", err)
		}
		chunk.Items = append(chunk.Items, is)
	}
	if err := rows.Err(); err != nil {
		return chunk, fmt.Errorf("store: issues: %w", err)
	}
	if len(chunk.Items) > limit {
		chunk.Items, chunk.More = chunk.Items[:limit], true
	}
	if err := s.attachJobs(ctx, chunk.Items); err != nil {
		return chunk, err
	}
	if n := len(chunk.Items); n > 0 {
		first, last := chunk.Items[0], chunk.Items[n-1]
		chunk.HeadCursor = encodeCursor(first.UpdatedAt, first.ID)
		chunk.NextCursor = encodeCursor(last.UpdatedAt, last.ID)
	}
	return chunk, nil
}

// Comment is a stored comment (API shape).
type Comment struct {
	ExternalID string `json:"id"`
	Author     string `json:"author"`
	Body       string `json:"body"`
	URL        string `json:"url"`
	CreatedAt  string `json:"createdAt"`
	UpdatedAt  string `json:"updatedAt"`
}

// IssueDetail is an issue with its body; comments come in chunks (Comments).
type IssueDetail struct {
	Issue
	Body string `json:"body"`
}

// Issue returns one issue with its body.
func (s *Store) Issue(ctx context.Context, id int64) (IssueDetail, error) {
	var d IssueDetail
	row := s.db.QueryRowContext(ctx, "SELECT "+issueColumns+", i.body FROM items i JOIN projects p ON p.id = i.project_id WHERE i.id = ?", id)
	var body string
	is, err := scanIssue(scanFunc(func(dst ...any) error { return row.Scan(append(dst, &body)...) }))
	if errors.Is(err, sql.ErrNoRows) {
		return d, ErrNotFound
	}
	if err != nil {
		return d, fmt.Errorf("store: issue %d: %w", id, err)
	}
	d.Issue, d.Body = is, body
	one := []Issue{d.Issue}
	if err := s.attachJobs(ctx, one); err != nil {
		return d, err
	}
	d.Issue = one[0]
	return d, nil
}

// attachJobs fills Issue.Job with each item's newest job.
func (s *Store) attachJobs(ctx context.Context, list []Issue) error {
	ids := make([]int64, len(list))
	for i := range list {
		ids[i] = list[i].ID
	}
	jobs, err := s.LatestJobs(ctx, ids)
	if err != nil {
		return err
	}
	for i := range list {
		if b, ok := jobs[list[i].ID]; ok {
			list[i].Job = &b
		}
	}
	return nil
}

// CommentChunk is one slice of an item's comments, oldest first.
type CommentChunk struct {
	Items      []Comment `json:"items"`
	NextCursor string    `json:"nextCursor"` // cursor of the last row; also finds comments added later
	More       bool      `json:"more"`
}

// Comments returns up to limit comments of item id after cursor ("" = from the first).
func (s *Store) Comments(ctx context.Context, id int64, cursor string, limit int) (CommentChunk, error) {
	limit = clampLimit(limit)
	chunk := CommentChunk{Items: []Comment{}}
	q := `SELECT id, external_id, author, body, url, created_at, updated_at FROM comments WHERE item_id = ?`
	args := []any{id}
	if cursor != "" {
		k, err := decodeCursor(cursor)
		if err != nil {
			return chunk, err
		}
		q += ` AND (created_at > ? OR (created_at = ? AND id > ?))`
		args = append(args, k.Value, k.Value, k.ID)
	}
	rows, err := s.db.QueryContext(ctx, q+` ORDER BY created_at, id LIMIT ?`, append(args, limit+1)...)
	if err != nil {
		return chunk, fmt.Errorf("store: comments: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var lastID int64
	for rows.Next() {
		var c Comment
		if err := rows.Scan(&lastID, &c.ExternalID, &c.Author, &c.Body, &c.URL, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return chunk, fmt.Errorf("store: scan comment: %w", err)
		}
		if len(chunk.Items) == limit {
			chunk.More = true
			break
		}
		chunk.Items = append(chunk.Items, c)
		chunk.NextCursor = encodeCursor(c.CreatedAt, lastID)
	}
	if err := rows.Err(); err != nil {
		return chunk, fmt.Errorf("store: comments: %w", err)
	}
	if cursor != "" && len(chunk.Items) == 0 {
		chunk.NextCursor = cursor // nothing new: keep the position
	}
	return chunk, nil
}

type scanFunc func(dst ...any) error

func (f scanFunc) Scan(dst ...any) error { return f(dst...) }

// Week is one bucket of the weekly chart (weeks start Monday, UTC).
type Week struct {
	Start  string `json:"start"` // YYYY-MM-DD
	Opened int    `json:"opened"`
	Closed int    `json:"closed"`
}

// Stats are totals plus a weekly opened/closed series.
type Stats struct {
	Open   int    `json:"open"`
	Closed int    `json:"closed"`
	Weekly []Week `json:"weekly"`
}

// Stats for one repo (repoID != 0) or all active repos, over the last weeks
// weeks ending with the week containing now.
func (s *Store) Stats(ctx context.Context, repoID int64, weeks int, now time.Time) (Stats, error) {
	weeks = min(max(weeks, 1), 520)
	st := Stats{}
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FILTER (WHERE i.status = 'open'),
		count(*) FILTER (WHERE i.status = 'closed')
		FROM items i JOIN projects p ON p.id = i.project_id
		WHERE p.active = 1 AND (?1 = 0 OR i.project_id = ?1)`, repoID).Scan(&st.Open, &st.Closed)
	if err != nil {
		return st, fmt.Errorf("store: stats totals: %w", err)
	}

	// Monday of the current week, then walk back.
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	monday := today.AddDate(0, 0, -((int(today.Weekday()) + 6) % 7))
	first := monday.AddDate(0, 0, -7*(weeks-1))
	idx := map[string]int{}
	for w := range weeks {
		start := first.AddDate(0, 0, 7*w).Format("2006-01-02")
		idx[start] = w
		st.Weekly = append(st.Weekly, Week{Start: start})
	}

	// date(x, '-6 days', 'weekday 1') = Monday on or before x.
	rows, err := s.db.QueryContext(ctx, `SELECT wk, sum(opened), sum(closed) FROM (
		SELECT date(i.created_at, '-6 days', 'weekday 1') AS wk, 1 AS opened, 0 AS closed
		FROM items i JOIN projects p ON p.id = i.project_id
		WHERE p.active = 1 AND (?1 = 0 OR i.project_id = ?1) AND i.created_at >= ?2
		UNION ALL
		SELECT date(i.closed_at, '-6 days', 'weekday 1'), 0, 1
		FROM items i JOIN projects p ON p.id = i.project_id
		WHERE p.active = 1 AND (?1 = 0 OR i.project_id = ?1) AND i.closed_at >= ?2
	) GROUP BY wk`, repoID, first.Format(timeFormat))
	if err != nil {
		return st, fmt.Errorf("store: stats weekly: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			wk             string
			opened, closed int
		)
		if err := rows.Scan(&wk, &opened, &closed); err != nil {
			return st, fmt.Errorf("store: scan week: %w", err)
		}
		if w, ok := idx[wk]; ok {
			st.Weekly[w].Opened, st.Weekly[w].Closed = opened, closed
		}
	}
	if err := rows.Err(); err != nil {
		return st, fmt.Errorf("store: stats weekly: %w", err)
	}
	return st, nil
}
