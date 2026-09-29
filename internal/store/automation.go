package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"
)

// Automation decisions and skip reasons written by the store (the runner adds
// its own reasons for checks made before the transaction).
const (
	DecisionQueued  = "queued"
	DecisionSkipped = "skipped"

	ReasonExists      = "exists"       // the item has an unfinished job of this flow
	ReasonMaxAttempts = "max_attempts" // rule jobs for item+flow reached maxAttempts
	ReasonTotalCap    = "total_cap"    // rule jobs of all projects in 24 h reached the global maxPerDay
	ReasonDayCap      = "day_cap"      // the project's rule jobs in 24 h reached its maxPerDay override
	ReasonRuleCap     = "rule_cap"     // the rule's jobs in 24 h reached its maxPerDay
)

// automationLogKeep bounds the decision log (oldest rows go first).
const automationLogKeep = 5000

const timeLayout = "2006-01-02T15:04:05Z"

// AutomationRequest is one rule decision to record and, unless Skip is set,
// to turn into a rule job within the caps.
type AutomationRequest struct {
	At        time.Time
	ItemID    int64
	Event     string
	RuleID    string
	Flow      string
	ProfileID string
	// Skip: a reason found before the transaction; only the log row is written.
	Skip string
	// TotalCap caps rule jobs of all projects in the 24 h before At (≥ 1).
	TotalCap int
	// DayCap caps the project's rule jobs in the 24 h before At; 0 = no project cap.
	DayCap int
	// RuleCap caps this rule's jobs in the 24 h before At; 0 = no rule cap.
	RuleCap int
	// MaxAttempts caps rule jobs ever made for item+flow (≥ 1).
	MaxAttempts int
}

// AutomationEntry is one row of the decision log (API shape).
type AutomationEntry struct {
	ID       int64  `json:"id"`
	At       string `json:"at"`
	ItemID   int64  `json:"itemId"`
	Event    string `json:"event"`
	RuleID   string `json:"ruleId"`
	Flow     string `json:"flow"`
	Decision string `json:"decision"`
	Reason   string `json:"reason"`
	JobID    *int64 `json:"jobId"`
	Repo     string `json:"repo"`
	Number   int    `json:"number"`
	Title    string `json:"title"`
}

// AutomationChunk is one slice of the decision log, newest first.
type AutomationChunk struct {
	Items      []AutomationEntry `json:"items"`
	NextCursor string            `json:"nextCursor"`
	More       bool              `json:"more"`
}

// AutomationItem holds the item facts rules match on.
type AutomationItem struct {
	Labels     []string
	LocalPath  string
	ProjectURL string
}

// AutomationItemFacts returns item itemID's labels and the folder its fixes run
// in (its project's, or a linked mod page's code project's).
func (s *Store) AutomationItemFacts(ctx context.Context, itemID int64) (AutomationItem, error) {
	var (
		it     AutomationItem
		labels string
	)
	err := s.db.QueryRowContext(ctx, `SELECT i.labels, `+folderCols+` FROM items i JOIN projects p ON p.id = i.project_id`+linkJoin+`
		WHERE i.id = ?`, itemID).Scan(&labels, &it.LocalPath, &it.ProjectURL)
	if errors.Is(err, sql.ErrNoRows) {
		return it, ErrNotFound
	}
	if err != nil {
		return it, fmt.Errorf("store: automation item: %w", err)
	}
	if err := json.Unmarshal([]byte(labels), &it.Labels); err != nil || it.Labels == nil {
		it.Labels = []string{}
	}
	return it, nil
}

// Automate records one rule decision. Without req.Skip it checks, in one
// transaction, the unfinished job of item+flow, maxAttempts and the 24 h caps,
// then inserts the rule job; the log row carries the outcome. job is nil when
// skipped.
func (s *Store) Automate(ctx context.Context, req AutomationRequest) (AutomationEntry, *Job, error) {
	at := req.At.UTC().Format(timeLayout)
	since := req.At.UTC().Add(-24 * time.Hour).Format(timeLayout)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return AutomationEntry{}, nil, fmt.Errorf("store: automate: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	reason := req.Skip
	var jobID int64
	if reason == "" {
		reason, jobID, err = automateTx(ctx, tx, req, at, since)
		if err != nil {
			return AutomationEntry{}, nil, err
		}
	}
	decision := DecisionQueued
	var logJob any
	if reason != "" {
		decision = DecisionSkipped
	} else {
		logJob = jobID
	}
	var logID int64
	if err := tx.QueryRowContext(ctx, `INSERT INTO automation_log (at, item_id, event, rule_id, flow, decision, reason, job_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?) RETURNING id`, at, req.ItemID, req.Event, req.RuleID, req.Flow, decision, reason, logJob).Scan(&logID); err != nil {
		return AutomationEntry{}, nil, fmt.Errorf("store: automation log: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM automation_log WHERE id <= ?`, logID-automationLogKeep); err != nil {
		return AutomationEntry{}, nil, fmt.Errorf("store: trim automation log: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return AutomationEntry{}, nil, fmt.Errorf("store: automate: %w", err)
	}
	e, err := s.automationEntry(ctx, logID)
	if err != nil || jobID == 0 {
		return e, nil, err
	}
	j, err := s.Job(ctx, jobID)
	if err != nil {
		return e, nil, err
	}
	return e, &j, nil
}

// automateTx runs the checks and the insert; reason != "" = skipped.
func automateTx(ctx context.Context, tx *sql.Tx, req AutomationRequest, at, since string) (string, int64, error) {
	var projectID int64
	if err := tx.QueryRowContext(ctx, `SELECT project_id FROM items WHERE id = ?`, req.ItemID).Scan(&projectID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", 0, ErrNotFound
		}
		return "", 0, fmt.Errorf("store: automate item: %w", err)
	}
	checks := []struct {
		reason string
		limit  int
		query  string
		args   []any
	}{
		{ReasonExists, 1, `SELECT count(*) FROM jobs WHERE item_id = ? AND flow = ? AND state IN ('queued', 'running', 'needs_review')`,
			[]any{req.ItemID, req.Flow}},
		{ReasonMaxAttempts, req.MaxAttempts, `SELECT count(*) FROM jobs WHERE origin = 'rule' AND item_id = ? AND flow = ?`,
			[]any{req.ItemID, req.Flow}},
		{ReasonTotalCap, req.TotalCap, `SELECT count(*) FROM jobs WHERE origin = 'rule' AND created_at > ?`,
			[]any{since}},
		{ReasonDayCap, req.DayCap, `SELECT count(*) FROM jobs WHERE origin = 'rule' AND project_id = ? AND created_at > ?`,
			[]any{projectID, since}},
		{ReasonRuleCap, req.RuleCap, `SELECT count(*) FROM jobs WHERE origin = 'rule' AND rule_id = ? AND created_at > ?`,
			[]any{req.RuleID, since}},
	}
	for _, c := range checks {
		if c.limit <= 0 { // project/rule cap 0 = none
			continue
		}
		var n int
		if err := tx.QueryRowContext(ctx, c.query, c.args...).Scan(&n); err != nil {
			return "", 0, fmt.Errorf("store: automate %s: %w", c.reason, err)
		}
		if n >= c.limit {
			return c.reason, 0, nil
		}
	}
	var id int64
	err := tx.QueryRowContext(ctx, `INSERT INTO jobs (item_id, project_id, flow, profile_id, origin, rule_id, created_at, updated_at)
		VALUES (?, ?, ?, ?, 'rule', ?, ?, ?) RETURNING id`, req.ItemID, projectID, req.Flow, req.ProfileID, req.RuleID, at, at).Scan(&id)
	if err != nil {
		return "", 0, fmt.Errorf("store: automate insert: %w", err)
	}
	return "", id, nil
}

const automationColumns = `a.id, a.at, a.item_id, a.event, a.rule_id, a.flow, a.decision, a.reason, a.job_id, p.name, i.number, i.title
	FROM automation_log a JOIN items i ON i.id = a.item_id JOIN projects p ON p.id = i.project_id`

func scanAutomation(sc interface{ Scan(...any) error }) (AutomationEntry, error) {
	var (
		e   AutomationEntry
		job sql.NullInt64
	)
	err := sc.Scan(&e.ID, &e.At, &e.ItemID, &e.Event, &e.RuleID, &e.Flow, &e.Decision, &e.Reason, &job, &e.Repo, &e.Number, &e.Title)
	if job.Valid {
		e.JobID = &job.Int64
	}
	return e, err
}

func (s *Store) automationEntry(ctx context.Context, id int64) (AutomationEntry, error) {
	e, err := scanAutomation(s.db.QueryRowContext(ctx, "SELECT "+automationColumns+" WHERE a.id = ?", id))
	if err != nil {
		return e, fmt.Errorf("store: automation entry: %w", err)
	}
	return e, nil
}

// AutomationLog returns a chunk of the decision log, newest first; cursor =
// older than that row.
func (s *Store) AutomationLog(ctx context.Context, cursor string, limit int) (AutomationChunk, error) {
	limit = clampLimit(limit)
	chunk := AutomationChunk{Items: []AutomationEntry{}}
	var before int64 = math.MaxInt64
	if cursor != "" {
		k, err := decodeCursor(cursor)
		if err != nil {
			return chunk, err
		}
		before = k.ID
	}
	rows, err := s.db.QueryContext(ctx, "SELECT "+automationColumns+" WHERE a.id < ? ORDER BY a.id DESC LIMIT ?", before, limit+1)
	if err != nil {
		return chunk, fmt.Errorf("store: automation log: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		e, err := scanAutomation(rows)
		if err != nil {
			return chunk, fmt.Errorf("store: scan automation log: %w", err)
		}
		chunk.Items = append(chunk.Items, e)
	}
	if err := rows.Err(); err != nil {
		return chunk, fmt.Errorf("store: automation log: %w", err)
	}
	if len(chunk.Items) > limit {
		chunk.Items, chunk.More = chunk.Items[:limit], true
	}
	if n := len(chunk.Items); n > 0 {
		chunk.NextCursor = encodeCursor(chunk.Items[n-1].ID, chunk.Items[n-1].ID)
	}
	return chunk, nil
}
