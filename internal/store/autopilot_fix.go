package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Fix run states besides pending / running / held / failed / cancelled
// (docs/AUTOPILOT.md → Fix run state machine): pushed = on the default
// branch, waiting for a release run to claim it; released = its release run
// finished.
const (
	RunPushed   = "pushed"
	RunReleased = "released"
)

// RuleAutopilot is the rule id of the fix jobs autopilot fix runs queue (job
// origin rule: the same caps as rule jobs; the prompt says "Refs #N").
const RuleAutopilot = "autopilot"

// Inbox outcomes of events dropped without a run, or attached to an open one.
const (
	InboxAttached = "attached"
)

// ErrClaimRace means a fix run to claim was not pushed and unclaimed any more.
var ErrClaimRace = errors.New("store: fix run already claimed or not pushed")

// openFixStates are the fix run states that still answer for their item (one per item).
const openFixStates = `('pending', 'running', 'held', 'pushed')`

// inboxEvents writes the batch's new_issue / new_comment / new_item events of
// an accepted code project into autopilot_inbox (in the sync transaction).
func (s *Store) inboxEvents(ctx context.Context, tx *sql.Tx, projectID int64, key string, events []Event) error {
	f := s.inbox.Load()
	if f == nil || len(events) == 0 {
		return nil
	}
	var code string
	_ = tx.QueryRowContext(ctx, `SELECT s.platform || ':' || p.external_id FROM project_links pl
		JOIN projects p ON p.id = pl.code_project_id JOIN sources s ON s.id = p.source_id
		WHERE pl.mod_project_id = ? AND s.platform = '`+CodePlatform+`'`, projectID).Scan(&code)
	if code == "" {
		code = key
	}
	if !(*f)(code) {
		return nil
	}
	at := ts(time.Now())
	for _, e := range events {
		if e.SourceEvent == "" || (e.Kind != EventNewIssue && e.Kind != EventNewComment && e.Kind != EventNewItem) {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO autopilot_inbox (source_event, item_id, kind, at) VALUES (?, ?, ?, ?)
			ON CONFLICT (source_event) DO NOTHING`, e.SourceEvent, e.ItemID, string(e.Kind), at); err != nil {
			return fmt.Errorf("store: autopilot inbox: %w", err)
		}
	}
	return nil
}

// InboxEvent is one autopilot inbox row.
type InboxEvent struct {
	ID          int64  `json:"id"`
	SourceEvent string `json:"sourceEvent"`
	ItemID      int64  `json:"itemId"`
	Kind        string `json:"kind"`
	At          string `json:"at"`
	RunID       int64  `json:"runId,omitempty"`
	Outcome     string `json:"outcome,omitempty"`
}

// PendingInbox lists unconsumed inbox events, oldest first (limit 0 = 100).
func (s *Store) PendingInbox(ctx context.Context, limit int) ([]InboxEvent, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.rd.QueryContext(ctx, `SELECT id, source_event, item_id, kind, at, coalesce(consumed_run_id, 0), outcome
		FROM autopilot_inbox WHERE consumed_run_id IS NULL AND outcome = '' ORDER BY id LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("store: inbox: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []InboxEvent{}
	for rows.Next() {
		var e InboxEvent
		if err := rows.Scan(&e.ID, &e.SourceEvent, &e.ItemID, &e.Kind, &e.At, &e.RunID, &e.Outcome); err != nil {
			return nil, fmt.Errorf("store: scan inbox: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// InboxEventByID reads one inbox row (ErrNotFound when absent).
func (s *Store) InboxEventByID(ctx context.Context, id int64) (InboxEvent, error) {
	var e InboxEvent
	err := s.rd.QueryRowContext(ctx, `SELECT id, source_event, item_id, kind, at, coalesce(consumed_run_id, 0), outcome
		FROM autopilot_inbox WHERE id = ?`, id).Scan(&e.ID, &e.SourceEvent, &e.ItemID, &e.Kind, &e.At, &e.RunID, &e.Outcome)
	if errors.Is(err, sql.ErrNoRows) {
		return e, ErrNotFound
	}
	if err != nil {
		return e, fmt.Errorf("store: inbox event: %w", err)
	}
	return e, nil
}

// DropInbox consumes inbox event id without a run, recording why. It reports
// false when the event was consumed meanwhile.
func (s *Store) DropInbox(ctx context.Context, id int64, outcome string) (bool, error) {
	if outcome == "" {
		outcome = "dropped"
	}
	res, err := s.db.ExecContext(ctx, `UPDATE autopilot_inbox SET outcome = ? WHERE id = ? AND consumed_run_id IS NULL AND outcome = ''`,
		outcome, id)
	if err != nil {
		return false, fmt.Errorf("store: drop inbox event: %w", err)
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// NewFixRun is a fix run ConsumeInbox creates for an inbox event's item.
type NewFixRun struct {
	ProjectID int64 // the code project
	Manifest  json.RawMessage
	Steps     []NewStep
	// Create: start a fix run when the item has no open one; false = attach
	// only (no open run → ErrNotFound, the event stays pending).
	Create bool
}

// ConsumeInbox consumes inbox event id in one transaction: an open fix run of
// its item (pending, running, held, pushed) gets the event (attached = true,
// no second fix); otherwise a pending fix run (origin auto) is created from
// nr with the item as primary. An event consumed meanwhile → ErrRunState.
func (s *Store) ConsumeInbox(ctx context.Context, id int64, nr NewFixRun) (run Run, attached bool, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Run{}, false, fmt.Errorf("store: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var item int64
	err = tx.QueryRowContext(ctx, `SELECT item_id FROM autopilot_inbox WHERE id = ? AND consumed_run_id IS NULL AND outcome = ''`, id).Scan(&item)
	if errors.Is(err, sql.ErrNoRows) {
		return Run{}, false, ErrRunState
	}
	if err != nil {
		return Run{}, false, fmt.Errorf("store: inbox event %d: %w", id, err)
	}
	var runID int64
	err = tx.QueryRowContext(ctx, `SELECT r.id FROM autopilot_runs r JOIN autopilot_run_items ri ON ri.run_id = r.id
		WHERE ri.item_id = ? AND r.kind = 'fix' AND r.state IN `+openFixStates+` ORDER BY r.id DESC LIMIT 1`, item).Scan(&runID)
	switch {
	case err == nil:
		attached = true
	case errors.Is(err, sql.ErrNoRows) && !nr.Create:
		return Run{}, false, ErrNotFound
	case errors.Is(err, sql.ErrNoRows):
		at := ts(time.Now())
		if err := tx.QueryRowContext(ctx, `INSERT INTO autopilot_runs (kind, project_id, state, origin, manifest_json, created_at, updated_at)
			VALUES ('fix', ?, 'pending', 'auto', ?, ?, ?) RETURNING id`, nr.ProjectID, rawOrEmpty(nr.Manifest), at, at).Scan(&runID); err != nil {
			return Run{}, false, fmt.Errorf("store: create fix run: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO autopilot_run_items (run_id, item_id, role) VALUES (?, ?, 'primary')`, runID, item); err != nil {
			return Run{}, false, fmt.Errorf("store: fix run item: %w", err)
		}
		for i, ns := range nr.Steps {
			if _, err := insertStep(ctx, tx, runID, i+1, ns); err != nil {
				return Run{}, false, err
			}
		}
	default:
		return Run{}, false, fmt.Errorf("store: open fix run: %w", err)
	}
	outcome := ""
	if attached {
		outcome = InboxAttached
	}
	if _, err := tx.ExecContext(ctx, `UPDATE autopilot_inbox SET consumed_run_id = ?, outcome = ? WHERE id = ?`, runID, outcome, id); err != nil {
		return Run{}, false, fmt.Errorf("store: consume inbox event: %w", err)
	}
	r, err := scanRun(tx.QueryRowContext(ctx, `SELECT `+runColumns+` FROM autopilot_runs WHERE id = ?`, runID))
	if err != nil {
		return Run{}, false, fmt.Errorf("store: read fix run: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Run{}, false, fmt.Errorf("store: commit inbox: %w", err)
	}
	return r, attached, nil
}

// ItemFixRuns counts item itemID's fix runs (any state).
func (s *Store) ItemFixRuns(ctx context.Context, itemID int64) (int, error) {
	var n int
	err := s.rd.QueryRowContext(ctx, `SELECT count(*) FROM autopilot_runs r JOIN autopilot_run_items ri ON ri.run_id = r.id
		WHERE ri.item_id = ? AND r.kind = 'fix'`, itemID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: item fix runs: %w", err)
	}
	return n, nil
}

// ItemCode is where an item's fixes go: its code project (the item's own
// GitHub project, or the code project its mod page is linked to).
type ItemCode struct {
	ItemID     int64
	Kind       string // issue | comment | bug
	Open       bool
	Platform   string // the item's platform
	Number     int
	URL        string
	CodeID     int64  // 0 = no code project
	CodeKey    string // github:owner/repo
	ProjectKey string // the item's own project key
}

// ItemCodeProject resolves item itemID's code project (ErrNotFound when the item is gone).
func (s *Store) ItemCodeProject(ctx context.Context, itemID int64) (ItemCode, error) {
	c := ItemCode{ItemID: itemID}
	var status string
	err := s.rd.QueryRowContext(ctx, `SELECT i.kind, i.status, s.platform, i.number, i.url, `+projectKeySQL+`,
		CASE WHEN s.platform = '`+CodePlatform+`' THEN p.id ELSE coalesce(cp.id, 0) END,
		CASE WHEN s.platform = '`+CodePlatform+`' THEN `+projectKeySQL+`
			ELSE coalesce((SELECT platform FROM sources WHERE id = cp.source_id) || ':' || cp.external_id, '') END
		FROM items i JOIN projects p ON p.id = i.project_id JOIN sources s ON s.id = i.source_id`+linkJoin+`
		WHERE i.id = ?`, itemID).Scan(&c.Kind, &status, &c.Platform, &c.Number, &c.URL, &c.ProjectKey, &c.CodeID, &c.CodeKey)
	if errors.Is(err, sql.ErrNoRows) {
		return c, ErrNotFound
	}
	if err != nil {
		return c, fmt.Errorf("store: item code project: %w", err)
	}
	c.Open = status == "open"
	return c, nil
}

// AutopilotJob returns item itemID's newest fix job queued by autopilot
// (rule RuleAutopilot) created at or after since (RFC 3339; "" = any);
// ErrNotFound when there is none.
func (s *Store) AutopilotJob(ctx context.Context, itemID int64, since string) (Job, error) {
	j, err := scanJob(s.rd.QueryRowContext(ctx, "SELECT "+jobColumns+jobFrom+` WHERE j.item_id = ? AND j.flow = 'fix'
		AND j.origin = 'rule' AND j.rule_id = ? AND j.created_at >= ? ORDER BY j.id DESC LIMIT 1`, itemID, RuleAutopilot, since))
	if errors.Is(err, sql.ErrNoRows) {
		return j, ErrNotFound
	}
	if err != nil {
		return j, fmt.Errorf("store: autopilot job: %w", err)
	}
	return j, nil
}

// ClaimableFixRuns lists project projectID's pushed fix runs no release has
// claimed, oldest first.
func (s *Store) ClaimableFixRuns(ctx context.Context, projectID int64) ([]Run, error) {
	return s.queryRuns(ctx, `SELECT `+runColumns+` FROM autopilot_runs
		WHERE kind = 'fix' AND project_id = ? AND state = 'pushed' AND release_id IS NULL ORDER BY id`, projectID)
}

// FixRunsOf lists the fix runs release run releaseID claimed.
func (s *Store) FixRunsOf(ctx context.Context, releaseID int64) ([]Run, error) {
	return s.queryRuns(ctx, `SELECT `+runColumns+` FROM autopilot_runs WHERE kind = 'fix' AND release_id = ? ORDER BY id`, releaseID)
}

// SetFixRunsReleased moves the fix runs claimed by releaseID from pushed to released.
func (s *Store) SetFixRunsReleased(ctx context.Context, releaseID int64) (int64, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE autopilot_runs SET state = 'released', updated_at = ?
		WHERE kind = 'fix' AND release_id = ? AND state = 'pushed'`, ts(time.Now()), releaseID)
	if err != nil {
		return 0, fmt.Errorf("store: release fix runs: %w", err)
	}
	return res.RowsAffected()
}

// UnclaimFixRuns returns the fix runs claimed by releaseID to the pool (a
// release cancelled before any public step).
func (s *Store) UnclaimFixRuns(ctx context.Context, releaseID int64) (int64, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE autopilot_runs SET release_id = NULL, updated_at = ?
		WHERE kind = 'fix' AND release_id = ? AND state = 'pushed'`, ts(time.Now()), releaseID)
	if err != nil {
		return 0, fmt.Errorf("store: unclaim fix runs: %w", err)
	}
	return res.RowsAffected()
}

// claimFixRuns sets release_id on the pushed, unclaimed fix runs ids (all or none).
func claimFixRuns(ctx context.Context, tx *sql.Tx, releaseID, projectID int64, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	args := []any{releaseID, ts(time.Now()), projectID}
	for _, id := range ids {
		args = append(args, id)
	}
	q := `UPDATE autopilot_runs SET release_id = ?, updated_at = ? WHERE kind = 'fix' AND project_id = ? AND state = 'pushed' AND release_id IS NULL AND id IN (?` + strings.Repeat(", ?", len(ids)-1) + `)` //nolint:gosec // G202: placeholders only
	res, err := tx.ExecContext(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("store: claim fix runs: %w", err)
	}
	if n, _ := res.RowsAffected(); n != int64(len(ids)) {
		return ErrClaimRace
	}
	return nil
}
