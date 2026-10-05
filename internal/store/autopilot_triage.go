package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Auto-triage (docs/AUTOPILOT.md → Fix run state machine, Phase 4): the
// terminal fix run states of the paths that fix nothing.
const (
	RunDuplicate = "duplicate" // attached to the original's run, or answered with the release that fixed the original
	RunAnswered  = "answered"  // a question: replied, nothing to fix
	RunIgnored   = "ignored"   // feature request / spam / not actionable / filtered: no action
)

// DupCandidate is an item a new report may duplicate: an item of the same
// code project (its own issues and its linked mod pages) with its latest fix
// run.
type DupCandidate struct {
	ItemID    int64
	Platform  string
	Kind      string
	Number    int
	Title     string
	Body      string // first bodyRunes runes
	Open      bool
	UpdatedAt string
	FixRunID  int64  // latest fix run (0 = none)
	FixState  string // its state
	ReleaseID int64  // the release run that claimed it (0 = none)
	Version   string // that release's version when it is done
}

// DuplicateCandidates lists up to limit items of code project codeID (its own
// items and its linked mod pages' items) except exclude: items with a fix run
// first, then open ones, newest first; bodies cut to bodyRunes.
func (s *Store) DuplicateCandidates(ctx context.Context, codeID, exclude int64, limit, bodyRunes int) ([]DupCandidate, error) {
	if limit <= 0 {
		limit = 40
	}
	rows, err := s.rd.QueryContext(ctx, `WITH fr AS (
			SELECT ri.item_id, max(r.id) AS run_id FROM autopilot_run_items ri JOIN autopilot_runs r ON r.id = ri.run_id
			WHERE r.kind = 'fix' GROUP BY ri.item_id)
		SELECT i.id, s.platform, i.kind, i.number, i.title, substr(i.body, 1, ?), i.status = 'open', i.updated_at,
			coalesce(r.id, 0), coalesce(r.state, ''), coalesce(r.release_id, 0),
			coalesce((SELECT rr.version FROM autopilot_runs rr WHERE rr.id = r.release_id AND rr.state = 'done'), '')
		FROM items i JOIN sources s ON s.id = i.source_id
		LEFT JOIN fr ON fr.item_id = i.id LEFT JOIN autopilot_runs r ON r.id = fr.run_id
		WHERE i.id <> ? AND (i.project_id = ? OR i.project_id IN (SELECT mod_project_id FROM project_links WHERE code_project_id = ?))
			AND (i.status = 'open' OR r.id IS NOT NULL)
		ORDER BY r.id IS NULL, i.updated_at DESC, i.id DESC LIMIT ?`, bodyRunes, exclude, codeID, codeID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: duplicate candidates: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []DupCandidate{}
	for rows.Next() {
		var c DupCandidate
		if err := rows.Scan(&c.ItemID, &c.Platform, &c.Kind, &c.Number, &c.Title, &c.Body, &c.Open, &c.UpdatedAt,
			&c.FixRunID, &c.FixState, &c.ReleaseID, &c.Version); err != nil {
			return nil, fmt.Errorf("store: scan candidate: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// OpenFixRun is item itemID's open fix run (pending, running, held, pushed);
// ErrNotFound when there is none.
func (s *Store) OpenFixRun(ctx context.Context, itemID int64) (Run, error) {
	return s.oneRun(ctx, `SELECT `+runColumns+` FROM autopilot_runs WHERE kind = 'fix' AND state IN `+openFixStates+`
		AND id IN (SELECT run_id FROM autopilot_run_items WHERE item_id = ?) ORDER BY id DESC LIMIT 1`, itemID)
}

// LatestFixRun is item itemID's newest fix run in any state (ErrNotFound when none).
func (s *Store) LatestFixRun(ctx context.Context, itemID int64) (Run, error) {
	return s.oneRun(ctx, `SELECT `+runColumns+` FROM autopilot_runs WHERE kind = 'fix'
		AND id IN (SELECT run_id FROM autopilot_run_items WHERE item_id = ?) ORDER BY id DESC LIMIT 1`, itemID)
}

// LatestRelease is project projectID's newest finished (done) release run
// (ErrNotFound when it never released).
func (s *Store) LatestRelease(ctx context.Context, projectID int64) (Run, error) {
	return s.oneRun(ctx, `SELECT `+runColumns+` FROM autopilot_runs WHERE kind = 'release' AND state = 'done' AND project_id = ?
		ORDER BY id DESC LIMIT 1`, projectID)
}

func (s *Store) oneRun(ctx context.Context, q string, args ...any) (Run, error) {
	r, err := scanRun(s.rd.QueryRowContext(ctx, q, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return r, ErrNotFound
	}
	if err != nil {
		return r, fmt.Errorf("store: run: %w", err)
	}
	return r, nil
}

// AttachDuplicate adds item itemID to fix run runID as a duplicate while the
// run is open (it then answers for the item too: the release replies to it).
// A run no longer open → ErrRunState; the item already in it → no change.
func (s *Store) AttachDuplicate(ctx context.Context, runID, itemID int64) error {
	res, err := s.db.ExecContext(ctx, `INSERT INTO autopilot_run_items (run_id, item_id, role)
		SELECT id, ?, 'duplicate' FROM autopilot_runs WHERE id = ? AND kind = 'fix' AND state IN `+openFixStates+`
		ON CONFLICT (run_id, item_id) DO NOTHING`, itemID, runID)
	if err != nil {
		return fmt.Errorf("store: attach duplicate: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 1 {
		_, _ = s.db.ExecContext(ctx, `UPDATE autopilot_runs SET updated_at = ? WHERE id = ?`, ts(time.Now()), runID)
		return nil
	}
	var in int
	if err := s.rd.QueryRowContext(ctx, `SELECT count(*) FROM autopilot_run_items ri JOIN autopilot_runs r ON r.id = ri.run_id
		WHERE ri.run_id = ? AND ri.item_id = ? AND r.state IN `+openFixStates, runID, itemID).Scan(&in); err != nil {
		return fmt.Errorf("store: attach duplicate: %w", err)
	}
	if in == 1 {
		return nil
	}
	return ErrRunState
}

// Reporter is the author of a regression report (an item of a fix run).
type Reporter struct {
	ItemID   int64
	Platform string
	Author   string
	RunID    int64
}

// RegressionReports lists the primary items of project projectID's fix runs
// created at or after since whose triage flagged a regression (manifest
// triage.regression), oldest first.
func (s *Store) RegressionReports(ctx context.Context, projectID int64, since time.Time) ([]Reporter, error) {
	rows, err := s.rd.QueryContext(ctx, `SELECT i.id, src.platform, i.author, r.id FROM autopilot_runs r
		JOIN autopilot_run_items ri ON ri.run_id = r.id AND ri.role = 'primary'
		JOIN items i ON i.id = ri.item_id JOIN sources src ON src.id = i.source_id
		WHERE r.kind = 'fix' AND r.project_id = ? AND r.created_at >= ?
			AND json_extract(r.manifest_json, '$.triage.regression') = 1
		ORDER BY r.id`, projectID, ts(since))
	if err != nil {
		return nil, fmt.Errorf("store: regression reports: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Reporter
	for rows.Next() {
		var r Reporter
		if err := rows.Scan(&r.ItemID, &r.Platform, &r.Author, &r.RunID); err != nil {
			return nil, fmt.Errorf("store: scan reporter: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// TriagesSince counts the triage steps of fix runs started at or after since
// (the classify agent's share of the daily automation cap).
func (s *Store) TriagesSince(ctx context.Context, since time.Time) (int, error) {
	var n int
	err := s.rd.QueryRowContext(ctx, `SELECT count(*) FROM autopilot_steps WHERE step = 'triage' AND started_at <> '' AND started_at >= ?`,
		ts(since)).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: triages: %w", err)
	}
	return n, nil
}
