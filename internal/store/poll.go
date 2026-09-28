package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/provider"
)

// PollTarget is an active project on the change-check schedule.
type PollTarget struct {
	Project
	Activity time.Time          // newest item update (zero = no items)
	Poll     provider.PollState // persisted change-detection state
}

// PollTargets lists the active projects of a source with their newest item
// update and poll state.
func (s *Store) PollTargets(ctx context.Context, sourceID int64) ([]PollTarget, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT p.id, p.external_id, p.name, p.url, p.sync_cursor, p.poll_state,
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
			cursor, state, activity string
		)
		if err := rows.Scan(&t.ID, &t.ExternalID, &t.Name, &t.URL, &cursor, &state, &activity); err != nil {
			return nil, fmt.Errorf("store: poll targets: %w", err)
		}
		t.Cursor, _ = time.Parse(timeFormat, cursor)
		t.Activity, _ = time.Parse(timeFormat, activity)
		_ = json.Unmarshal([]byte(state), &t.Poll) // a damaged state only costs one full answer
		out = append(out, t)
	}
	return out, rows.Err()
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
