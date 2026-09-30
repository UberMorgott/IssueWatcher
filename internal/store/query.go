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
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	URL      string `json:"url"`
	Platform string `json:"platform"`
	Key      string `json:"key"` // settings key platform:external_id (agents.projects, rules)
	Open     int    `json:"open"`
	Closed   int    `json:"closed"`
	Unread   int    `json:"unread"`
	// UnreadComments / OpenComments: the unread / open items of kind comment (mod page threads); the rest are issues and bug reports.
	UnreadComments int `json:"unreadComments"`
	OpenComments   int `json:"openComments"`
	// ClosedComments: comment threads answered or resolved (flat list only; 0 in the grouped list).
	ClosedComments int    `json:"closedComments"`
	LocalPath      string `json:"localPath"` // mapped local folder, "" = none
	LastSync       string `json:"lastSync"`
	SyncedAt       string `json:"syncedAt"` // legacy alias of lastSync
	// LinkedTo is a mod page's code project id (0 = none); Links a code project's mod page ids.
	LinkedTo int64   `json:"linkedTo,omitempty"`
	Links    []int64 `json:"links,omitempty"`
	// Suggest are code projects an unlinked mod page may belong to (name match,
	// not certain enough to link automatically): one click links it.
	Suggest []int64 `json:"suggest,omitempty"`
	// Integrations (grouped list only): the row's own project first, then its
	// linked mod pages, each with its own counts.
	Integrations []Integration `json:"integrations,omitempty"`
	Fix                        // where fixes of the project's items run (fixProjectId, fixFolder, fixable, needsLink)
}

// Repos lists active projects with counts, by name.
func (s *Store) Repos(ctx context.Context) ([]Repo, error) {
	return s.repos(ctx, `p.active = 1`)
}

// Repo is one active project with counts (ErrNotFound when there is none).
func (s *Store) Repo(ctx context.Context, id int64) (Repo, error) {
	list, err := s.repos(ctx, `p.active = 1 AND p.id = ?`, id)
	if err != nil {
		return Repo{}, err
	}
	if len(list) == 0 {
		return Repo{}, ErrNotFound
	}
	return list[0], nil
}

// ProjectCounts is the number of active projects per platform.
func (s *Store) ProjectCounts(ctx context.Context) (map[string]int, error) {
	rows, err := s.rd.QueryContext(ctx, `SELECT s.platform, count(*) FROM projects p JOIN sources s ON s.id = p.source_id
		WHERE p.active = 1 GROUP BY s.platform`)
	if err != nil {
		return nil, fmt.Errorf("store: project counts: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]int{}
	for rows.Next() {
		var (
			platform string
			n        int
		)
		if err := rows.Scan(&platform, &n); err != nil {
			return nil, fmt.Errorf("store: project counts: %w", err)
		}
		out[platform] = n
	}
	return out, rows.Err()
}

const reposSelect = `SELECT p.id, p.name, p.url, s.platform, s.platform || ':' || p.external_id, p.local_path, p.synced_at, ` + linkColsAs + `,
		count(i.id) FILTER (WHERE i.status = 'open'),
		count(i.id) FILTER (WHERE i.status = 'closed'),
		count(i.id) FILTER (WHERE i.unread = 1),
		count(i.id) FILTER (WHERE i.unread = 1 AND i.kind = 'comment'),
		count(i.id) FILTER (WHERE i.status = 'open' AND i.kind = 'comment'),
		count(i.id) FILTER (WHERE i.status = 'closed' AND i.kind = 'comment')
		FROM projects p JOIN sources s ON s.id = p.source_id
		LEFT JOIN items i ON i.project_id = p.id AND i.hidden = 0
		WHERE `

func (s *Store) repos(ctx context.Context, where string, args ...any) ([]Repo, error) {
	q := reposSelect + where + " GROUP BY p.id ORDER BY p.name COLLATE NOCASE" //nolint:gosec // G202: where is a fixed clause, values bound
	rows, err := s.rd.QueryContext(ctx, q, args...)
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
		if err := rows.Scan(&r.ID, &r.Name, &r.URL, &r.Platform, &r.Key, &r.LocalPath, &r.LastSync, &r.LinkedTo, &links, &r.Open, &r.Closed, &r.Unread, &r.UnreadComments, &r.OpenComments, &r.ClosedComments); err != nil {
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
	if err := s.fillRepoFix(ctx, out); err != nil {
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
	// Group: one row per project (a code project with its linked mod pages
	// folded in: summed counts, Integrations); linked mod pages get no own row.
	Group bool
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
	with, inner := "", `SELECT p.id, p.name, p.url, s.platform, s.platform || ':' || p.external_id AS key, p.local_path, p.synced_at, `+linkColsAs+`,
		count(i.id) FILTER (WHERE i.status = 'open') AS open,
		count(i.id) FILTER (WHERE i.status = 'closed') AS closed,
		count(i.id) FILTER (WHERE i.unread = 1) AS unread,
		count(i.id) FILTER (WHERE i.unread = 1 AND i.kind = 'comment') AS unread_comments,
	count(i.id) FILTER (WHERE i.status = 'open' AND i.kind = 'comment') AS open_comments
		FROM projects p JOIN sources s ON s.id = p.source_id
		LEFT JOIN items i ON i.project_id = p.id AND i.hidden = 0
		WHERE p.active = 1 AND (?1 = '' OR p.name LIKE ?1 ESCAPE '\') GROUP BY p.id`
	count := `SELECT count(*) FROM projects p WHERE p.active = 1 AND (?1 = '' OR p.name LIKE ?1 ESCAPE '\')`
	if q.Group {
		with, inner, count = groupMembersWith, groupedRepos, groupedCount
	}
	chunk := RepoChunk{Items: []Repo{}}
	like := ""
	if t := strings.TrimSpace(q.Text); t != "" {
		like = "%" + likeEscape(t) + "%"
	}
	if err := s.rd.QueryRowContext(ctx, with+count, like).Scan(&chunk.Total); err != nil {
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
	rows, err := s.rd.QueryContext(ctx, with+"SELECT r.id, r.name, r.url, r.platform, r.key, r.local_path, r.synced_at, r.linked_to, r.links, r.open, r.closed, r.unread, r.unread_comments, r.open_comments, "+key+ // sort key from the fixed RepoSorts map; values are bound args
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
		if err := rows.Scan(&r.ID, &r.Name, &r.URL, &r.Platform, &r.Key, &r.LocalPath, &r.LastSync, &r.LinkedTo, &links, &r.Open, &r.Closed, &r.Unread, &r.UnreadComments, &r.OpenComments, &sortVal); err != nil {
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
	_ = rows.Close() // before the next query (the loop may stop early)
	if err := s.fillRepoFix(ctx, chunk.Items); err != nil {
		return chunk, err
	}
	if q.Group {
		if err := s.fillIntegrations(ctx, chunk.Items); err != nil {
			return chunk, err
		}
	}
	return chunk, nil
}

// IssueFilter selects issues for the dashboard table (keyset pagination: newest
// update first, id breaks ties).
type IssueFilter struct {
	Platform string   // source platform (github, ...), "" = all
	Kinds    []string // item kinds (issue | comment | bug), empty = all
	RepoID   int64    // 0 = all; a code project includes its linked mod pages' items
	State    string   // open | closed | resolved | "" (all); a comment thread is open while it waits for an answer
	Label    string   // exact label name
	Text     string   // substring of title/body/replies; a number (#12) also matches the item number
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
	Mine      bool      `json:"mine"`  // written by the synced account (the owner)
	State     string    `json:"state"` // comment threads: open = waiting for an answer, closed = answered or resolved
	Resolved  bool      `json:"resolved"`
	RawStatus string    `json:"rawStatus"`
	Labels    []string  `json:"labels"`
	Comments  int       `json:"comments"`
	Unread    bool      `json:"unread"`
	CreatedAt string    `json:"createdAt"`
	UpdatedAt string    `json:"updatedAt"`
	ClosedAt  string    `json:"closedAt"`
	Job       *JobBadge `json:"job,omitempty"` // newest agent job of the item
	Fix                 // where a fix of the item runs (its project's Fix)
}

// IssueChunk is one slice of the issue list.
type IssueChunk struct {
	Items      []Issue `json:"items"`
	HeadCursor string  `json:"headCursor"`      // cursor of the first row (After for a later head refresh), "" when empty
	NextCursor string  `json:"nextCursor"`      // cursor of the last row (Cursor for the next chunk), "" when empty
	More       bool    `json:"more"`            // Cursor/first chunk: older rows follow; After: more than Limit newer rows
	Total      *int    `json:"total,omitempty"` // rows matching the filter (first chunk and After only)
	// Counts (first chunk and After): open / closed / unread rows matching every
	// filter except state and unread — the list's header counters.
	Counts *IssueCounts `json:"counts,omitempty"`
}

// IssueCounts are the header counters of a filtered issue list.
type IssueCounts struct {
	Open     int `json:"open"`
	Closed   int `json:"closed"`
	Unread   int `json:"unread"`
	Resolved int `json:"resolved"` // comment threads marked «Решено» (part of closed)
}

// MaxIDs bounds IssueFilter.IDs.
const MaxIDs = 500
const issueColumns = `i.id, i.project_id, p.name, i.number, i.title, i.url, i.author, i.status, i.raw_status,
	i.labels, (SELECT count(*) FROM comments c WHERE c.item_id = i.id), i.unread, i.created_at, i.updated_at, i.closed_at,
	i.kind, (SELECT platform FROM sources WHERE id = p.source_id), i.resolved,
	i.author = (SELECT nullif(account, '') FROM sources WHERE id = i.source_id) IS 1`

func scanIssue(sc interface{ Scan(...any) error }) (Issue, error) {
	var (
		is     Issue
		labels string
	)
	err := sc.Scan(&is.ID, &is.RepoID, &is.Repo, &is.Number, &is.Title, &is.URL, &is.Author, &is.State,
		&is.RawStatus, &labels, &is.Comments, &is.Unread, &is.CreatedAt, &is.UpdatedAt, &is.ClosedAt, &is.Kind, &is.Platform,
		&is.Resolved, &is.Mine)
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
	where := []string{"p.active = 1", "i.hidden = 0"}
	var args []any
	if f.RepoID != 0 {
		where, args = append(where, scopeItemQ), append(args, f.RepoID, f.RepoID)
	}
	if f.Platform != "" {
		where, args = append(where, "i.source_id IN (SELECT id FROM sources WHERE platform = ?)"), append(args, f.Platform)
	}
	if len(f.Kinds) > 0 {
		where = append(where, "i.kind IN (?"+strings.Repeat(", ?", len(f.Kinds)-1)+")")
		for _, k := range f.Kinds {
			args = append(args, k)
		}
	}
	if f.Label != "" {
		where, args = append(where, "EXISTS (SELECT 1 FROM json_each(i.labels) WHERE value = ?)"), append(args, f.Label)
	}
	if t := strings.TrimSpace(f.Text); t != "" {
		// Text matches the title, the body and the replies; "12" or "#12" also the item number.
		like := "%" + likeEscape(t) + "%"
		text := `(i.title LIKE ? ESCAPE '\' OR i.body LIKE ? ESCAPE '\' OR EXISTS (SELECT 1 FROM comments c WHERE c.item_id = i.id AND c.body LIKE ? ESCAPE '\'))`
		if n, err := strconv.Atoi(strings.TrimPrefix(t, "#")); err == nil {
			if strings.HasPrefix(t, "#") {
				where, args = append(where, "i.number = ?"), append(args, n)
			} else {
				where, args = append(where, "(i.number = ? OR "+text+")"), append(args, n, like, like, like)
			}
		} else {
			where, args = append(where, text), append(args, like, like, like)
		}
	}
	chunk := IssueChunk{Items: []Issue{}}
	if len(f.IDs) == 0 && f.Cursor == "" { // header counters (first chunk, head refresh): every filter but state and unread
		var n IssueCounts
		if err := s.rd.QueryRowContext(ctx, "SELECT count(*) FILTER (WHERE i.status = 'open'), count(*) FILTER (WHERE i.status = 'closed'), count(*) FILTER (WHERE i.unread = 1),"+
			" count(*) FILTER (WHERE i.resolved = 1) FROM items i JOIN projects p ON p.id = i.project_id WHERE "+strings.Join(where, " AND "), args...).
			Scan(&n.Open, &n.Closed, &n.Unread, &n.Resolved); err != nil {
			return chunk, fmt.Errorf("store: count issues: %w", err)
		}
		chunk.Counts = &n
	}
	switch f.State {
	case "open", "closed":
		where, args = append(where, "i.status = ?"), append(args, f.State)
	case "resolved":
		where = append(where, "i.resolved = 1")
	}
	if f.Unread {
		where = append(where, "i.unread = 1")
	}
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
		if err := s.rd.QueryRowContext(ctx, "SELECT count(*)"+from, args...).Scan(&n); err != nil {
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
	rows, err := s.rd.QueryContext(ctx, "SELECT "+issueColumns+from+keyset+" ORDER BY i.updated_at DESC, i.id DESC LIMIT ?", //nolint:gosec // G202: WHERE built from constant fragments; all values are bound args
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
	if err := s.fillIssueFix(ctx, chunk.Items); err != nil {
		return chunk, err
	}
	if n := len(chunk.Items); n > 0 {
		first, last := chunk.Items[0], chunk.Items[n-1]
		chunk.HeadCursor = encodeCursor(first.UpdatedAt, first.ID)
		chunk.NextCursor = encodeCursor(last.UpdatedAt, last.ID)
	}
	return chunk, nil
}

// ItemLabels lists the distinct labels of active projects' items of kinds
// (empty = all), by name: the list's label filter offers every label, not only
// those of the loaded rows.
func (s *Store) ItemLabels(ctx context.Context, kinds []string) ([]string, error) {
	q := `SELECT DISTINCT l.value FROM items i JOIN projects p ON p.id = i.project_id, json_each(i.labels) l
		WHERE p.active = 1 AND i.hidden = 0`
	args := make([]any, 0, len(kinds))
	if len(kinds) > 0 {
		q += " AND i.kind IN (?" + strings.Repeat(", ?", len(kinds)-1) + ")"
		for _, k := range kinds {
			args = append(args, k)
		}
	}
	rows, err := s.rd.QueryContext(ctx, q+" ORDER BY l.value COLLATE NOCASE", args...)
	if err != nil {
		return nil, fmt.Errorf("store: item labels: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []string{}
	for rows.Next() {
		var l string
		if err := rows.Scan(&l); err != nil {
			return nil, fmt.Errorf("store: item labels: %w", err)
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// Comment is a stored comment (API shape).
type Comment struct {
	ExternalID string `json:"id"`
	Author     string `json:"author"`
	Mine       bool   `json:"mine"` // written by the synced account (the owner)
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
	row := s.rd.QueryRowContext(ctx, "SELECT "+issueColumns+", i.body FROM items i JOIN projects p ON p.id = i.project_id WHERE i.id = ?", id)
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
	if err := s.fillIssueFix(ctx, one); err != nil {
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
	q := `SELECT id, external_id, author, author = (SELECT nullif(s.account, '') FROM items it JOIN sources s ON s.id = it.source_id WHERE it.id = ?1) IS 1,
		body, url, created_at, updated_at FROM comments WHERE item_id = ?1`
	args := []any{id}
	if cursor != "" {
		k, err := decodeCursor(cursor)
		if err != nil {
			return chunk, err
		}
		q += ` AND (created_at > ? OR (created_at = ? AND id > ?))`
		args = append(args, k.Value, k.Value, k.ID)
	}
	rows, err := s.rd.QueryContext(ctx, q+` ORDER BY created_at, id LIMIT ?`, append(args, limit+1)...)
	if err != nil {
		return chunk, fmt.Errorf("store: comments: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var lastID int64
	for rows.Next() {
		var c Comment
		if err := rows.Scan(&lastID, &c.ExternalID, &c.Author, &c.Mine, &c.Body, &c.URL, &c.CreatedAt, &c.UpdatedAt); err != nil {
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

// Week is one bucket of the weekly chart (weeks start Monday, UTC): issues
// and bug reports opened / closed, and new comment threads (their own series).
type Week struct {
	Start    string `json:"start"` // YYYY-MM-DD
	Opened   int    `json:"opened"`
	Closed   int    `json:"closed"`
	Comments int    `json:"comments"`
}

// Stats are totals plus a weekly series. Open / Closed count issues and bug
// reports only; comment threads are counted apart (OpenComments = waiting for
// an answer, Comments = all).
type Stats struct {
	Open         int    `json:"open"`
	Closed       int    `json:"closed"`
	OpenComments int    `json:"openComments"`
	Comments     int    `json:"comments"`
	Weekly       []Week `json:"weekly"`
}

// Stats for one repo (repoID != 0) or all active repos, over the last weeks
// weeks ending with the week containing now.
func (s *Store) Stats(ctx context.Context, repoID int64, weeks int, now time.Time) (Stats, error) {
	weeks = min(max(weeks, 1), 520)
	st := Stats{}
	err := s.rd.QueryRowContext(ctx, `SELECT count(*) FILTER (WHERE i.status = 'open' AND i.kind <> 'comment'),
		count(*) FILTER (WHERE i.status = 'closed' AND i.kind <> 'comment'),
		count(*) FILTER (WHERE i.status = 'open' AND i.kind = 'comment'),
		count(*) FILTER (WHERE i.kind = 'comment')
		FROM items i JOIN projects p ON p.id = i.project_id
		WHERE p.active = 1 AND i.hidden = 0 AND (?1 = 0 OR `+scopeItem1+`)`, repoID).Scan(&st.Open, &st.Closed, &st.OpenComments, &st.Comments)
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
	rows, err := s.rd.QueryContext(ctx, `SELECT wk, sum(opened), sum(closed), sum(comments) FROM (
		SELECT date(i.created_at, '-6 days', 'weekday 1') AS wk, i.kind <> 'comment' AS opened, 0 AS closed, i.kind = 'comment' AS comments
		FROM items i JOIN projects p ON p.id = i.project_id
		WHERE p.active = 1 AND i.hidden = 0 AND (?1 = 0 OR `+scopeItem1+`) AND i.created_at >= ?2
		UNION ALL
		SELECT date(i.closed_at, '-6 days', 'weekday 1'), 0, 1, 0
		FROM items i JOIN projects p ON p.id = i.project_id
		WHERE p.active = 1 AND i.kind <> 'comment' AND (?1 = 0 OR `+scopeItem1+`) AND i.closed_at >= ?2
	) GROUP BY wk`, repoID, first.Format(timeFormat))
	if err != nil {
		return st, fmt.Errorf("store: stats weekly: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			wk                       string
			opened, closed, comments int
		)
		if err := rows.Scan(&wk, &opened, &closed, &comments); err != nil {
			return st, fmt.Errorf("store: scan week: %w", err)
		}
		if w, ok := idx[wk]; ok {
			st.Weekly[w].Opened, st.Weekly[w].Closed, st.Weekly[w].Comments = opened, closed, comments
		}
	}
	if err := rows.Err(); err != nil {
		return st, fmt.Errorf("store: stats weekly: %w", err)
	}
	return st, nil
}
