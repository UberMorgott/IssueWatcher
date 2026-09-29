package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/provider"
)

// PollTarget is an active project on the change-check schedule.
type PollTarget struct {
	Project
	Activity time.Time          // newest item update (zero = no items)
	Checked  time.Time          // last change check (zero = never)
	Poll     provider.PollState // persisted change-detection state
}

// PollTargets lists the active projects of a source with their newest item
// update and poll state.
func (s *Store) PollTargets(ctx context.Context, sourceID int64) ([]PollTarget, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT p.id, p.external_id, p.name, p.url, p.sync_cursor, p.poll_state, p.checked_at,
		COALESCE((SELECT max(i.updated_at) FROM items i WHERE i.project_id = p.id), '')
		FROM projects p WHERE p.source_id = ? AND p.active = 1 ORDER BY p.id`, sourceID)
	if err != nil {
		return nil, fmt.Errorf("store: poll targets: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []PollTarget
	for rows.Next() {
		var (
			t                       PollTarget
			cursor, state, checked, activity string
		)
		if err := rows.Scan(&t.ID, &t.ExternalID, &t.Name, &t.URL, &cursor, &state, &checked, &activity); err != nil {
			return nil, fmt.Errorf("store: poll targets: %w", err)
		}
		t.Cursor, _ = time.Parse(timeFormat, cursor)
		t.Activity, _ = time.Parse(timeFormat, activity)
		t.Checked, _ = time.Parse(timeFormat, checked)
		_ = json.Unmarshal([]byte(state), &t.Poll) // a damaged state only costs one full answer
		out = append(out, t)
	}
	return out, rows.Err()
}

// SourceState is a stored source with the time of its last completed full
// reconcile (zero = never): what a restart resumes from.
type SourceState struct {
	ID           int64
	Account      string
	ReconciledAt time.Time
}

// LastSource returns the source of platform to resume from: the one of
// account when account is not "", else the most recently reconciled one.
// ok is false when the store has none.
func (s *Store) LastSource(ctx context.Context, platform, account string) (st SourceState, ok bool, err error) {
	q := `SELECT id, account, reconciled_at FROM sources WHERE platform = ?`
	args := []any{platform}
	if account != "" {
		q += ` AND account = ?`
		args = append(args, account)
	}
	var rec string
	err = s.db.QueryRowContext(ctx, q+` ORDER BY reconciled_at DESC, id DESC LIMIT 1`, args...).Scan(&st.ID, &st.Account, &rec)
	if errors.Is(err, sql.ErrNoRows) {
		return st, false, nil
	}
	if err != nil {
		return st, false, fmt.Errorf("store: last source: %w", err)
	}
	st.ReconciledAt, _ = time.Parse(timeFormat, rec)
	return st, true, nil
}

// MarkReconciled records a completed full reconcile of source id at t.
func (s *Store) MarkReconciled(ctx context.Context, id int64, t time.Time) error {
	if _, err := s.db.ExecContext(ctx, `UPDATE sources SET reconciled_at = ? WHERE id = ?`, ts(t), id); err != nil {
		return fmt.Errorf("store: mark reconciled: %w", err)
	}
	return nil
}

// SavePollState stores a project's change-detection state after a check at checked.
func (s *Store) SavePollState(ctx context.Context, projectID int64, st provider.PollState, checked time.Time) error {
	b, err := json.Marshal(st)
	if err != nil {
		return fmt.Errorf("store: encode poll state: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE projects SET poll_state = ?, checked_at = ? WHERE id = ?`,
		string(b), ts(checked), projectID); err != nil {
		return fmt.Errorf("store: save poll state: %w", err)
	}
	return nil
}
