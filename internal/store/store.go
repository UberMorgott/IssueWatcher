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
	// inbox decides, per code project settings key, whether sync events go to
	// the autopilot inbox (nil = never); see SetInboxFilter.
	inbox atomic.Pointer[func(codeKey string) bool]
}

// SetInboxFilter makes sync write its new_issue / new_comment / new_item
// events into autopilot_inbox, in the sync's own transaction, when accept
// reports true for the item's code project key (a mod page's linked code
// project, else the project itself). nil turns the inbox off.
func (s *Store) SetInboxFilter(accept func(codeKey string) bool) {
	if accept == nil {
		s.inbox.Store(nil)
		return
	}
	s.inbox.Store(&accept)
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
	// SourceEvent identifies the change on its platform (platform:item external
	// id[:comment external id]): the autopilot inbox's UNIQUE key.
	SourceEvent string
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

// AddProject upserts one active project of a source without touching the
// others (a project the app just created on its platform, before the next sync
// lists it; the sync then updates the same row).
func (s *Store) AddProject(ctx context.Context, sourceID int64, p provider.Project) (int64, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, `INSERT INTO projects (source_id, external_id, name, url, game, code_url, active)
		VALUES (?, ?, ?, ?, ?, ?, 1)
		ON CONFLICT (source_id, external_id) DO UPDATE SET active = 1
		RETURNING id`, sourceID, p.ExternalID, p.Name, p.URL, p.Game, p.CodeURL).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("store: add project %s: %w", p.ExternalID, err)
	}
	s.changes.Add(1)
	return id, nil
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
	return s.applyItems(ctx, sourceID, projectID, items, self, nil, time.Time{}, time.Time{})
}

// ApplyReconciled is ApplyItems for a project's full read at fullAt: the
// project's poll state records it (provider.PollState.FullAt) in the same
// transaction, so the next change check adopts its page fingerprint instead
// of asking for another full read.
func (s *Store) ApplyReconciled(ctx context.Context, sourceID, projectID int64, items []provider.Item, self string, fullAt time.Time) ([]Event, error) {
	return s.applyItems(ctx, sourceID, projectID, items, self, nil, time.Time{}, fullAt)
}

// ApplyChecked is ApplyItems for a change check's findings: the items, the
// project's cursor and its poll state (checked at checked) are written in one
// transaction, so a failure leaves all three as they were and the next check
// re-detects the same changes.
func (s *Store) ApplyChecked(ctx context.Context, sourceID, projectID int64, items []provider.Item, self string,
	poll provider.PollState, checked time.Time,
) ([]Event, error) {
	return s.applyItems(ctx, sourceID, projectID, items, self, &poll, checked, time.Time{})
}

func (s *Store) applyItems(ctx context.Context, sourceID, projectID int64, items []provider.Item, self string,
	poll *provider.PollState, checked, fullAt time.Time,
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

	stored, err := loadStored(ctx, tx, sourceID, items, self)
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
	if err := s.inboxEvents(ctx, tx, projectID, pr.key, events); err != nil {
		return nil, err
	}
	if !fullAt.IsZero() {
		if _, err := tx.ExecContext(ctx, `UPDATE projects SET poll_state = json_set(CASE WHEN json_valid(poll_state) THEN poll_state ELSE '{}' END,
			'$.fullAt', ?) WHERE id = ?`, fullAt.UTC().Format(time.RFC3339Nano), projectID); err != nil {
			return nil, fmt.Errorf("store: mark full read: %w", err)
		}
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
		JOIN projects p ON p.id = pl.code_project_id JOIN sources s ON s.id = p.source_id WHERE pl.mod_project_id = ? AND s.platform = '`+CodePlatform+`'`, id).Scan(&key)
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
	fields          [15]any // as the UPDATE binds them
	status, updated string
	unread          bool
	resolved        bool
	comments        map[string][4]string // external id → author, body, url, updated_at
	// last is the newest stored comment (author, created_at); others: a stored
	// comment by someone other than self exists.
	lastAuthor, lastAt string
	others             bool
}

// loadStored reads the stored rows of a batch's items and their comments in
// two queries, so an unchanged item costs no statement at all.
func loadStored(ctx context.Context, tx *sql.Tx, sourceID int64, items []provider.Item, self string) (map[string]*storedItem, error) {
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
		labels, created_at, updated_at, closed_at, unread, hidden, resolved FROM items
		WHERE source_id = ? AND external_id IN (SELECT value FROM json_each(?))`,
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
			hidden                                                            bool
		)
		if err := rows.Scan(&st.id, &ext, &project, &kind, &number, &title, &body, &url, &author, &st.status, &raw,
			&labels, &created, &st.updated, &closed, &st.unread, &hidden, &st.resolved); err != nil {
			return nil, fmt.Errorf("store: lookup items: %w", err)
		}
		st.fields = [15]any{project, kind, number, title, body, url, author, st.status, raw, labels, created, st.updated, closed, hidden, st.resolved}
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
	crows, err := tx.QueryContext(ctx, `SELECT item_id, external_id, author, body, url, updated_at, created_at FROM comments
		WHERE item_id IN (SELECT value FROM json_each(?)) ORDER BY item_id, created_at, id`, string(keysJSON))
	if err != nil {
		return nil, fmt.Errorf("store: lookup comments: %w", err)
	}
	defer func() { _ = crows.Close() }()
	for crows.Next() {
		var (
			item    int64
			ext     string
			created string
			c       [4]string
		)
		if err := crows.Scan(&item, &ext, &c[0], &c[1], &c[2], &c[3], &created); err != nil {
			return nil, fmt.Errorf("store: lookup comments: %w", err)
		}
		st := byID[item]
		st.comments[ext] = c
		st.lastAuthor, st.lastAt = c[0], created // oldest first: the last row wins
		st.others = st.others || !isSelf(c[0], self)
	}
	return out, crows.Err()
}

// isSelf reports whether author is the source's own account (never for an
// unknown account).
func isSelf(author, self string) bool { return self != "" && author == self }

// threadState is a comment thread's stored state after a sync batch: hidden
// (only the owner wrote in it), answered (the newest message is the owner's)
// and resolved (the local «Решено» flag, cleared by a new message from
// someone else).
func threadState(it *provider.Item, st *storedItem, self string) (hidden, answered, resolved bool) {
	lastAuthor, lastAt, others := it.Author, "", !isSelf(it.Author, self)
	if st != nil {
		resolved, others = st.resolved, others || st.others
		if st.lastAt != "" {
			lastAuthor, lastAt = st.lastAuthor, st.lastAt
		}
	}
	for _, c := range it.Comments {
		if at := ts(c.CreatedAt); at >= lastAt {
			lastAuthor, lastAt = c.Author, at
		}
		if !isSelf(c.Author, self) {
			others = true
			if _, known := st.comment(c.ExternalID); !known && st != nil {
				resolved = false // someone wrote again: waiting for an answer
			}
		}
	}
	return !others, isSelf(lastAuthor, self), resolved
}

func applyItem(ctx context.Context, tx *sql.Tx, sourceID, projectID int64, pr projectRef,
	it *provider.Item, st *storedItem, self string, baseline bool,
) (events []Event, changed bool, err error) {
	kind := it.Kind
	if kind == "" {
		kind = KindIssue
	}
	status := "closed"
	if it.Open {
		status = "open"
	}
	var hidden, resolved bool
	if kind == KindComment { // open = waiting for the owner's answer
		var answered bool
		hidden, answered, resolved = threadState(it, st, self)
		if hidden || answered || resolved {
			status = "closed"
		}
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
	fields := [15]any{projectID, kind, it.Number, it.Title, it.Body, it.URL, it.Author, status, it.RawStatus,
		string(lj), ts(it.CreatedAt), ts(it.UpdatedAt), ts(it.ClosedAt), hidden, resolved}
	args := fields[:]
	switch {
	case existed && fields == st.fields:
		// Unchanged row: no write (a full-history source re-reads every item
		// on each reconcile).
	case existed:
		_, err = tx.ExecContext(ctx, `UPDATE items SET project_id = ?, kind = ?, number = ?, title = ?, body = ?,
			url = ?, author = ?, status = ?, raw_status = ?, labels = ?, created_at = ?, updated_at = ?, closed_at = ?,
			hidden = ?, resolved = ? WHERE id = ?`, append(args, id)...)
	default:
		err = tx.QueryRowContext(ctx, `INSERT INTO items (project_id, kind, number, title, body, url, author,
			status, raw_status, labels, created_at, updated_at, closed_at, hidden, resolved, source_id, external_id)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) RETURNING id`,
			append(args, sourceID, it.ExternalID)...).Scan(&id)
	}
	if err != nil {
		return nil, false, fmt.Errorf("store: save item %s: %w", it.ExternalID, err)
	}

	base := Event{ItemID: id, ItemKind: kind, Project: pr.key, Repo: pr.name, Number: it.Number, Title: it.Title}
	platform, _, _ := strings.Cut(pr.key, ":")
	itemEvent := platform + ":" + it.ExternalID
	if !baseline && !existed && it.Author != self {
		e := base
		e.Kind, e.Actor, e.SourceEvent = EventNewIssue, it.Author, itemEvent
		if kind != KindIssue {
			e.Kind = EventNewItem
		}
		events = append(events, e)
	}
	if !baseline && existed && oldStatus == "open" && status == "closed" && kind != KindComment { // an answered thread is not closed on the platform
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
			e.Kind, e.Actor, e.Body, e.SourceEvent = EventNewComment, c.Author, c.Body, itemEvent+":"+c.ExternalID
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
	// The owner's reply answers a comment thread: it stops waiting (and is read).
	if _, err := s.db.ExecContext(ctx, `UPDATE items SET status = 'closed', unread = 0
		WHERE id = ? AND kind = 'comment' AND status = 'open' AND `+answeredSQL, itemID); err != nil {
		return Comment{}, fmt.Errorf("store: mark answered: %w", err)
	}
	return Comment{
		ExternalID: c.ExternalID, Author: c.Author, Mine: true, Body: c.Body, URL: c.URL,
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

// SetRead sets the unread flag of items ids (read = clear it) and returns how
// many really changed. Only an open item can be unread (a closed one is never).
func (s *Store) SetRead(ctx context.Context, ids []int64, read bool) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	b, err := json.Marshal(ids)
	if err != nil {
		return 0, err
	}
	q := `UPDATE items SET unread = 0 WHERE unread = 1 AND id IN (SELECT value FROM json_each(?))`
	if !read {
		q = `UPDATE items SET unread = 1 WHERE unread = 0 AND status = 'open' AND id IN (SELECT value FROM json_each(?))`
	}
	res, err := s.db.ExecContext(ctx, q, string(b))
	if err != nil {
		return 0, fmt.Errorf("store: set read: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// answeredSQL: the newest message of comment thread items (the item itself
// when it has no reply) is by its source's account.
const answeredSQL = `coalesce((SELECT c.author FROM comments c WHERE c.item_id = items.id ORDER BY c.created_at DESC, c.id DESC LIMIT 1), items.author)
	= (SELECT nullif(account, '') FROM sources WHERE id = items.source_id)`

// SetResolved sets the local «Решено» flag of comment threads ids (other kinds
// are left alone) and returns how many changed. Resolved threads are closed
// and read; reopening one makes it wait again unless the owner answered last.
func (s *Store) SetResolved(ctx context.Context, ids []int64, resolved bool) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	b, err := json.Marshal(ids)
	if err != nil {
		return 0, err
	}
	q := `UPDATE items SET resolved = 1, status = 'closed', unread = 0
		WHERE kind = 'comment' AND resolved = 0 AND id IN (SELECT value FROM json_each(?))`
	if !resolved {
		q = `UPDATE items SET resolved = 0, status = CASE WHEN hidden = 1 OR ` + answeredSQL + ` THEN 'closed' ELSE 'open' END
			WHERE kind = 'comment' AND resolved = 1 AND id IN (SELECT value FROM json_each(?))`
	}
	res, err := s.db.ExecContext(ctx, q, string(b))
	if err != nil {
		return 0, fmt.Errorf("store: set resolved: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
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
