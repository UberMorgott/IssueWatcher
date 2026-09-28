package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
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
type Store struct{ db *sql.DB }

// New wraps an opened database (see Open).
func New(db *sql.DB) *Store { return &Store{db: db} }

// EventKind is a change detected by sync.
type EventKind string

// Event kinds that trigger notifications.
const (
	EventNewIssue   EventKind = "new_issue"
	EventNewComment EventKind = "new_comment"
	EventClosed     EventKind = "issue_closed"
)

// Event is one notification-worthy change.
type Event struct {
	Kind   EventKind
	ItemID int64
	Repo   string
	Number int
	Title  string
	Actor  string // issue/comment author; "" for closes
	Body   string // comment text for new_comment
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
	if _, err := tx.ExecContext(ctx, `UPDATE projects SET active = 0 WHERE source_id = ?`, sourceID); err != nil {
		return nil, fmt.Errorf("store: deactivate projects: %w", err)
	}
	out := make([]Project, 0, len(list))
	for _, p := range list {
		var (
			row    = Project{ExternalID: p.ExternalID, Name: p.Name, URL: p.URL}
			cursor string
		)
		err := tx.QueryRowContext(ctx, `INSERT INTO projects (source_id, external_id, name, url, active)
			VALUES (?, ?, ?, ?, 1)
			ON CONFLICT (source_id, external_id) DO UPDATE SET name = excluded.name, url = excluded.url, active = 1
			RETURNING id, sync_cursor`, sourceID, p.ExternalID, p.Name, p.URL).Scan(&row.ID, &cursor)
		if err != nil {
			return nil, fmt.Errorf("store: upsert project %s: %w", p.ExternalID, err)
		}
		row.Cursor, _ = time.Parse(timeFormat, cursor)
		out = append(out, row)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("store: commit projects: %w", err)
	}
	return out, nil
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

	var repo, cursor, syncedAt string
	err = tx.QueryRowContext(ctx, `SELECT name, sync_cursor, synced_at FROM projects WHERE id = ?`, projectID).
		Scan(&repo, &cursor, &syncedAt)
	if err != nil {
		return nil, fmt.Errorf("store: project %d: %w", projectID, err)
	}
	baseline := syncedAt == ""

	var events []Event
	for i := range items {
		ev, err := applyItem(ctx, tx, sourceID, projectID, repo, &items[i], self, baseline)
		if err != nil {
			return nil, err
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
	return events, nil
}

func applyItem(ctx context.Context, tx *sql.Tx, sourceID, projectID int64, repo string,
	it *provider.Item, self string, baseline bool,
) ([]Event, error) {
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
		return nil, err
	}

	var (
		id        int64
		oldStatus string
	)
	err = tx.QueryRowContext(ctx, `SELECT id, status FROM items WHERE source_id = ? AND external_id = ?`,
		sourceID, it.ExternalID).Scan(&id, &oldStatus)
	existed := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("store: lookup item: %w", err)
	}
	args := []any{projectID, it.Kind, it.Number, it.Title, it.Body, it.URL, it.Author, status, it.RawStatus,
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
		return nil, fmt.Errorf("store: save item %s: %w", it.ExternalID, err)
	}

	base := Event{ItemID: id, Repo: repo, Number: it.Number, Title: it.Title}
	var events []Event
	if !baseline && !existed && it.Author != self {
		e := base
		e.Kind, e.Actor = EventNewIssue, it.Author
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
			return nil, err
		}
		if inserted && !baseline && existed && c.Author != self {
			e := base
			e.Kind, e.Actor, e.Body = EventNewComment, c.Author, c.Body
			events = append(events, e)
		}
	}
	if len(events) > 0 {
		if _, err := tx.ExecContext(ctx, `UPDATE items SET unread = 1 WHERE id = ?`, id); err != nil {
			return nil, fmt.Errorf("store: mark unread: %w", err)
		}
	}
	return events, nil
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
}

// ItemRef looks up the platform identity of item id.
func (s *Store) ItemRef(ctx context.Context, id int64) (ItemRef, error) {
	ref := ItemRef{ID: id}
	err := s.db.QueryRowContext(ctx, `SELECT i.external_id, s.platform FROM items i
		JOIN sources s ON s.id = i.source_id WHERE i.id = ?`, id).Scan(&ref.ExternalID, &ref.Platform)
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
