package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/folders"
)

// Job states (docs/ARCHITECTURE.md → Runner).
const (
	JobQueued      = "queued"
	JobRunning     = "running"
	JobNeedsReview = "needs_review"
	JobDone        = "done"
	JobFailed      = "failed"
	JobCancelled   = "cancelled"
)

// Job origins: queued by a click or by an automation rule.
const (
	OriginManual = "manual"
	OriginRule   = "rule"
)

// ActiveJobStates are the unfinished states; an item has at most one such job per flow.
var ActiveJobStates = []string{JobQueued, JobRunning, JobNeedsReview}

// ErrJobState means the job is not in a state that allows the change.
var ErrJobState = errors.New("store: job state does not allow this")

// ErrJobExists means the item already has an unfinished job of that flow.
var ErrJobExists = errors.New("store: item already has an unfinished job of this flow")

// Job is a queued or finished agent run (API shape).
type Job struct {
	ID         int64           `json:"id"`
	ItemID     int64           `json:"itemId"`
	ProjectID  int64           `json:"projectId"`
	Flow       string          `json:"flow"`
	State      string          `json:"state"`
	Origin     string          `json:"origin"` // manual | rule
	RuleID     string          `json:"ruleId"` // origin rule: agents.automation.rules[].id
	ProfileID  string          `json:"profileId"`
	Attempt    int             `json:"attempt"`
	Phase      string          `json:"phase"`
	Branch     string          `json:"branch"`
	Worktree   string          `json:"worktree"`
	BaseSHA    string          `json:"baseSha"`
	Error      string          `json:"error"`
	Result     json.RawMessage `json:"result"`
	CreatedAt  string          `json:"createdAt"`
	StartedAt  string          `json:"startedAt"`
	FinishedAt string          `json:"finishedAt"`
	UpdatedAt  string          `json:"updatedAt"`
	// From the item and project.
	Repo string `json:"repo"`
	// ProjectKey is the settings key platform:external_id (agents.projects, rules).
	ProjectKey string `json:"projectKey"`
	Number     int    `json:"number"`
	Title      string `json:"title"`
	ItemURL    string `json:"itemUrl"` // a project job (triage): the project's URL
	// LocalPath is the folder fix jobs run in: the project's mapped folder, or
	// for a mod page the linked code project's.
	LocalPath string `json:"localPath"`
	// Mod: the item is on a mod page (not a code project); CodeProject is the
	// linked code project's name ("" = not linked).
	Mod         bool   `json:"mod"`
	CodeProject string `json:"codeProject,omitempty"`
	// CodeProjectID is that code project's id (0 = none): the Jobs project
	// filter of a code project includes its linked mod pages' jobs.
	CodeProjectID int64 `json:"codeProjectId,omitempty"`
}

const jobColumns = `j.id, coalesce(j.item_id, 0), j.project_id, j.flow, j.state, j.origin, j.rule_id, j.profile_id, j.attempt, j.phase, j.branch,
	j.worktree, j.base_sha, j.error, j.result, j.created_at, j.started_at, j.finished_at, j.updated_at,
	p.name, s.platform || ':' || p.external_id, coalesce(i.number, 0), coalesce(i.title, ''), coalesce(i.url, p.url),
	coalesce(cp.local_path, p.local_path), s.platform <> '` + CodePlatform + `', coalesce(cp.name, ''), coalesce(cp.id, 0)`

// A project job (triage) has no item (item_id NULL): its item fields read as zero.
const jobFrom = ` FROM jobs j LEFT JOIN items i ON i.id = j.item_id JOIN projects p ON p.id = j.project_id JOIN sources s ON s.id = p.source_id` + linkJoin

func scanJob(sc interface{ Scan(...any) error }) (Job, error) {
	var (
		j      Job
		result string
	)
	err := sc.Scan(&j.ID, &j.ItemID, &j.ProjectID, &j.Flow, &j.State, &j.Origin, &j.RuleID, &j.ProfileID, &j.Attempt, &j.Phase, &j.Branch,
		&j.Worktree, &j.BaseSHA, &j.Error, &result, &j.CreatedAt, &j.StartedAt, &j.FinishedAt, &j.UpdatedAt,
		&j.Repo, &j.ProjectKey, &j.Number, &j.Title, &j.ItemURL, &j.LocalPath, &j.Mod, &j.CodeProject, &j.CodeProjectID)
	if err != nil {
		return j, err
	}
	if !json.Valid([]byte(result)) {
		result = "{}"
	}
	j.Result = json.RawMessage(result)
	return j, nil
}

// CreateJob queues a job for item itemID. origin is OriginManual ("" = manual)
// or OriginRule with ruleID. An unfinished job of the same flow for that item
// → ErrJobExists (with that job).
func (s *Store) CreateJob(ctx context.Context, itemID int64, flow, profileID, origin, ruleID string) (Job, error) {
	return s.createJob(ctx, itemID, flow, profileID, origin, ruleID, false)
}

// ErrNotOpen means the item is no longer open.
var ErrNotOpen = errors.New("store: item is not open")

// CreateOpenJob is CreateJob for an item that must still be open when the job
// is inserted (checked in the same statement): a closed item → ErrNotOpen.
func (s *Store) CreateOpenJob(ctx context.Context, itemID int64, flow, profileID, origin, ruleID string) (Job, error) {
	return s.createJob(ctx, itemID, flow, profileID, origin, ruleID, true)
}

func (s *Store) createJob(ctx context.Context, itemID int64, flow, profileID, origin, ruleID string, openOnly bool) (Job, error) {
	if origin == "" {
		origin = OriginManual
	}
	var id int64
	err := s.db.QueryRowContext(ctx, `INSERT INTO jobs (item_id, project_id, flow, profile_id, origin, rule_id)
		SELECT id, project_id, ?, ?, ?, ? FROM items WHERE id = ? AND (? = 0 OR status = 'open')
		ON CONFLICT DO NOTHING RETURNING id`, flow, profileID, origin, ruleID, itemID, openOnly).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		var status string
		err := s.rd.QueryRowContext(ctx, `SELECT status FROM items WHERE id = ?`, itemID).Scan(&status)
		if errors.Is(err, sql.ErrNoRows) {
			return Job{}, ErrNotFound
		}
		if err != nil {
			return Job{}, fmt.Errorf("store: create job: %w", err)
		}
		if openOnly && status != "open" {
			return Job{}, ErrNotOpen
		}
		existing, err := s.activeJob(ctx, itemID, flow)
		if err != nil {
			return Job{}, err
		}
		return existing, ErrJobExists
	}
	if err != nil {
		return Job{}, fmt.Errorf("store: create job: %w", err)
	}
	return s.Job(ctx, id)
}

// FlowTriage is the project job that ranks a project's open issues (no item).
const FlowTriage = "triage"

// CreateProjectJob queues a project job (flow triage, no item) for project
// projectID. An unfinished triage of that project → ErrJobExists (with it).
func (s *Store) CreateProjectJob(ctx context.Context, projectID int64, flow, profileID, origin, ruleID string) (Job, error) {
	if origin == "" {
		origin = OriginManual
	}
	var id int64
	err := s.db.QueryRowContext(ctx, `INSERT INTO jobs (item_id, project_id, flow, profile_id, origin, rule_id)
		SELECT NULL, id, ?, ?, ?, ? FROM projects WHERE id = ?
		ON CONFLICT DO NOTHING RETURNING id`, flow, profileID, origin, ruleID, projectID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		existing, err := scanJob(s.rd.QueryRowContext(ctx, "SELECT "+jobColumns+jobFrom+
			` WHERE j.project_id = ? AND j.flow = ? AND j.item_id IS NULL AND j.state IN ('queued', 'running', 'needs_review')`, projectID, flow))
		if errors.Is(err, sql.ErrNoRows) {
			return Job{}, ErrNotFound // no such project
		}
		if err != nil {
			return Job{}, fmt.Errorf("store: active project job: %w", err)
		}
		return existing, ErrJobExists
	}
	if err != nil {
		return Job{}, fmt.Errorf("store: create project job: %w", err)
	}
	return s.Job(ctx, id)
}

// ModItemRef numbers a linked mod page's item in a code project's triage:
// ModItemRef + item id, so it never collides with the repository's issue numbers.
const ModItemRef = 10_000_000

// triageNumber is an item's triage number in project ?2's triage (?4 = ModItemRef).
const triageNumber = `CASE WHEN i.project_id = ?2 THEN i.number ELSE ?4 + i.id END`

// TriageIssue is one open issue as the triage agent sees it: the project's own
// issues and the open reports of its linked mod pages.
type TriageIssue struct {
	ItemID    int64
	Number    int    // issue number; ModItemRef + item id for a mod page's report
	Source    string // platform of a linked mod page's report, "" = the project's own issue
	Title     string
	Body      string // clipped
	Author    string
	Labels    []string
	Comments  int
	CreatedAt string
	UpdatedAt string
}

// TriageInput is a project and its open issues (most recently updated first).
type TriageInput struct {
	ProjectName string
	ProjectKey  string // platform:external_id (settings key)
	ProjectURL  string
	LocalPath   string
	Issues      []TriageIssue
	More        bool // more open issues than the limit
}

// OpenIssues returns the project's issues (and linked mod page reports, by
// triage number) that are open now among numbers, by number (ItemID, Number and Title set).
func (s *Store) OpenIssues(ctx context.Context, projectID int64, numbers []int) (map[int]TriageIssue, error) {
	nums, err := json.Marshal(numbers)
	if err != nil {
		return nil, err
	}
	rows, err := s.rd.QueryContext(ctx, `SELECT id, n, title FROM (SELECT i.id, `+triageNumber+` AS n, i.title, i.status
		FROM items i WHERE `+scopeItem2+`)
		WHERE status = 'open' AND n IN (SELECT value FROM json_each(?1))`, string(nums), projectID, nil, ModItemRef)
	if err != nil {
		return nil, fmt.Errorf("store: open issues: %w", err)
	}
	defer func() { _ = rows.Close() }()
	open := map[int]TriageIssue{}
	for rows.Next() {
		var t TriageIssue
		if err := rows.Scan(&t.ItemID, &t.Number, &t.Title); err != nil {
			return nil, fmt.Errorf("store: scan open issue: %w", err)
		}
		open[t.Number] = t
	}
	return open, rows.Err()
}

// TriageInput loads project projectID with up to limit open issues, bodies
// clipped to bodyRunes characters.
func (s *Store) TriageInput(ctx context.Context, projectID int64, limit, bodyRunes int) (TriageInput, error) {
	var in TriageInput
	err := s.rd.QueryRowContext(ctx, `SELECT p.name, `+projectKeySQL+`, p.url, p.local_path
		FROM projects p JOIN sources s ON s.id = p.source_id WHERE p.id = ?`, projectID).
		Scan(&in.ProjectName, &in.ProjectKey, &in.ProjectURL, &in.LocalPath)
	if errors.Is(err, sql.ErrNoRows) {
		return in, ErrNotFound
	}
	if err != nil {
		return in, fmt.Errorf("store: triage input: %w", err)
	}
	rows, err := s.rd.QueryContext(ctx, `SELECT i.id, `+triageNumber+`, CASE WHEN i.project_id = ?2 THEN '' ELSE s.platform END,
		i.title, substr(i.body, 1, ?1), i.author, i.labels,
		(SELECT count(*) FROM comments c WHERE c.item_id = i.id), i.created_at, i.updated_at
		FROM items i JOIN projects p ON p.id = i.project_id JOIN sources s ON s.id = p.source_id
		WHERE `+scopeItem2+` AND (i.project_id = ?2 OR p.active = 1) AND i.status = 'open'
		ORDER BY i.updated_at DESC, i.id DESC LIMIT ?3`, bodyRunes, projectID, limit+1, ModItemRef)
	if err != nil {
		return in, fmt.Errorf("store: triage issues: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			t      TriageIssue
			labels string
		)
		if err := rows.Scan(&t.ItemID, &t.Number, &t.Source, &t.Title, &t.Body, &t.Author, &labels, &t.Comments, &t.CreatedAt, &t.UpdatedAt); err != nil {
			return in, fmt.Errorf("store: scan triage issue: %w", err)
		}
		if err := json.Unmarshal([]byte(labels), &t.Labels); err != nil || t.Labels == nil {
			t.Labels = []string{}
		}
		if len(in.Issues) == limit {
			in.More = true
			break
		}
		in.Issues = append(in.Issues, t)
	}
	return in, rows.Err()
}

func (s *Store) activeJob(ctx context.Context, itemID int64, flow string) (Job, error) {
	j, err := scanJob(s.rd.QueryRowContext(ctx, "SELECT "+jobColumns+jobFrom+
		` WHERE j.item_id = ? AND j.flow = ? AND j.state IN ('queued', 'running', 'needs_review')`, itemID, flow))
	if errors.Is(err, sql.ErrNoRows) {
		return j, ErrNotFound
	}
	if err != nil {
		return j, fmt.Errorf("store: active job: %w", err)
	}
	return j, nil
}

// Job returns job id.
func (s *Store) Job(ctx context.Context, id int64) (Job, error) {
	j, err := scanJob(s.rd.QueryRowContext(ctx, "SELECT "+jobColumns+jobFrom+" WHERE j.id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return j, ErrNotFound
	}
	if err != nil {
		return j, fmt.Errorf("store: job %d: %w", id, err)
	}
	return j, nil
}

// JobFilter selects jobs for the Jobs page (newest first).
type JobFilter struct {
	State     string // a job state, "active" (queued/running/needs_review) or "" (all)
	Flow      string // fix | reply | label | ""
	Origin    string // manual | rule | ""
	ProjectID int64
	ItemID    int64
	Cursor    string // older than this row
	Limit     int
}

// JobChunk is one slice of the job list.
type JobChunk struct {
	Items      []Job  `json:"items"`
	NextCursor string `json:"nextCursor"`
	More       bool   `json:"more"`
	Total      *int   `json:"total,omitempty"` // first chunk only
}

// Jobs returns a chunk of jobs, newest first.
func (s *Store) Jobs(ctx context.Context, f JobFilter) (JobChunk, error) {
	limit := clampLimit(f.Limit)
	var (
		where []string
		args  []any
	)
	switch {
	case f.State == "active":
		where = append(where, "j.state IN ('queued', 'running', 'needs_review')")
	case f.State != "":
		where, args = append(where, "j.state = ?"), append(args, f.State)
	}
	if f.Flow != "" {
		where, args = append(where, "j.flow = ?"), append(args, f.Flow)
	}
	if f.Origin != "" {
		where, args = append(where, "j.origin = ?"), append(args, f.Origin)
	}
	if f.ProjectID != 0 {
		// A code project's group: its own jobs and its linked mod pages' (as the live filter).
		where = append(where, "(j.project_id = ? OR j.project_id IN (SELECT mod_project_id FROM project_links WHERE code_project_id = ?))")
		args = append(args, f.ProjectID, f.ProjectID)
	}
	if f.ItemID != 0 {
		where, args = append(where, "j.item_id = ?"), append(args, f.ItemID)
	}
	cond := ""
	if len(where) > 0 {
		cond = " WHERE " + strings.Join(where, " AND ")
	}
	chunk := JobChunk{Items: []Job{}}
	if f.Cursor == "" {
		var n int
		if err := s.rd.QueryRowContext(ctx, "SELECT count(*) FROM jobs j"+cond, args...).Scan(&n); err != nil {
			return chunk, fmt.Errorf("store: count jobs: %w", err)
		}
		chunk.Total = &n
	} else {
		k, err := decodeCursor(f.Cursor)
		if err != nil {
			return chunk, err
		}
		if cond == "" {
			cond = " WHERE j.id < ?"
		} else {
			cond += " AND j.id < ?"
		}
		args = append(args, k.ID)
	}
	rows, err := s.rd.QueryContext(ctx, "SELECT "+jobColumns+jobFrom+cond+" ORDER BY j.id DESC LIMIT ?", append(args, limit+1)...)
	if err != nil {
		return chunk, fmt.Errorf("store: jobs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return chunk, fmt.Errorf("store: scan job: %w", err)
		}
		chunk.Items = append(chunk.Items, j)
	}
	if err := rows.Err(); err != nil {
		return chunk, fmt.Errorf("store: jobs: %w", err)
	}
	if len(chunk.Items) > limit {
		chunk.Items, chunk.More = chunk.Items[:limit], true
	}
	if n := len(chunk.Items); n > 0 {
		chunk.NextCursor = encodeCursor(chunk.Items[n-1].ID, chunk.Items[n-1].ID)
	}
	return chunk, nil
}

// JobsInState lists the jobs in any of states, oldest first (the runner's queue).
func (s *Store) JobsInState(ctx context.Context, states ...string) ([]Job, error) {
	if len(states) == 0 {
		return nil, nil
	}
	args := make([]any, len(states))
	for i, st := range states {
		args[i] = st
	}
	rows, err := s.rd.QueryContext(ctx, "SELECT "+jobColumns+jobFrom+" WHERE j.state IN (?"+strings.Repeat(",?", len(states)-1)+") ORDER BY j.id", args...) //nolint:gosec // G202: placeholders only
	if err != nil {
		return nil, fmt.Errorf("store: jobs in state: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan job: %w", err)
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// JobChange is a partial update of a job row; nil fields stay.
type JobChange struct {
	State    *string
	Phase    *string
	Branch   *string
	Worktree *string
	BaseSHA  *string
	Error    *string
	Result   json.RawMessage // nil = keep
	// NextAttempt increments attempt and clears the run fields (retry).
	NextAttempt bool
	Started     bool // started_at = now, finished_at = ''
	Finished    bool // finished_at = now
}

// UpdateJob applies c to job id when its state is one of from (none = any) and
// returns the new row; otherwise ErrJobState (with the current row).
func (s *Store) UpdateJob(ctx context.Context, id int64, from []string, c JobChange) (Job, error) {
	now := ts(time.Now())
	set := []string{"updated_at = ?"}
	args := []any{now}
	add := func(col string, v any) {
		set = append(set, col+" = ?")
		args = append(args, v)
	}
	for col, v := range map[string]*string{
		"state": c.State, "phase": c.Phase, "branch": c.Branch, "worktree": c.Worktree, "base_sha": c.BaseSHA, "error": c.Error,
	} {
		if v != nil {
			add(col, *v)
		}
	}
	if c.Result != nil {
		add("result", string(c.Result))
	}
	if c.NextAttempt {
		set = append(set, "attempt = attempt + 1")
	}
	if c.Started {
		add("started_at", now)
		add("finished_at", "")
	}
	if c.Finished {
		add("finished_at", now)
		defer folders.Invalidate() // a fix may have created, cloned or changed a folder
	}
	q := "UPDATE jobs SET " + strings.Join(set, ", ") + " WHERE id = ?" //nolint:gosec // G202: fixed column names, bound values
	args = append(args, id)
	if len(from) > 0 {
		q += " AND state IN (?" + strings.Repeat(",?", len(from)-1) + ")"
		for _, st := range from {
			args = append(args, st)
		}
	}
	res, err := s.db.ExecContext(ctx, q, args...)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return Job{}, ErrJobExists
		}
		return Job{}, fmt.Errorf("store: update job: %w", err)
	}
	j, gerr := s.Job(ctx, id)
	if gerr != nil {
		return j, gerr
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return j, ErrJobState
	}
	return j, nil
}

// RecoverJobs marks jobs left running by a previous process as failed
// ("interrupted"): an agent may have been half way, so nothing restarts on its
// own. Queued jobs stay queued.
func (s *Store) RecoverJobs(ctx context.Context) ([]Job, error) {
	running, err := s.JobsInState(ctx, JobRunning)
	if err != nil {
		return nil, err
	}
	out := make([]Job, 0, len(running))
	for _, j := range running {
		nj, err := s.UpdateJob(ctx, j.ID, []string{JobRunning}, JobChange{
			State: new(JobFailed), Phase: new(""), Error: new("interrupted: the app stopped while the job was running"), Finished: true,
		})
		if err != nil && !errors.Is(err, ErrJobState) {
			return out, err
		}
		out = append(out, nj)
	}
	return out, nil
}

// ClosedDirectFixes lists direct-mode fix jobs (needs_review, or done after a
// push) whose issue is closed on the platform now but not marked so yet.
func (s *Store) ClosedDirectFixes(ctx context.Context) ([]Job, error) {
	rows, err := s.rd.QueryContext(ctx, "SELECT "+jobColumns+jobFrom+` WHERE j.state IN ('needs_review', 'done') AND j.flow = 'fix'
		AND i.status = 'closed' AND json_valid(j.result) AND json_extract(j.result, '$.mode') = 'direct'
		AND coalesce(json_extract(j.result, '$.local.closed'), 0) = 0 ORDER BY j.id`)
	if err != nil {
		return nil, fmt.Errorf("store: closed direct fixes: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan job: %w", err)
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// LatestJobs maps item id → its newest job (issue row badges).
func (s *Store) LatestJobs(ctx context.Context, itemIDs []int64) (map[int64]JobBadge, error) {
	out := map[int64]JobBadge{}
	if len(itemIDs) == 0 {
		return out, nil
	}
	ids := slices.Compact(slices.Sorted(slices.Values(itemIDs)))
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	const cols = `j.item_id, j.id, j.flow, j.state, j.started_at,
		CASE WHEN json_valid(j.result) THEN coalesce(json_extract(j.result, '$.local.outcome'), '') ELSE '' END`
	q := "SELECT " + cols + " FROM jobs j WHERE j.id IN (SELECT max(id) FROM jobs WHERE item_id IN (?" + strings.Repeat(",?", len(ids)-1) + ") GROUP BY item_id)" //nolint:gosec // G202: placeholders only
	rows, err := s.rd.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: latest jobs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			item int64
			b    JobBadge
		)
		if err := rows.Scan(&item, &b.ID, &b.Flow, &b.State, &b.StartedAt, &b.Outcome); err != nil {
			return nil, fmt.Errorf("store: latest jobs: %w", err)
		}
		out[item] = b
	}
	return out, rows.Err()
}

// JobBadge is the newest job of an item, shown on its row.
type JobBadge struct {
	ID        int64  `json:"id"`
	Flow      string `json:"flow"`
	State     string `json:"state"`
	StartedAt string `json:"startedAt"`
	Outcome   string `json:"outcome"` // direct fix: runner LocalResult.Outcome; "" otherwise
}

// JobInput is what a job needs to build its prompt and workspace.
type JobInput struct {
	ItemExternalID string
	Platform       string
	Number         int
	Title          string
	Body           string
	URL            string
	Author         string
	ProjectName    string // owner/repo
	ProjectKey     string // platform:external_id (settings key)
	// CodeKey is the settings key of the project whose code a fix changes: the
	// linked code project's for a mod page, else ProjectKey. Mode, verify and
	// the project prompt of code work follow it.
	CodeKey string
	// ProjectURL and LocalPath are where the code lives: the project's own, or
	// for a linked mod page the code project's.
	ProjectURL string
	LocalPath  string
	Labels     []string
	Comments   []Comment
	// Mod: the item is on a mod page; CodeProject is the linked code project's
	// name (owner/repo, "" = not linked).
	Mod         bool
	CodeProject string
	// Mine: the item was written by the synced account (the owner).
	Mine bool
}

// CodeRepo is the repository (owner/repo) the item's code lives in: the
// project itself, or a mod page's linked code project.
func (in JobInput) CodeRepo() string {
	if in.Mod {
		return in.CodeProject
	}
	return in.ProjectName
}

// SetItemLabels stores item itemID's labels as the platform reported them
// after labels were added (the next sync writes the same list).
func (s *Store) SetItemLabels(ctx context.Context, itemID int64, labels []string) error {
	if labels == nil {
		labels = []string{}
	}
	lj, err := json.Marshal(labels)
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, `UPDATE items SET labels = ? WHERE id = ?`, string(lj), itemID)
	if err != nil {
		return fmt.Errorf("store: set labels: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// JobInput loads item itemID with its project and every comment.
func (s *Store) JobInput(ctx context.Context, itemID int64) (JobInput, error) {
	var in JobInput
	var labels string
	err := s.rd.QueryRowContext(ctx, `SELECT i.external_id, s.platform, i.number, i.title, i.body, i.url, i.author, i.labels,
		p.name, `+projectKeySQL+`, coalesce((SELECT platform FROM sources WHERE id = cp.source_id) || ':' || cp.external_id, `+projectKeySQL+`),
		`+folderCols+`, s.platform <> '`+CodePlatform+`', coalesce(cp.name, ''),
		i.author = nullif(s.account, '') IS 1
		FROM items i JOIN projects p ON p.id = i.project_id JOIN sources s ON s.id = i.source_id`+linkJoin+`
		WHERE i.id = ?`, itemID).Scan(&in.ItemExternalID, &in.Platform, &in.Number, &in.Title, &in.Body, &in.URL, &in.Author, &labels,
		&in.ProjectName, &in.ProjectKey, &in.CodeKey, &in.LocalPath, &in.ProjectURL, &in.Mod, &in.CodeProject, &in.Mine)
	if errors.Is(err, sql.ErrNoRows) {
		return in, ErrNotFound
	}
	if err != nil {
		return in, fmt.Errorf("store: job input: %w", err)
	}
	if err := json.Unmarshal([]byte(labels), &in.Labels); err != nil || in.Labels == nil {
		in.Labels = []string{}
	}
	cursor := ""
	for {
		c, err := s.Comments(ctx, itemID, cursor, 200)
		if err != nil {
			return in, err
		}
		in.Comments = append(in.Comments, c.Items...)
		if !c.More {
			return in, nil
		}
		cursor = c.NextCursor
	}
}

// ItemNonBug is what item itemID is when it is not a bug: its newest fix
// job's non-bug outcome (feedback | question | suggestion), else — no fix job
// said either way — its newest autopilot fix run's triage kind (question,
// feedback; feature → suggestion). "" = a bug or not known.
func (s *Store) ItemNonBug(ctx context.Context, itemID int64) (string, error) {
	var outcome string
	err := s.rd.QueryRowContext(ctx, `SELECT coalesce(json_extract(result, '$.local.outcome'), '') FROM jobs
		WHERE item_id = ? AND flow = 'fix' AND json_valid(result) AND state IN ('needs_review', 'done') ORDER BY id DESC LIMIT 1`, itemID).Scan(&outcome)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("store: item fix outcome: %w", err)
	}
	switch outcome {
	case "feedback", "question", "suggestion":
		return outcome, nil
	case "fixed_local", "pushed", "closed", "changed_folder", "not_reproduced", "needs_info":
		return "", nil // the fix agent judged it a bug
	}
	var kind string
	err = s.rd.QueryRowContext(ctx, `SELECT coalesce(json_extract(manifest_json, '$.triage.kind'), '') FROM autopilot_runs WHERE kind = 'fix'
		AND json_valid(manifest_json) AND id IN (SELECT run_id FROM autopilot_run_items WHERE item_id = ?) ORDER BY id DESC LIMIT 1`, itemID).Scan(&kind)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("store: item triage kind: %w", err)
	}
	switch kind {
	case "feedback", "question":
		return kind, nil
	case "feature":
		return "suggestion", nil
	}
	return "", nil
}
