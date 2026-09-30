package runner

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/UberMorgott/issuewatcher/internal/config"
	"github.com/UberMorgott/issuewatcher/internal/folders"
	"github.com/UberMorgott/issuewatcher/internal/store"
)

// Project triage (docs/ARCHITECTURE.md → Runner): a read-only agent ranks a
// project's open issues by criticality; the app checks the picks and queues a
// fix job for each of the first top-N that has no unfinished fix job.

// Triage input bounds: the agent's context is the budget.
const (
	triageBodyRunes = 400
	triageMaxPicks  = 20
)

// triageMaxIssues is how many open issues the prompt lists (a var: tests lower it).
var triageMaxIssues = 200

// TriagePick.Queue codes (the UI shows its own text; "" = below the top N).
const (
	PickQueued   = "queued"    // JobID = the new fix job
	PickExists   = "exists"    // JobID = the unfinished fix job of the issue
	PickClosed   = "closed"    // the issue closed meanwhile
	PickNoFolder = "no_folder" // the project has no usable local folder
	PickFailed   = "failed"    // queueing failed (the job log has the error)
)

// Severities, most critical first.
var severities = []string{"critical", "high", "medium", "low"}

func severityRank(s string) int {
	if i := slices.Index(severities, s); i >= 0 {
		return i
	}
	return len(severities)
}

// TriageResult is Result.Triage.
type TriageResult struct {
	Open int  `json:"open"`           // open issues given to the agent
	More bool `json:"more,omitempty"` // the project has more open issues than that
	TopN int  `json:"topN"`
	// Picks are the checked picks, most critical first, with the app's decision.
	Picks []TriagePick `json:"picks"`
	// Dropped are picks that are not among the project's open issues (or repeat one).
	Dropped []TriagePick `json:"dropped,omitempty"`
	Summary string       `json:"summary,omitempty"`
}

// checkPicks keeps the picks that name one of the open issues (first mention
// wins), severity normalized ("" when unknown), stably ordered by severity.
func checkPicks(byNumber map[int]store.TriageIssue, picks []TriagePick) (kept, dropped []TriagePick) {
	seen := map[int]bool{}
	for _, p := range picks {
		is, ok := byNumber[p.Number]
		p.Reason = clipRunes(strings.TrimSpace(p.Reason), 500)
		p.Severity = strings.ToLower(strings.TrimSpace(p.Severity))
		if severityRank(p.Severity) == len(severities) {
			p.Severity = ""
		}
		p.Queue, p.JobID, p.ItemID, p.Title = "", 0, 0, ""
		if !ok || seen[p.Number] || len(kept) == triageMaxPicks {
			dropped = append(dropped, p)
			continue
		}
		seen[p.Number] = true
		p.ItemID, p.Title = is.ItemID, is.Title
		kept = append(kept, p)
	}
	slices.SortStableFunc(kept, func(a, b TriagePick) int { return cmp.Compare(severityRank(a.Severity), severityRank(b.Severity)) })
	return kept, dropped
}

// Triage queues a triage job for project projectID with profileID ("" = the
// responder). An unfinished triage of the project → store.ErrJobExists with it.
func (r *Runner) Triage(ctx context.Context, projectID int64, profileID string) (store.Job, error) {
	cfg := r.opts.Settings().Agents
	if profileID == "" {
		profileID = cfg.Roles.Responder
	}
	if _, ok := cfg.Profile(profileID); !ok {
		return store.Job{}, coded(CodeNoProfile, fmt.Errorf("%w: no agent profile %q (Settings › Agents)", ErrBadRequest, profileID))
	}
	if _, ok := cfg.Profile(cfg.Roles.Coder); !ok {
		return store.Job{}, coded(CodeNoProfile, fmt.Errorf("%w: no coder profile for the fix jobs (Settings › Agents)", ErrBadRequest))
	}
	j, err := r.opts.Store.CreateProjectJob(ctx, projectID, flowTriage, profileID, store.OriginManual, "")
	if err != nil {
		return j, err
	}
	r.opts.OnJob(j)
	r.kick()
	return j, nil
}

// runTriage: the agent ranks the open issues read-only; the app queues fix
// jobs for the first top-N free picks. The job ends done (nothing to publish).
func (r *Runner) runTriage(ctx context.Context, j *store.Job, res *Result, log *jobLog) (string, error) {
	prof, cfg, err := r.profileFor(*j)
	if err != nil {
		return "", err
	}
	coder, ok := cfg.Profile(cfg.Roles.Coder)
	if !ok {
		return "", coded(CodeNoProfile, errors.New("no coder profile for the fix jobs (Settings › Agents)"))
	}
	t, err := r.opts.Store.TriageInput(ctx, j.ProjectID, triageMaxIssues, triageBodyRunes)
	if err != nil {
		return "", err
	}
	if _, err := r.resolveExe(prof); err != nil {
		return "", coded(CodeNoCLI, err)
	}
	topN := cfg.TriageTopNFor(t.ProjectKey)
	tr := &TriageResult{Open: len(t.Issues), More: t.More, TopN: topN, Picks: []TriagePick{}}
	res.Triage = tr
	more := map[bool]string{true: r.say(" (самые свежие)", " (most recent)")}[t.More]
	log.add(StepInfo, fmt.Sprintf(r.say("%s: открытых issues — %d%s, задачу исправления получат первые %d", "%s: %d open issues%s, top %d get a fix job"),
		t.ProjectName, len(t.Issues), more, topN))
	if len(t.Issues) == 0 {
		log.add(StepInfo, r.say("открытых issues нет: разбирать нечего", "no open issues: nothing to rank"))
		return store.JobDone, nil
	}
	in := store.JobInput{ProjectName: t.ProjectName, ProjectKey: t.ProjectKey, ProjectURL: t.ProjectURL, LocalPath: t.LocalPath}
	files, dir, err := r.readOnlyDir(*j, in, log)
	if err != nil {
		return "", err
	}
	r.phase(ctx, j, "agent")
	system, task := prompts(cfg, flowTriage, promptInput{in: in, triage: &t, topN: topN})
	agent, err := r.runAgent(ctx, agentSpec{project: j.ProjectID, repo: j.ProjectKey, profile: prof, flow: flowTriage, dir: dir, workDir: files,
		system: system, task: task, readOnly: true}, log)
	res.Agent = &agent
	if err != nil {
		if errors.Is(err, errTimeout) || errors.Is(err, ErrCancelled) {
			return "", err
		}
		return "", coded(CodeAgent, err)
	}
	if agent.Picks == nil && agent.Final != "" {
		return "", coded(CodeAgent, errors.New("the agent returned no structured ranking"))
	}
	tr.Summary = agent.Summary
	// Checked against the issues open now: the agent may have paged past the
	// prompt's list through its MCP server, and sync may have closed some.
	nums := make([]int, len(agent.Picks))
	for i, p := range agent.Picks {
		nums[i] = p.Number
	}
	open, err := r.opts.Store.OpenIssues(ctx, j.ProjectID, nums)
	if err != nil {
		return "", err
	}
	tr.Picks, tr.Dropped = checkPicks(open, agent.Picks)
	if tr.Picks == nil {
		tr.Picks = []TriagePick{}
	}
	if len(tr.Dropped) > 0 {
		nums := make([]string, len(tr.Dropped))
		for i, p := range tr.Dropped {
			nums[i] = "#" + strconv.Itoa(p.Number)
		}
		log.add(StepInfo, fmt.Sprintf(r.say("отброшены (не открыты в %s, повтор или больше %d): %s", "dropped picks (not open in %s, repeated or over %d): %s"),
			t.ProjectName, triageMaxPicks, strings.Join(nums, ", ")))
	}
	// A fix job runs in the mapped folder: without one each would fail at once.
	if st := folders.Check(t.LocalPath, t.ProjectURL); !st.Exists() {
		for i := range tr.Picks[:min(tr.TopN, len(tr.Picks))] {
			tr.Picks[i].Queue = PickNoFolder
		}
		log.add(StepInfo, fmt.Sprintf(r.say("задачи исправления не поставлены: у %s нет рабочей папки (%s)", "no fix job queued: %s has no usable local folder (%s)"), t.ProjectName, st))
		return store.JobDone, nil
	}
	// A cancel up to here cancels the triage; from here the top-N fix jobs are
	// queued as one step, never cut short halfway behind a "done" triage.
	if ctx.Err() != nil {
		return "", context.Cause(ctx)
	}
	r.queueFixes(context.WithoutCancel(ctx), *j, coder, tr, log)
	return store.JobDone, nil
}

// queueFixes queues a fix job for each pick, in rank order, until topN new
// jobs are queued; a pick whose issue already has an unfinished fix job is
// marked exists and does not count.
func (r *Runner) queueFixes(ctx context.Context, j store.Job, coder config.AgentProfile, tr *TriageResult, log *jobLog) {
	queued := 0
	for i := range tr.Picks {
		p := &tr.Picks[i]
		if queued >= tr.TopN {
			break
		}
		fj, err := r.opts.Store.CreateOpenJob(ctx, p.ItemID, flowFix, coder.ID, j.Origin, j.RuleID)
		switch {
		case err == nil:
			p.Queue, p.JobID = PickQueued, fj.ID
			queued++
			r.opts.OnJob(fj)
			log.add(StepInfo, fmt.Sprintf(r.say("#%d (%s): поставлена задача исправления %d", "#%d (%s): fix job %d queued"), p.Number, cmp.Or(p.Severity, "?"), fj.ID))
		case errors.Is(err, store.ErrJobExists):
			p.Queue, p.JobID = PickExists, fj.ID
			log.add(StepInfo, fmt.Sprintf(r.say("#%d: пропущено, задача исправления %d ещё не завершена", "#%d: skipped, fix job %d is unfinished"), p.Number, fj.ID))
		case errors.Is(err, store.ErrNotOpen):
			p.Queue = PickClosed
			log.add(StepInfo, fmt.Sprintf(r.say("#%d: пропущено, issue уже закрыт", "#%d: skipped, closed meanwhile"), p.Number))
		default:
			p.Queue = PickFailed
			log.add(StepError, fmt.Sprintf("#%d: queue fix job: %v", p.Number, err))
		}
	}
	if queued == 0 {
		log.add(StepInfo, r.say("ни одной задачи исправления не поставлено", "no fix job queued"))
	}
	r.kick()
}
