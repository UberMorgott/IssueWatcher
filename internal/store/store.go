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

	"github.com/UberMorgott/issuewatcher/internal/folders"
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
	db      *sql.DB      // the writer (one connection): writes and transactions
	rd      *sql.DB      // readers: a query_only pool, or db itself (New)
	changes atomic.Int64 // rows sync really changed (items, comments, projects)
}

// Changes is a counter of synced rows that really changed (new, edited, closed,
// new comment, project list). Callers diff two readings to see whether a sync step
// changed what the dashboard shows.
func (s *Store) Changes() int64 { return s.changes.Load() }

// New wraps an opened database (see Open); reads share its connection.
func New(db *sql.DB) *Store { return &Store{db: db, rd: db} }

// NewWithReader wraps the writer db and a reader pool (see OpenReader): reads
// never queue behind a write transaction.
func NewWithReader(db, rd *sql.DB) *Store { return &Store{db: db, rd: rd} }

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
	return s.applyItems(ctx, sourceID, projectID, items, self, nil, time.Time{})
}

// ApplyChecked is ApplyItems for a change check's findings: the items, the
// project's cursor and its poll state (checked at checked) are written in one
// transaction, so a failure leaves all three as they were and the next check
// re-detects the same changes.
func (s *Store) ApplyChecked(ctx context.Context, sourceID, projectID int64, items []provider.Item, self string,
	poll provider.PollState, checked time.Time,
) ([]Event, error) {
	return s.applyItems(ctx, sourceID, projectID, items, self, &poll, checked)
}

func (s *Store) applyItems(ctx context.Context, sourceID, projectID int64, items []provider.Item, self string,
	poll *provider.PollState, checked time.Time,
) ([]Event, error) {
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

	stored, err := loadStored(ctx, tx, sourceID, items)
	if err != nil {
		return nil, err
	}
	var (
		events  []Event
		changes int64
	)
	for i := range items {
		ev, changed, err := applyItem(ctx, tx, sourceID, projectID, pr, &items[i], stored[items[i].ExternalID], self, baseline)
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
	if poll != nil {
		b, err := json.Marshal(poll)
		if err != nil {
			return nil, fmt.Errorf("store: encode poll state: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE projects SET poll_state = ?, checked_at = ? WHERE id = ?`,
			string(b), ts(checked), projectID); err != nil {
			return nil, fmt.Errorf("store: save poll state: %w", err)
		}
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
	_ = s.rd.QueryRowContext(ctx, `SELECT s.platform || ':' || p.external_id FROM project_links pl
		JOIN projects p ON p.id = pl.code_project_id JOIN sources s ON s.id = p.source_id WHERE pl.mod_project_id = ?`, id).Scan(&key)
	return key
}

// projectKeySQL is a project's settings key platform:external_id (config.ProjectKey)
// over projects p JOIN sources s.
const projectKeySQL = `s.platform || ':' || p.external_id`

// projectRef is a project's display name and settings key.
type projectRef struct{ name, key string }

// storedItem is an item's stored row and comments, loaded once per batch.
type storedItem struct {
	id              int64
	fields          [13]any // as the UPDATE binds them
	status, updated string
	unread          bool
	comments        map[string][4]string // external id → author, body, url, updated_at
}

// loadStored reads the stored rows of a batch's items and their comments in
// two queries, so an unchanged item costs no statement at all.
func loadStored(ctx context.Context, tx *sql.Tx, sourceID int64, items []provider.Item) (map[string]*storedItem, error) {
	out := make(map[string]*storedItem, len(items))
	if len(items) == 0 {
		return out, nil
	}
	ids := make([]string, len(items))
	for i := range items {
		ids[i] = items[i].ExternalID
	}
	idsJSON, err := json.Marshal(ids)
	if err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT id, external_id, project_id, kind, number, title, body, url, author, status, raw_status,
		labels, created_at, updated_at, closed_at, unread FROM items WHERE source_id = ? AND external_id IN (SELECT value FROM json_each(?))`,
		sourceID, string(idsJSON))
	if err != nil {
		return nil, fmt.Errorf("store: lookup items: %w", err)
	}
	defer func() { _ = rows.Close() }()
	byID := map[int64]*storedItem{}
	var keys []int64
	for rows.Next() {
		var (
			st                                                                storedItem
			ext, kind, title, body, url, author, raw, labels, created, closed string
			project                                                           int64
			number                                                            int
		)
		if err := rows.Scan(&st.id, &ext, &project, &kind, &number, &title, &body, &url, &author, &st.status, &raw,
			&labels, &created, &st.updated, &closed, &st.unread); err != nil {
			return nil, fmt.Errorf("store: lookup items: %w", err)
		}
		st.fields = [13]any{project, kind, number, title, body, url, author, st.status, raw, labels, created, st.updated, closed}
		st.comments = map[string][4]string{}
		out[ext], byID[st.id] = &st, &st
		keys = append(keys, st.id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: lookup items: %w", err)
	}
	_ = rows.Close()
	if len(keys) == 0 {
		return out, nil
	}
	keysJSON, err := json.Marshal(keys)
	if err != nil {
		return nil, err
	}
	crows, err := tx.QueryContext(ctx, `SELECT item_id, external_id, author, body, url, updated_at FROM comments
		WHERE item_id IN (SELECT value FROM json_each(?))`, string(keysJSON))
	if err != nil {
		return nil, fmt.Errorf("store: lookup comments: %w", err)
	}
	defer func() { _ = crows.Close() }()
	for crows.Next() {
		var (
			item int64
			ext  string
			c    [4]string
		)
		if err := crows.Scan(&item, &ext, &c[0], &c[1], &c[2], &c[3]); err != nil {
			return nil, fmt.Errorf("store: lookup comments: %w", err)
		}
		byID[item].comments[ext] = c
	}
	return out, crows.Err()
}

func applyItem(ctx context.Context, tx *sql.Tx, sourceID, projectID int64, pr projectRef,
	it *provider.Item, st *storedItem, self string, baseline bool,
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
	existed := st != nil
	if existed {
		id, oldStatus, oldUpdated = st.id, st.status, st.updated
	}
	changed = !existed || oldStatus != status || oldUpdated != ts(it.UpdatedAt)
	kind := it.Kind
	if kind == "" {
		kind = KindIssue
	}
	fields := [13]any{projectID, kind, it.Number, it.Title, it.Body, it.URL, it.Author, status, it.RawStatus,
		string(lj), ts(it.CreatedAt), ts(it.UpdatedAt), ts(it.ClosedAt)}
	args := fields[:]
	switch {
	case existed && fields == st.fields:
		// Unchanged row: no write (a full-history source re-reads every item
		// on each reconcile).
	case existed:
		_, err = tx.ExecContext(ctx, `UPDATE items SET project_id = ?, kind = ?, number = ?, title = ?, body = ?,
			url = ?, author = ?, status = ?, raw_status = ?, labels = ?, created_at = ?, updated_at = ?, closed_at = ?
			WHERE id = ?`, append(args, id)...)
	default:
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
		var inserted bool
		if sc, ok := st.comment(c.ExternalID); ok {
			if sc != [4]string{c.Author, c.Body, c.URL, ts(c.UpdatedAt)} {
				if _, err := saveComment(ctx, tx, id, c); err != nil {
					return nil, false, err
				}
			}
		} else if inserted, err = saveComment(ctx, tx, id, c); err != nil {
			return nil, false, err
		}
		changed = changed || inserted
		if inserted && !baseline && existed && c.Author != self {
			e := base
			e.Kind, e.Actor, e.Body = EventNewComment, c.Author, c.Body
			events = append(events, e)
		}
	}
	// A closed item is never unread (counters, badges, the unread filter and the
	// tray all read the flag): closing it, here or by our own push/Fixes seen on
	// sync, marks it read; its events still notify.
	switch {
	case status == "closed" && existed && st.unread:
		if _, err := tx.ExecContext(ctx, `UPDATE items SET unread = 0 WHERE id = ? AND unread = 1`, id); err != nil {
			return nil, false, fmt.Errorf("store: mark read: %w", err)
		}
	case status != "closed" && len(events) > 0:
		if _, err := tx.ExecContext(ctx, `UPDATE items SET unread = 1 WHERE id = ?`, id); err != nil {
			return nil, false, fmt.Errorf("store: mark unread: %w", err)
		}
	}
	return events, changed, nil
}

// comment is a stored comment of the item (none when st is nil).
func (st *storedItem) comment(ext string) ([4]string, bool) {
	if st == nil {
		return [4]string{}, false
	}
	c, ok := st.comments[ext]
	return c, ok
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
	_, err = db.ExecContext(ctx, `UPDATE comments SET author = ?1, body = ?2, url = ?3, updated_at = ?4
		WHERE item_id = ?5 AND external_id = ?6
			AND (author IS NOT ?1 OR body IS NOT ?2 OR url IS NOT ?3 OR updated_at IS NOT ?4)`, c.Author, c.Body, c.URL, ts(c.UpdatedAt), itemID, c.ExternalID)
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
	err := s.rd.QueryRowContext(ctx, `SELECT i.external_id, s.platform, s.id, s.account FROM items i
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
	folders.Invalidate()
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
	if err := s.rd.QueryRowContext(ctx, `SELECT count(*) FROM items WHERE unread = 1`).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: unread count: %w", err)
	}
	return n, nil
}

func likeEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}
