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

// Autopilot run kinds and origins (docs/AUTOPILOT.md).
const (
	RunKindFix     = "fix"
	RunKindRelease = "release"

	RunOriginAuto   = "auto"
	RunOriginManual = "manual" // UI click
	RunOriginMCP    = "mcp"    // MCP / CLI
)

// Release run states. pending, running and held are unfinished: a project has
// at most one unfinished release run (index autopilot_runs_release_active).
const (
	RunPending   = "pending"
	RunRunning   = "running"
	RunHeld      = "held"
	RunDone      = "done"
	RunCancelled = "cancelled"
	RunFailed    = "failed"
)

// Step states: pending → sending → sent | failed | unknown | skipped. sending
// is committed before the external call, the answer after it.
const (
	StepPending = "pending"
	StepSending = "sending"
	StepSent    = "sent"
	StepFailed  = "failed"
	StepUnknown = "unknown"
	StepSkipped = "skipped"
)

// Run item roles.
const (
	RunItemPrimary   = "primary"
	RunItemDuplicate = "duplicate"
)

// Activity log severities.
const (
	SeverityInfo      = "info"
	SeverityAttention = "attention"
)

var (
	// ErrRunBusy means the project already has an unfinished release run.
	ErrRunBusy = errors.New("store: project already has an unfinished release run")
	// ErrCapReached means maxReleasesPerDay (per project or global) is used up.
	ErrCapReached = errors.New("store: release cap reached")
	// ErrRunState means the run is not in a state that allows the change (CAS miss).
	ErrRunState = errors.New("store: run state does not allow this")
	// ErrStepState means the step is not in the expected state (CAS miss).
	ErrStepState = errors.New("store: step state does not allow this")
)

// autopilotEventKeep is how many activity log rows are kept (newest first).
var autopilotEventKeep = 5000

// Run is an autopilot run (API shape).
type Run struct {
	ID             int64           `json:"id"`
	Kind           string          `json:"kind"`
	ProjectID      int64           `json:"projectId"`
	State          string          `json:"state"`
	Origin         string          `json:"origin"`
	ReleaseID      int64           `json:"releaseId,omitempty"` // fix run: the release that claimed it
	Manifest       json.RawMessage `json:"manifest"`
	Version        string          `json:"version"`
	ArtifactSHA256 string          `json:"artifactSha256"`
	HeldReason     string          `json:"heldReason"`
	CreatedAt      string          `json:"createdAt"`
	UpdatedAt      string          `json:"updatedAt"`
}

// RunItem is an item a run answers for.
type RunItem struct {
	ItemID int64  `json:"itemId"`
	Role   string `json:"role"` // primary | duplicate ("" = primary)
}

// Step is one persisted step of a run.
type Step struct {
	ID          int64           `json:"id"`
	RunID       int64           `json:"runId"`
	Seq         int             `json:"seq"`
	Step        string          `json:"step"`
	Target      string          `json:"target"`
	State       string          `json:"state"`
	Attempt     int             `json:"attempt"`
	IdemKey     string          `json:"idemKey"`
	Request     json.RawMessage `json:"request"`
	ExternalRef string          `json:"externalRef"`
	Error       string          `json:"error"`
	StartedAt   string          `json:"startedAt"`
	FinishedAt  string          `json:"finishedAt"`
}

// NewStep is a step to insert. State "" = pending; a step may start skipped
// (e.g. smoke without an adapter) with Error as its note.
type NewStep struct {
	Step    string
	Target  string
	State   string
	IdemKey string
	Request json.RawMessage
	Error   string
}

// NewReleaseRun is what CreateReleaseRun inserts.
type NewReleaseRun struct {
	ProjectID int64
	Origin    string // manual | mcp | auto
	Version   string
	Manifest  json.RawMessage
	Items     []RunItem
	Steps     []NewStep // in execution order
	// MaxPerProjectPerDay / MaxGlobalPerDay cap release runs created on the same
	// UTC calendar day as Now (0 = no cap). Every release run created that day
	// counts, whatever its state: the slot is reserved at creation, so a run
	// that failed or was cancelled after a public step still used it.
	MaxPerProjectPerDay int
	MaxGlobalPerDay     int
	Now                 time.Time // zero = time.Now()
}

func terminalStep(state string) bool {
	return state == StepSent || state == StepFailed || state == StepSkipped
}

func validStep(state string) bool {
	switch state {
	case StepPending, StepSending, StepSent, StepFailed, StepUnknown, StepSkipped:
		return true
	}
	return false
}

func rawOrEmpty(b json.RawMessage) string {
	if len(b) == 0 || !json.Valid(b) {
		return "{}"
	}
	return string(b)
}

func rawJSON(s string) json.RawMessage {
	if !json.Valid([]byte(s)) {
		s = "{}"
	}
	return json.RawMessage(s)
}

const runColumns = `id, kind, project_id, state, origin, coalesce(release_id, 0), manifest_json, version, artifact_sha256,
	held_reason, created_at, updated_at`

func scanRun(sc interface{ Scan(...any) error }) (Run, error) {
	var (
		r        Run
		manifest string
	)
	if err := sc.Scan(&r.ID, &r.Kind, &r.ProjectID, &r.State, &r.Origin, &r.ReleaseID, &manifest, &r.Version,
		&r.ArtifactSHA256, &r.HeldReason, &r.CreatedAt, &r.UpdatedAt); err != nil {
		return r, err
	}
	r.Manifest = rawJSON(manifest)
	return r, nil
}

const stepColumns = `id, run_id, seq, step, target, state, attempt, idem_key, request_json, external_ref, error, started_at, finished_at`

func scanStep(sc interface{ Scan(...any) error }) (Step, error) {
	var (
		st  Step
		req string
	)
	if err := sc.Scan(&st.ID, &st.RunID, &st.Seq, &st.Step, &st.Target, &st.State, &st.Attempt, &st.IdemKey, &req,
		&st.ExternalRef, &st.Error, &st.StartedAt, &st.FinishedAt); err != nil {
		return st, err
	}
	st.Request = rawJSON(req)
	return st, nil
}

func insertStep(ctx context.Context, tx execer, runID int64, seq int, ns NewStep) (bool, error) {
	state := ns.State
	if state == "" {
		state = StepPending
	}
	if !validStep(state) {
		return false, fmt.Errorf("store: step %s: bad state %q", ns.Step, state)
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO autopilot_steps (run_id, seq, step, target, state, idem_key, request_json, error)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (run_id, step, target) DO NOTHING`,
		runID, seq, ns.Step, ns.Target, state, ns.IdemKey, rawOrEmpty(ns.Request), ns.Error)
	if err != nil {
		return false, fmt.Errorf("store: insert step %s: %w", ns.Step, err)
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// CreateReleaseRun inserts a pending release run with its items and initial
// steps (seq 1..n in the given order) in one transaction, after reserving a
// slot of the daily caps (see NewReleaseRun). An unfinished release run of the
// project → ErrRunBusy; a cap used up → ErrCapReached (wrapped with counts).
func (s *Store) CreateReleaseRun(ctx context.Context, nr NewReleaseRun) (Run, error) {
	now := nr.Now
	if now.IsZero() {
		now = time.Now()
	}
	now = now.UTC()
	dayStart := ts(now.Truncate(24 * time.Hour))
	origin := nr.Origin
	if origin == "" {
		origin = RunOriginManual
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Run{}, fmt.Errorf("store: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var busy int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM autopilot_runs WHERE project_id = ? AND kind = 'release'
		AND state IN ('pending', 'running', 'held')`, nr.ProjectID).Scan(&busy); err != nil {
		return Run{}, fmt.Errorf("store: release runs: %w", err)
	}
	if busy > 0 {
		return Run{}, ErrRunBusy
	}
	var perProject, global int
	if err := tx.QueryRowContext(ctx, `SELECT coalesce(sum(project_id = ?), 0), count(*) FROM autopilot_runs
		WHERE kind = 'release' AND created_at >= ?`, nr.ProjectID, dayStart).Scan(&perProject, &global); err != nil {
		return Run{}, fmt.Errorf("store: release cap: %w", err)
	}
	if nr.MaxPerProjectPerDay > 0 && perProject >= nr.MaxPerProjectPerDay {
		return Run{}, fmt.Errorf("%w: project %d/%d today (UTC)", ErrCapReached, perProject, nr.MaxPerProjectPerDay)
	}
	if nr.MaxGlobalPerDay > 0 && global >= nr.MaxGlobalPerDay {
		return Run{}, fmt.Errorf("%w: all projects %d/%d today (UTC)", ErrCapReached, global, nr.MaxGlobalPerDay)
	}

	at := ts(now)
	var id int64
	err = tx.QueryRowContext(ctx, `INSERT INTO autopilot_runs (kind, project_id, state, origin, manifest_json, version, created_at, updated_at)
		VALUES ('release', ?, 'pending', ?, ?, ?, ?, ?) RETURNING id`,
		nr.ProjectID, origin, rawOrEmpty(nr.Manifest), nr.Version, at, at).Scan(&id)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return Run{}, ErrRunBusy
		}
		return Run{}, fmt.Errorf("store: create release run: %w", err)
	}
	for _, it := range nr.Items {
		role := it.Role
		if role == "" {
			role = RunItemPrimary
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO autopilot_run_items (run_id, item_id, role) VALUES (?, ?, ?)
			ON CONFLICT DO NOTHING`, id, it.ItemID, role); err != nil {
			return Run{}, fmt.Errorf("store: run item %d: %w", it.ItemID, err)
		}
	}
	seq := 0
	for _, ns := range nr.Steps {
		ok, err := insertStep(ctx, tx, id, seq+1, ns)
		if err != nil {
			return Run{}, err
		}
		if ok {
			seq++
		}
	}
	r, err := scanRun(tx.QueryRowContext(ctx, `SELECT `+runColumns+` FROM autopilot_runs WHERE id = ?`, id))
	if err != nil {
		return Run{}, fmt.Errorf("store: read run: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Run{}, fmt.Errorf("store: commit release run: %w", err)
	}
	return r, nil
}

// Run returns run id (ErrNotFound if absent).
func (s *Store) Run(ctx context.Context, id int64) (Run, error) {
	r, err := scanRun(s.rd.QueryRowContext(ctx, `SELECT `+runColumns+` FROM autopilot_runs WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Run{}, ErrNotFound
	}
	if err != nil {
		return Run{}, fmt.Errorf("store: run %d: %w", id, err)
	}
	return r, nil
}

// RunFilter narrows Runs; zero fields match everything. Limit 0 = 100.
type RunFilter struct {
	ProjectID int64
	Kind      string
	State     string
	Limit     int
}

// Runs lists runs newest first.
func (s *Store) Runs(ctx context.Context, f RunFilter) ([]Run, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = 100
	}
	return s.queryRuns(ctx, `SELECT `+runColumns+` FROM autopilot_runs
		WHERE (? = 0 OR project_id = ?) AND (? = '' OR kind = ?) AND (? = '' OR state = ?)
		ORDER BY id DESC LIMIT ?`, f.ProjectID, f.ProjectID, f.Kind, f.Kind, f.State, f.State, limit)
}

// UnfinishedRuns lists runs not yet done, cancelled or failed, oldest first
// (crash resume reconciles them at startup).
func (s *Store) UnfinishedRuns(ctx context.Context) ([]Run, error) {
	return s.queryRuns(ctx, `SELECT `+runColumns+` FROM autopilot_runs
		WHERE state NOT IN ('done', 'cancelled', 'failed') ORDER BY id`)
}

func (s *Store) queryRuns(ctx context.Context, q string, args ...any) ([]Run, error) {
	rows, err := s.rd.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: runs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []Run{}
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan run: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RunSteps lists a run's steps in execution order.
func (s *Store) RunSteps(ctx context.Context, runID int64) ([]Step, error) {
	rows, err := s.rd.QueryContext(ctx, `SELECT `+stepColumns+` FROM autopilot_steps WHERE run_id = ? ORDER BY seq, id`, runID)
	if err != nil {
		return nil, fmt.Errorf("store: run steps: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []Step{}
	for rows.Next() {
		st, err := scanStep(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan step: %w", err)
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// RunItems lists a run's items (primary first, then by item id).
func (s *Store) RunItems(ctx context.Context, runID int64) ([]RunItem, error) {
	rows, err := s.rd.QueryContext(ctx, `SELECT item_id, role FROM autopilot_run_items WHERE run_id = ?
		ORDER BY role <> 'primary', item_id`, runID)
	if err != nil {
		return nil, fmt.Errorf("store: run items: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []RunItem{}
	for rows.Next() {
		var it RunItem
		if err := rows.Scan(&it.ItemID, &it.Role); err != nil {
			return nil, fmt.Errorf("store: scan run item: %w", err)
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// RunUpdate is what UpdateRun changes; nil fields stay. HeldReason nil and a
// new State other than held clears the held reason.
type RunUpdate struct {
	State          string // "" = keep
	HeldReason     *string
	Version        *string
	ArtifactSHA256 *string
	Manifest       json.RawMessage // nil = keep
}

// UpdateRun applies u to run id only while its state is one of from
// (compare-and-set). A CAS miss → ErrRunState (with the current row); an
// absent run → ErrNotFound. Finishing a release run frees the project's slot.
func (s *Store) UpdateRun(ctx context.Context, id int64, from []string, u RunUpdate) (Run, error) {
	if len(from) == 0 {
		return Run{}, fmt.Errorf("store: update run %d: no from states", id)
	}
	sets := []string{"updated_at = ?"}
	args := []any{ts(time.Now())}
	if u.State != "" {
		sets = append(sets, "state = ?")
		args = append(args, u.State)
	}
	switch {
	case u.HeldReason != nil:
		sets = append(sets, "held_reason = ?")
		args = append(args, *u.HeldReason)
	case u.State != "" && u.State != RunHeld:
		sets = append(sets, "held_reason = ''")
	}
	if u.Version != nil {
		sets = append(sets, "version = ?")
		args = append(args, *u.Version)
	}
	if u.ArtifactSHA256 != nil {
		sets = append(sets, "artifact_sha256 = ?")
		args = append(args, *u.ArtifactSHA256)
	}
	if u.Manifest != nil {
		sets = append(sets, "manifest_json = ?")
		args = append(args, rawOrEmpty(u.Manifest))
	}
	args = append(args, id)
	for _, f := range from {
		args = append(args, f)
	}
	q := `UPDATE autopilot_runs SET ` + strings.Join(sets, ", ") + ` WHERE id = ? AND state IN (?` +
		strings.Repeat(", ?", len(from)-1) + `) RETURNING ` + runColumns
	r, err := scanRun(s.db.QueryRowContext(ctx, q, args...))
	if err == nil {
		return r, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return Run{}, ErrRunBusy
		}
		return Run{}, fmt.Errorf("store: update run %d: %w", id, err)
	}
	cur, err := s.Run(ctx, id)
	if err != nil {
		return Run{}, err
	}
	return cur, ErrRunState
}

// StepUpdate is what TransitionStep changes besides the state; nil fields stay.
type StepUpdate struct {
	IncAttempt  bool // attempt++ (a new send)
	IdemKey     *string
	Request     json.RawMessage // nil = keep
	ExternalRef *string
	Error       *string
}

// TransitionStep moves step (step, target) of runID from state from to to
// (compare-and-set, so two workers never both claim a send). →sending sets
// started_at and clears finished_at; →sent|failed|skipped sets finished_at;
// →pending clears both. A CAS miss → ErrStepState (with the current row); an
// absent step → ErrNotFound.
func (s *Store) TransitionStep(ctx context.Context, runID int64, step, target, from, to string, u StepUpdate) (Step, error) {
	if !validStep(to) {
		return Step{}, fmt.Errorf("store: step %s: bad state %q", step, to)
	}
	now := ts(time.Now())
	sets := []string{"state = ?"}
	args := []any{to}
	switch {
	case to == StepSending:
		sets = append(sets, "started_at = ?", "finished_at = ''")
		args = append(args, now)
	case terminalStep(to):
		sets = append(sets, "finished_at = ?")
		args = append(args, now)
	case to == StepPending:
		sets = append(sets, "started_at = ''", "finished_at = ''")
	}
	if u.IncAttempt {
		sets = append(sets, "attempt = attempt + 1")
	}
	if u.IdemKey != nil {
		sets = append(sets, "idem_key = ?")
		args = append(args, *u.IdemKey)
	}
	if u.Request != nil {
		sets = append(sets, "request_json = ?")
		args = append(args, rawOrEmpty(u.Request))
	}
	if u.ExternalRef != nil {
		sets = append(sets, "external_ref = ?")
		args = append(args, *u.ExternalRef)
	}
	if u.Error != nil {
		sets = append(sets, "error = ?")
		args = append(args, *u.Error)
	}
	args = append(args, runID, step, target, from)
	st, err := scanStep(s.db.QueryRowContext(ctx, `UPDATE autopilot_steps SET `+strings.Join(sets, ", ")+
		` WHERE run_id = ? AND step = ? AND target = ? AND state = ? RETURNING `+stepColumns, args...))
	if err == nil {
		return st, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Step{}, fmt.Errorf("store: transition step %s: %w", step, err)
	}
	cur, err := scanStep(s.db.QueryRowContext(ctx, `SELECT `+stepColumns+` FROM autopilot_steps
		WHERE run_id = ? AND step = ? AND target = ?`, runID, step, target))
	if errors.Is(err, sql.ErrNoRows) {
		return Step{}, ErrNotFound
	}
	if err != nil {
		return Step{}, fmt.Errorf("store: step %s: %w", step, err)
	}
	return cur, ErrStepState
}

// AddSteps inserts steps known only later (e.g. gh_asset:<file>) right after
// step (afterStep, afterTarget) in their given order, shifting later steps;
// an anchor that is absent (or afterStep "") appends at the end. Steps that
// already exist are left as they are. Returns how many were inserted.
func (s *Store) AddSteps(ctx context.Context, runID int64, afterStep, afterTarget string, steps []NewStep) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("store: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM autopilot_runs WHERE id = ?`, runID).Scan(&exists); err != nil {
		return 0, fmt.Errorf("store: run %d: %w", runID, err)
	}
	if exists == 0 {
		return 0, ErrNotFound
	}
	var anchor int
	err = tx.QueryRowContext(ctx, `SELECT seq FROM autopilot_steps WHERE run_id = ? AND step = ? AND target = ?`,
		runID, afterStep, afterTarget).Scan(&anchor)
	if afterStep == "" || errors.Is(err, sql.ErrNoRows) {
		err = tx.QueryRowContext(ctx, `SELECT coalesce(max(seq), 0) FROM autopilot_steps WHERE run_id = ?`, runID).Scan(&anchor)
	}
	if err != nil {
		return 0, fmt.Errorf("store: step anchor: %w", err)
	}
	var fresh []NewStep
	for _, ns := range steps {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM autopilot_steps WHERE run_id = ? AND step = ? AND target = ?`,
			runID, ns.Step, ns.Target).Scan(&n); err != nil {
			return 0, fmt.Errorf("store: step %s: %w", ns.Step, err)
		}
		if n == 0 {
			fresh = append(fresh, ns)
		}
	}
	if len(fresh) == 0 {
		return 0, nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE autopilot_steps SET seq = seq + ? WHERE run_id = ? AND seq > ?`,
		len(fresh), runID, anchor); err != nil {
		return 0, fmt.Errorf("store: shift steps: %w", err)
	}
	added := 0
	for _, ns := range fresh {
		ok, err := insertStep(ctx, tx, runID, anchor+added+1, ns)
		if err != nil {
			return 0, err
		}
		if ok {
			added++
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("store: commit steps: %w", err)
	}
	return added, nil
}

// AutopilotEvent is one activity log row (API shape). Zero ids mean none.
type AutopilotEvent struct {
	ID        int64           `json:"id"`
	At        string          `json:"at"`
	RunID     int64           `json:"runId,omitempty"`
	ProjectID int64           `json:"projectId,omitempty"`
	ItemID    int64           `json:"itemId,omitempty"`
	Kind      string          `json:"kind"`
	Severity  string          `json:"severity"` // info | attention
	Title     string          `json:"title"`
	Detail    json.RawMessage `json:"detail"`
	ReadAt    string          `json:"readAt"`
}

const eventColumns = `id, at, coalesce(run_id, 0), coalesce(project_id, 0), coalesce(item_id, 0), kind, severity, title, detail_json, read_at`

func scanAutopilotEvent(sc interface{ Scan(...any) error }) (AutopilotEvent, error) {
	var (
		e      AutopilotEvent
		detail string
	)
	if err := sc.Scan(&e.ID, &e.At, &e.RunID, &e.ProjectID, &e.ItemID, &e.Kind, &e.Severity, &e.Title, &detail, &e.ReadAt); err != nil {
		return e, err
	}
	e.Detail = rawJSON(detail)
	return e, nil
}

// AddEvent appends e to the activity log (At "" = now, Severity "" = info)
// and prunes it to the newest 5000 rows.
func (s *Store) AddEvent(ctx context.Context, e AutopilotEvent) (AutopilotEvent, error) {
	if e.At == "" {
		e.At = ts(time.Now())
	}
	if e.Severity == "" {
		e.Severity = SeverityInfo
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return AutopilotEvent{}, fmt.Errorf("store: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	out, err := scanAutopilotEvent(tx.QueryRowContext(ctx, `INSERT INTO autopilot_events
		(at, run_id, project_id, item_id, kind, severity, title, detail_json)
		VALUES (?, nullif(?, 0), nullif(?, 0), nullif(?, 0), ?, ?, ?, ?) RETURNING `+eventColumns,
		e.At, e.RunID, e.ProjectID, e.ItemID, e.Kind, e.Severity, e.Title, rawOrEmpty(e.Detail)))
	if err != nil {
		return AutopilotEvent{}, fmt.Errorf("store: add event: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM autopilot_events WHERE id <=
		(SELECT id FROM autopilot_events ORDER BY id DESC LIMIT 1 OFFSET ?)`, autopilotEventKeep); err != nil {
		return AutopilotEvent{}, fmt.Errorf("store: prune events: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return AutopilotEvent{}, fmt.Errorf("store: commit event: %w", err)
	}
	return out, nil
}

// Events lists activity log rows newest first (limit 0 = 200).
func (s *Store) Events(ctx context.Context, unreadOnly bool, limit int) ([]AutopilotEvent, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := s.rd.QueryContext(ctx, `SELECT `+eventColumns+` FROM autopilot_events
		WHERE (? = 0 OR read_at = '') ORDER BY id DESC LIMIT ?`, unreadOnly, limit)
	if err != nil {
		return nil, fmt.Errorf("store: events: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []AutopilotEvent{}
	for rows.Next() {
		e, err := scanAutopilotEvent(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan event: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// MarkEventsRead marks the unread events ids read, or every unread event when
// all is true. Returns how many changed.
func (s *Store) MarkEventsRead(ctx context.Context, ids []int64, all bool) (int64, error) {
	if !all && len(ids) == 0 {
		return 0, nil
	}
	q := `UPDATE autopilot_events SET read_at = ? WHERE read_at = ''`
	args := []any{ts(time.Now())}
	if !all {
		q += ` AND id IN (?` + strings.Repeat(", ?", len(ids)-1) + `)` //nolint:gosec // G202: placeholders only
		for _, id := range ids {
			args = append(args, id)
		}
	}
	res, err := s.db.ExecContext(ctx, q, args...)
	if err != nil {
		return 0, fmt.Errorf("store: mark events read: %w", err)
	}
	return res.RowsAffected()
}

// UnreadEventCount returns the unread events and how many of them are attention.
func (s *Store) UnreadEventCount(ctx context.Context) (unread, attention int, err error) {
	err = s.rd.QueryRowContext(ctx, `SELECT count(*), coalesce(sum(severity = 'attention'), 0) FROM autopilot_events
		WHERE read_at = ''`).Scan(&unread, &attention)
	if err != nil {
		return 0, 0, fmt.Errorf("store: unread events: %w", err)
	}
	return unread, attention, nil
}
