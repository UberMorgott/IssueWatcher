package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/provider"
)

// ErrNotFound is returned when a requested row does not exist.
var ErrNotFound = errors.New("store: not found")

const timeFormat = "2006-01-02T15:04:05Z"

func ts(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(timeFormat)
}

// Store wraps the database with the queries sync and the API need.
type Store struct {
	db      *sql.DB
	changes atomic.Int64 // rows sync really changed (items, comments, projects)
}

// Changes is a counter of synced rows that really changed (new, edited, closed,
// new comment, project list). Callers diff two readings to see whether a sync step
// changed what the dashboard shows.
func (s *Store) Changes() int64 { return s.changes.Load() }

// New wraps an opened database (see Open).
func New(db *sql.DB) *Store { return &Store{db: db} }

// EventKind is a change detected by sync.
type EventKind string

// Event kinds that trigger notifications.
const (
	EventNewIssue   EventKind = "new_issue"
	EventNewComment EventKind = "new_comment"
	EventClosed     EventKind = "issue_closed"
	// EventNewItem is a new item of a kind other than issue (a mod page's
	// comment thread or bug report); Event.ItemKind tells which.
	EventNewItem EventKind = "new_item"
)

// Item kinds (items.kind): GitHub issues, mod-page comment threads, mod bug reports.
const (
	KindIssue   = "issue"
	KindComment = "comment"
	KindBug     = "bug"
)

// Event is one notification-worthy change.
type Event struct {
	Kind     EventKind
	ItemKind string // issue | comment | bug
	ItemID   int64
	Project  string // settings key platform:external_id (agents.projects, rules)
	// CodeProject is the settings key of the code project a mod page is linked
	// to ("" = none): that project's rules and automation policy cover the item.
	CodeProject string
	Repo        string
	Number      int
	Title       string
	Actor       string // issue/comment author; "" for closes
	Body        string // comment text for new_comment
}

// UpsertSource returns the id of (platform, account), creating it if needed.
func (s *Store) UpsertSource(ctx context.Context, platform, account string) (int64, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, `INSERT INTO sources (platform, account) VALUES (?, ?)
		ON CONFLICT (platform, account) DO UPDATE SET account = excluded.account RETURNING id`,
		platform, account).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("store: upsert source: %w", err)
	}
	return id, nil
}

// Project is a synced project row.
type Project struct {
	ID         int64
	ExternalID string
	Name       string
	URL        string
	Cursor     time.Time // zero = full sync
}

// SyncProjects upserts the reachable projects of a source, deactivates the
// ones no longer listed and returns the active set.
func (s *Store) SyncProjects(ctx context.Context, sourceID int64, list []provider.Project) ([]Project, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("store: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	before, err := activeProjects(ctx, tx, sourceID)
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE projects SET active = 0 WHERE source_id = ?`, sourceID); err != nil {
		return nil, fmt.Errorf("store: deactivate projects: %w", err)
	}
	out := make([]Project, 0, len(list))
	changed := len(before) != len(list)
	for _, p := range list {
		if before[p.ExternalID] != p.Name+"\x00"+p.URL {
			changed = true
		}
		var (
			row    = Project{ExternalID: p.ExternalID, Name: p.Name, URL: p.URL}
			cursor string
		)
		err := tx.QueryRowContext(ctx, `INSERT INTO projects (source_id, external_id, name, url, game, code_url, active)
			VALUES (?, ?, ?, ?, ?, ?, 1)
			ON CONFLICT (source_id, external_id) DO UPDATE SET name = excluded.name, url = excluded.url,
				game = coalesce(nullif(excluded.game, ''), game), code_url = coalesce(nullif(excluded.code_url, ''), code_url), active = 1
			RETURNING id, sync_cursor`, sourceID, p.ExternalID, p.Name, p.URL, p.Game, p.CodeURL).Scan(&row.ID, &cursor)
		if err != nil {
			return nil, fmt.Errorf("store: upsert project %s: %w", p.ExternalID, err)
		}
		row.Cursor, _ = time.Parse(timeFormat, cursor)
		out = append(out, row)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("store: commit projects: %w", err)
	}
	if changed {
		s.changes.Add(1)
	}
	return out, nil
}

// activeProjects maps external id → name+"\x00"+url of the active projects of a source.
func activeProjects(ctx context.Context, tx *sql.Tx, sourceID int64) (map[string]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT external_id, name, url FROM projects WHERE source_id = ? AND active = 1`, sourceID)
	if err != nil {
		return nil, fmt.Errorf("store: active projects: %w", err)
	}
	defer func() { _ = rows.Close() }()
	m := map[string]string{}
	for rows.Next() {
		var id, name, url string
		if err := rows.Scan(&id, &name, &url); err != nil {
			return nil, fmt.Errorf("store: active projects: %w", err)
		}
		m[id] = name + "\x00" + url
	}
	return m, rows.Err()
}

// ApplyItems stores a sync batch for one project and returns the changes worth
// notifying about. The first sync of a project is a silent baseline. Items and
// comments authored by self never notify.
func (s *Store) ApplyItems(ctx context.Context, sourceID, projectID int64, items []provider.Item, self string) ([]Event, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("store: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var pr projectRef
	var cursor, syncedAt string
	err = tx.QueryRowContext(ctx, `SELECT p.name, `+projectKeySQL+`, p.sync_cursor, p.synced_at
		FROM projects p JOIN sources s ON s.id = p.source_id WHERE p.id = ?`, projectID).
		Scan(&pr.name, &pr.key, &cursor, &syncedAt)
	if err != nil {
		return nil, fmt.Errorf("store: project %d: %w", projectID, err)
	}
	baseline := syncedAt == ""

	var (
		events  []Event
		changes int64
	)
	for i := range items {
		ev, changed, err := applyItem(ctx, tx, sourceID, projectID, pr, &items[i], self, baseline)
		if err != nil {
			return nil, err
		}
		if changed {
			changes++
		}
		events = append(events, ev...)
		if u := ts(items[i].UpdatedAt); u > cursor {
			cursor = u
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE projects SET sync_cursor = ?, synced_at = ? WHERE id = ?`,
		cursor, ts(time.Now()), projectID); err != nil {
		return nil, fmt.Errorf("store: update cursor: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("store: commit items: %w", err)
	}
	s.changes.Add(changes)
	if len(events) > 0 {
		code := s.linkedCodeKey(ctx, projectID)
		for i := range events {
			events[i].CodeProject = code
		}
	}
	return events, nil
}

// linkedCodeKey is the settings key of the code project mod page id is linked
// to; "" when none (or on a schema before project links).
func (s *Store) linkedCodeKey(ctx context.Context, id int64) string {
	var key string
	_ = s.db.QueryRowContext(ctx, `SELECT s.platform || ':' || p.external_id FROM project_links pl
		JOIN projects p ON p.id = pl.code_project_id JOIN sources s ON s.id = p.source_id WHERE pl.mod_project_id = ?`, id).Scan(&key)
	return key
}

// projectKeySQL is a project's settings key platform:external_id (config.ProjectKey)
// over projects p JOIN sources s.
const projectKeySQL = `s.platform || ':' || p.external_id`

// projectRef is a project's display name and settings key.
type projectRef struct{ name, key string }

func applyItem(ctx context.Context, tx *sql.Tx, sourceID, projectID int64, pr projectRef,
	it *provider.Item, self string, baseline bool,
) (events []Event, changed bool, err error) {
	status := "closed"
	if it.Open {
		status = "open"
	}
	labels := it.Labels
	if labels == nil {
		labels = []string{}
	}
	lj, err := json.Marshal(labels)
	if err != nil {
		return nil, false, err
	}

	var (
		id         int64
		oldStatus  string
		oldUpdated string
	)
	err = tx.QueryRowContext(ctx, `SELECT id, status, updated_at FROM items WHERE source_id = ? AND external_id = ?`,
		sourceID, it.ExternalID).Scan(&id, &oldStatus, &oldUpdated)
	existed := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, false, fmt.Errorf("store: lookup item: %w", err)
	}
	changed = !existed || oldStatus != status || oldUpdated != ts(it.UpdatedAt)
	kind := it.Kind
	if kind == "" {
		kind = KindIssue
	}
	args := []any{projectID, kind, it.Number, it.Title, it.Body, it.URL, it.Author, status, it.RawStatus,
		string(lj), ts(it.CreatedAt), ts(it.UpdatedAt), ts(it.ClosedAt)}
	if existed {
		_, err = tx.ExecContext(ctx, `UPDATE items SET project_id = ?, kind = ?, number = ?, title = ?, body = ?,
			url = ?, author = ?, status = ?, raw_status = ?, labels = ?, created_at = ?, updated_at = ?, closed_at = ?
			WHERE id = ?`, append(args, id)...)
	} else {
		err = tx.QueryRowContext(ctx, `INSERT INTO items (project_id, kind, number, title, body, url, author,
			status, raw_status, labels, created_at, updated_at, closed_at, source_id, external_id)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) RETURNING id`,
			append(args, sourceID, it.ExternalID)...).Scan(&id)
	}
	if err != nil {
		return nil, false, fmt.Errorf("store: save item %s: %w", it.ExternalID, err)
	}

	base := Event{ItemID: id, ItemKind: kind, Project: pr.key, Repo: pr.name, Number: it.Number, Title: it.Title}
	if !baseline && !existed && it.Author != self {
		e := base
		e.Kind, e.Actor = EventNewIssue, it.Author
		if kind != KindIssue {
			e.Kind = EventNewItem
		}
		events = append(events, e)
	}
	if !baseline && existed && oldStatus == "open" && status == "closed" {
		e := base
		e.Kind = EventClosed
		events = append(events, e)
	}
	for _, c := range it.Comments {
		inserted, err := saveComment(ctx, tx, id, c)
		if err != nil {
			return nil, false, err
		}
		changed = changed || inserted
		if inserted && !baseline && existed && c.Author != self {
			e := base
			e.Kind, e.Actor, e.Body = EventNewComment, c.Author, c.Body
			events = append(events, e)
		}
	}
	if len(events) > 0 {
		if _, err := tx.ExecContext(ctx, `UPDATE items SET unread = 1 WHERE id = ?`, id); err != nil {
			return nil, false, fmt.Errorf("store: mark unread: %w", err)
		}
	}
	return events, changed, nil
}

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// saveComment inserts or updates a comment; reports whether it was new.
func saveComment(ctx context.Context, db execer, itemID int64, c provider.Comment) (bool, error) {
	res, err := db.ExecContext(ctx, `INSERT INTO comments (item_id, external_id, author, body, url, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?) ON CONFLICT (item_id, external_id) DO NOTHING`,
		itemID, c.ExternalID, c.Author, c.Body, c.URL, ts(c.CreatedAt), ts(c.UpdatedAt))
	if err != nil {
		return false, fmt.Errorf("store: insert comment: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 1 {
		return true, nil
	}
	_, err = db.ExecContext(ctx, `UPDATE comments SET author = ?, body = ?, url = ?, updated_at = ?
		WHERE item_id = ? AND external_id = ?`, c.Author, c.Body, c.URL, ts(c.UpdatedAt), itemID, c.ExternalID)
	if err != nil {
		return false, fmt.Errorf("store: update comment: %w", err)
	}
	return false, nil
}

// ItemRef identifies an item on its platform.
type ItemRef struct {
	ID         int64
	ExternalID string
	Platform   string
	SourceID   int64  // the account the item was synced from
	Account    string // that account's login
}

// ItemRef looks up the platform identity of item id.
func (s *Store) ItemRef(ctx context.Context, id int64) (ItemRef, error) {
	ref := ItemRef{ID: id}
	err := s.db.QueryRowContext(ctx, `SELECT i.external_id, s.platform, s.id, s.account FROM items i
		JOIN sources s ON s.id = i.source_id WHERE i.id = ?`, id).Scan(&ref.ExternalID, &ref.Platform, &ref.SourceID, &ref.Account)
	if errors.Is(err, sql.ErrNoRows) {
		return ref, ErrNotFound
	}
	if err != nil {
		return ref, fmt.Errorf("store: item ref: %w", err)
	}
	return ref, nil
}

// AddComment stores a comment the user just posted.
func (s *Store) AddComment(ctx context.Context, itemID int64, c provider.Comment) (Comment, error) {
	if _, err := saveComment(ctx, s.db, itemID, c); err != nil {
		return Comment{}, err
	}
	return Comment{
		ExternalID: c.ExternalID, Author: c.Author, Body: c.Body, URL: c.URL,
		CreatedAt: ts(c.CreatedAt), UpdatedAt: ts(c.UpdatedAt),
	}, nil
}

// SetLocalPath maps project id to a local folder ("" = unmap).
func (s *Store) SetLocalPath(ctx context.Context, id int64, path string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE projects SET local_path = ? WHERE id = ?`, path, id)
	if err != nil {
		return fmt.Errorf("store: set local path: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// MarkRead clears the unread flag of item id.
func (s *Store) MarkRead(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `UPDATE items SET unread = 0 WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: mark read: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// UnreadCount is the tray badge: items with unseen activity.
func (s *Store) UnreadCount(ctx context.Context) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM items WHERE unread = 1`).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: unread count: %w", err)
	}
	return n, nil
}

func likeEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}
