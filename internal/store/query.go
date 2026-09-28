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
	Open      int    `json:"open"`
	Closed    int    `json:"closed"`
	Unread    int    `json:"unread"`
	LocalPath string `json:"localPath"` // mapped local folder, "" = none
	LastSync  string `json:"lastSync"`
	SyncedAt  string `json:"syncedAt"` // legacy alias of lastSync
}

// Repos lists active projects with counts, by name.
func (s *Store) Repos(ctx context.Context) ([]Repo, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT p.id, p.name, p.url, s.platform, p.local_path, p.synced_at,
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
		var r Repo
		if err := rows.Scan(&r.ID, &r.Name, &r.URL, &r.Platform, &r.LocalPath, &r.LastSync, &r.Open, &r.Closed, &r.Unread); err != nil {
			return nil, fmt.Errorf("store: scan repo: %w", err)
		}
		r.SyncedAt = r.LastSync
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: repos: %w", err)
	}
	return out, nil
}

// IssueFilter selects issues for the dashboard table.
type IssueFilter struct {
	Platform string // source platform (github, ...), "" = all
	RepoID   int64  // 0 = all
	State    string // open | closed | "" (all)
	Label    string // exact label name
	Text     string // substring of title/body, or #number
	Unread   bool
	Page     int // 1-based
	PerPage  int // default 50, max 200
}

// Issue is a table row.
type Issue struct {
	ID        int64    `json:"id"`
	RepoID    int64    `json:"repoId"`
	Repo      string   `json:"repo"`
	Number    int      `json:"number"`
	Title     string   `json:"title"`
	URL       string   `json:"url"`
	Author    string   `json:"author"`
	State     string   `json:"state"`
	RawStatus string   `json:"rawStatus"`
	Labels    []string `json:"labels"`
	Comments  int      `json:"comments"`
	Unread    bool     `json:"unread"`
	CreatedAt string   `json:"createdAt"`
	UpdatedAt string   `json:"updatedAt"`
	ClosedAt  string   `json:"closedAt"`
}

// IssuePage is one page of issues plus the total match count.
type IssuePage struct {
	Total   int     `json:"total"`
	Page    int     `json:"page"`
	PerPage int     `json:"perPage"`
	Items   []Issue `json:"items"`
}

const issueColumns = `i.id, i.project_id, p.name, i.number, i.title, i.url, i.author, i.status, i.raw_status,
	i.labels, (SELECT count(*) FROM comments c WHERE c.item_id = i.id), i.unread, i.created_at, i.updated_at, i.closed_at`

func scanIssue(sc interface{ Scan(...any) error }) (Issue, error) {
	var (
		is     Issue
		labels string
	)
	err := sc.Scan(&is.ID, &is.RepoID, &is.Repo, &is.Number, &is.Title, &is.URL, &is.Author, &is.State,
		&is.RawStatus, &labels, &is.Comments, &is.Unread, &is.CreatedAt, &is.UpdatedAt, &is.ClosedAt)
	if err != nil {
		return is, err
	}
	if err := json.Unmarshal([]byte(labels), &is.Labels); err != nil || is.Labels == nil {
		is.Labels = []string{}
	}
	return is, nil
}

// Issues returns a filtered page, most recently updated first.
func (s *Store) Issues(ctx context.Context, f IssueFilter) (IssuePage, error) {
	if f.PerPage <= 0 {
		f.PerPage = 50
	}
	f.PerPage = min(f.PerPage, 200)
	f.Page = max(f.Page, 1)

	where := []string{"p.active = 1"}
	var args []any
	if f.RepoID != 0 {
		where, args = append(where, "i.project_id = ?"), append(args, f.RepoID)
	}
	if f.Platform != "" {
		where, args = append(where, "i.source_id IN (SELECT id FROM sources WHERE platform = ?)"), append(args, f.Platform)
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
	from := " FROM items i JOIN projects p ON p.id = i.project_id WHERE " + strings.Join(where, " AND ")

	page := IssuePage{Page: f.Page, PerPage: f.PerPage, Items: []Issue{}}
	if err := s.db.QueryRowContext(ctx, "SELECT count(*)"+from, args...).Scan(&page.Total); err != nil {
		return page, fmt.Errorf("store: count issues: %w", err)
	}
	rows, err := s.db.QueryContext(ctx, "SELECT "+issueColumns+from+" ORDER BY i.updated_at DESC, i.id DESC LIMIT ? OFFSET ?", //nolint:gosec // G202: WHERE built from constant fragments; all values are bound args
		append(args, f.PerPage, (f.Page-1)*f.PerPage)...)
	if err != nil {
		return page, fmt.Errorf("store: issues: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		is, err := scanIssue(rows)
		if err != nil {
			return page, fmt.Errorf("store: scan issue: %w", err)
		}
		page.Items = append(page.Items, is)
	}
	if err := rows.Err(); err != nil {
		return page, fmt.Errorf("store: issues: %w", err)
	}
	return page, nil
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

// IssueDetail is an issue with its body and comments.
type IssueDetail struct {
	Issue
	Body         string    `json:"body"`
	CommentsList []Comment `json:"commentsList"`
}

// Issue returns one issue with comments, oldest first.
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
	d.Issue, d.Body, d.CommentsList = is, body, []Comment{}
	rows, err := s.db.QueryContext(ctx, `SELECT external_id, author, body, url, created_at, updated_at
		FROM comments WHERE item_id = ? ORDER BY created_at, id`, id)
	if err != nil {
		return d, fmt.Errorf("store: comments: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var c Comment
		if err := rows.Scan(&c.ExternalID, &c.Author, &c.Body, &c.URL, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return d, fmt.Errorf("store: scan comment: %w", err)
		}
		d.CommentsList = append(d.CommentsList, c)
	}
	if err := rows.Err(); err != nil {
		return d, fmt.Errorf("store: comments: %w", err)
	}
	return d, nil
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
