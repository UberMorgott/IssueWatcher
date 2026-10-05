package release

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/UberMorgott/issuewatcher/internal/config"
	"github.com/UberMorgott/issuewatcher/internal/release/source"
	"github.com/UberMorgott/issuewatcher/internal/replystyle"
	"github.com/UberMorgott/issuewatcher/internal/store"
)

// Fix runs (docs/AUTOPILOT.md → Fix run state machine): an inbox event starts
// one per item; fix (the autopilot direct fix job, "Refs #N") → verify (the
// static gate on the fix head + diff limits) → push (trusted push from the
// app-owned mirror, fast-forward, only the fix's own commits) → pushed, where
// the coalescing timer's release run claims it → released.

// FixManifest is a fix run's input (store.Run.Manifest); the fix step fills
// the job's git facts once.
type FixManifest struct {
	ProjectID int64  `json:"projectId"` // the code project
	Project   string `json:"project"`   // github:owner/repo
	Repo      string `json:"repo"`      // owner/repo
	RepoURL   string `json:"repoUrl"`
	Folder    string `json:"folder"`
	ItemID    int64  `json:"itemId"`
	Event     string `json:"event"` // the inbox event kind that started it
	Number    int    `json:"number"`
	ItemURL   string `json:"itemUrl"`

	JobID    int64    `json:"jobId,omitempty"`
	Branch   string   `json:"branch,omitempty"` // the default branch the fix is on
	StartSHA string   `json:"startSha,omitempty"`
	HeadSHA  string   `json:"headSha,omitempty"`
	Commits  []string `json:"commits,omitempty"` // StartSHA..HeadSHA

	// Auto-triage (Phase 4): the classify verdict, the path it took and, for a
	// duplicate, the original item with its run (and the release that fixed it).
	Triage        *Classification `json:"triage,omitempty"`
	Outcome       string          `json:"outcome,omitempty"` // "" = fix → push; answered | duplicate | needs_info | ignored
	OutcomeReason string          `json:"outcomeReason,omitempty"`
	Original      int64           `json:"original,omitempty"`
	OriginalRun   int64           `json:"originalRun,omitempty"`
	ReleaseRun    int64           `json:"releaseRun,omitempty"`
	Notes         string          `json:"notes,omitempty"` // the fixing agent's note when it needs details
}

// DiffCheck is the verify step's diff limits result (part of its external ref).
type DiffCheck struct {
	Lines   int      `json:"lines"`
	Files   int      `json:"files"`
	Flagged []string `json:"flagged,omitempty"` // paths that need review
	Deleted []string `json:"deleted,omitempty"`
}

func fixManifestOf(r store.Run) (*FixManifest, error) {
	var m FixManifest
	if err := json.Unmarshal(r.Manifest, &m); err != nil {
		return nil, fmt.Errorf("release: fix run %d manifest: %w", r.ID, err)
	}
	return &m, nil
}

// fixSteps are a new fix run's steps.
func fixSteps() []store.NewStep {
	return []store.NewStep{{Step: StepFix}, {Step: StepVerify}, {Step: StepPush}}
}

// asRelease is the manifest view the shared git helpers take (folder, branch, remote).
func (m *FixManifest) asRelease() *Manifest {
	return &Manifest{ProjectID: m.ProjectID, Project: m.Project, Repo: m.Repo, RepoURL: m.RepoURL, Folder: m.Folder, Branch: m.Branch}
}

// --- inbox --------------------------------------------------------------------

// consumeInbox turns pending inbox events into fix runs (or attaches them to
// the item's open one); events of projects without autopilot autoFix, of
// closed items, or matching no fix rule are dropped with the reason.
func (e *Engine) consumeInbox(ctx context.Context, cfg config.Settings) {
	evs, err := e.d.Store.PendingInbox(ctx, 100)
	if err != nil {
		e.d.Log.Error("autopilot: inbox", "err", err)
		return
	}
	for _, ev := range evs {
		if outcome := e.consumeOne(ctx, cfg, ev); outcome != "" {
			if _, err := e.d.Store.DropInbox(ctx, ev.ID, outcome); err != nil {
				e.d.Log.Error("autopilot: drop inbox event", "event", ev.ID, "err", err)
			}
		}
	}
}

// consumeOne handles one event; a non-empty outcome drops it.
func (e *Engine) consumeOne(ctx context.Context, cfg config.Settings, ev store.InboxEvent) string {
	code, err := e.d.Store.ItemCodeProject(ctx, ev.ItemID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return "gone"
	case err != nil:
		e.d.Log.Error("autopilot: inbox item", "item", ev.ItemID, "err", err)
		return ""
	case code.CodeID == 0:
		return "no_code_project"
	}
	ap := cfg.Agents.AutopilotFor(code.CodeKey)
	if !ap.Enabled || !ap.AutoFix {
		return "off"
	}
	if e.d.Fixer == nil {
		return "no_fixer"
	}
	create, why := code.Open, "closed"
	if create && ev.Kind == string(store.EventNewComment) {
		// A comment starts a fix only on an item autopilot never fixed (a
		// reporter's "thanks" after the release must not start another), or
		// when its run waits for the reporter's details (it is looked at again).
		if !e.reporterAnswered(ctx, ev.ItemID) {
			if n, err := e.d.Store.ItemFixRuns(ctx, ev.ItemID); err != nil || n > 0 {
				create, why = false, "has_fix_run"
			}
		}
	}
	triage := e.triageOn(code.CodeKey)
	if create && !triage {
		// Without auto-triage the project's fix rules decide (with it, the
		// classify verdict does; rules then only override / filter).
		ok, _, err := e.d.Fixer.AutopilotMatch(ctx, ev.ItemID, ev.Kind)
		if err != nil {
			e.d.Log.Error("autopilot: fix rule", "item", ev.ItemID, "err", err)
			return ""
		}
		create, why = ok, "no_fix_rule"
	}
	steps := fixSteps()
	if triage {
		steps = fixStepsTriage()
	}
	repo, err := e.d.Store.Repo(ctx, code.CodeID)
	if err != nil {
		e.d.Log.Error("autopilot: code project", "project", code.CodeID, "err", err)
		return ""
	}
	m := FixManifest{ProjectID: repo.ID, Project: repo.Key, Repo: strings.TrimPrefix(repo.Key, "github:"), RepoURL: repo.URL,
		Folder: repo.LocalPath, ItemID: ev.ItemID, Event: ev.Kind, Number: code.Number, ItemURL: code.URL}
	b, _ := json.Marshal(m)
	run, attached, err := e.d.Store.ConsumeInbox(ctx, ev.ID, store.NewFixRun{ProjectID: repo.ID, Manifest: b, Steps: steps, Create: create})
	switch {
	case errors.Is(err, store.ErrNotFound):
		return why
	case errors.Is(err, store.ErrRunState):
		return "" // consumed meanwhile
	case err != nil:
		e.d.Log.Error("autopilot: consume inbox", "event", ev.ID, "err", err)
		return ""
	}
	e.d.OnChange(run.ID)
	if !attached {
		e.itemEvent(ctx, run, ev.ItemID, "fix.started", store.SeverityInfo, fmt.Sprintf("Автопилот: исправление #%d начато", code.Number),
			map[string]any{"event": ev.Kind, "url": code.URL})
	}
	return ""
}

// --- fix runs -----------------------------------------------------------------

// advanceFixRuns continues every pending / running fix run not already
// running in this process (one goroutine each).
func (e *Engine) advanceFixRuns(ctx context.Context) {
	for _, state := range []string{store.RunPending, store.RunRunning} {
		runs, err := e.d.Store.Runs(ctx, store.RunFilter{Kind: store.RunKindFix, State: state, Limit: 500})
		if err != nil {
			e.d.Log.Error("autopilot: fix runs", "err", err)
			return
		}
		for _, r := range slices.Backward(runs) { //nolint:contextcheck // oldest first; fix runs outlive the tick (the engine's context)
			e.launchFix(r.ID)
		}
	}
}

// launchFix runs AdvanceFix for id in the background unless it already runs.
func (e *Engine) launchFix(id int64) {
	e.mu.Lock()
	_, busy := e.active[id]
	ctx := e.ctx
	e.mu.Unlock()
	if !busy {
		e.wg.Go(func() { e.AdvanceFix(ctx, id) })
	}
}

// AdvanceFix moves fix run id forward as far as it can go now (synchronously).
func (e *Engine) AdvanceFix(ctx context.Context, id int64) {
	a, ok := e.begin(id)
	if !ok {
		return
	}
	defer e.end(id, a)
	for range 12 {
		progressed, err := e.fixStep(ctx, id)
		if err != nil {
			if ctx.Err() == nil {
				e.d.Log.Error("autopilot: fix run", "run", id, "err", err)
			}
			return
		}
		if !progressed {
			return
		}
	}
}

// fixStep runs the fix run's next step once; progressed = call again.
func (e *Engine) fixStep(ctx context.Context, id int64) (bool, error) {
	run, err := e.d.Store.Run(ctx, id)
	if err != nil {
		return false, err
	}
	if run.State == store.RunPending {
		if run, err = e.d.Store.UpdateRun(ctx, id, []string{store.RunPending}, store.RunUpdate{State: store.RunRunning}); err != nil {
			return false, err
		}
		e.d.OnChange(id)
	}
	if run.State != store.RunRunning {
		return false, nil
	}
	m, err := fixManifestOf(run)
	if err != nil {
		return false, err
	}
	steps, err := e.d.Store.RunSteps(ctx, id)
	if err != nil {
		return false, err
	}
	var st *store.Step
	for i := range steps {
		if steps[i].State != store.StepSent && steps[i].State != store.StepSkipped {
			st = &steps[i]
			break
		}
	}
	if st == nil {
		return false, e.fixFinished(ctx, run, m)
	}
	switch st.State {
	case store.StepFailed, store.StepUnknown:
		e.holdFix(ctx, run, m, "failed:"+stepName(*st), st.Error)
		return false, nil
	}
	switch st.Step {
	case StepTriage:
		return e.triageStep(ctx, run, m, *st)
	case StepReply:
		return e.fixReplyStep(ctx, run, m, *st)
	case StepFix:
		return e.fixJobStep(ctx, run, m, *st)
	case StepVerify:
		return e.verifyStep(ctx, run, m, *st)
	case StepPush:
		return e.fixPushStep(ctx, run, m, *st)
	}
	return false, fmt.Errorf("fix run %d: unknown step %s", id, st.Step)
}

// holdFix holds fix run r with reason and records an attention event.
func (e *Engine) holdFix(ctx context.Context, r store.Run, m *FixManifest, reason, detail string) {
	ctx = context.WithoutCancel(ctx)
	nr, err := e.d.Store.UpdateRun(ctx, r.ID, []string{store.RunPending, store.RunRunning}, store.RunUpdate{State: store.RunHeld, HeldReason: &reason})
	if err != nil {
		e.d.Log.Error("autopilot: hold fix run", "run", r.ID, "err", err)
		return
	}
	e.d.OnChange(r.ID)
	e.itemEvent(ctx, nr, m.ItemID, "fix.held", store.SeverityAttention, fmt.Sprintf("Исправление #%d остановлено: %s", m.Number, reason),
		map[string]any{"reason": reason, "detail": tail(detail, 2000), "url": m.ItemURL})
}

// endFix finishes fix run r as state (cancelled / failed) with an info event.
func (e *Engine) endFix(ctx context.Context, r store.Run, m *FixManifest, state, why string) {
	ctx = context.WithoutCancel(ctx)
	nr, err := e.d.Store.UpdateRun(ctx, r.ID, []string{store.RunPending, store.RunRunning, store.RunHeld}, store.RunUpdate{State: state})
	if err != nil {
		e.d.Log.Error("autopilot: end fix run", "run", r.ID, "err", err)
		return
	}
	e.d.OnChange(r.ID)
	e.itemEvent(ctx, nr, m.ItemID, "fix."+state, store.SeverityInfo, fmt.Sprintf("Исправление #%d: %s", m.Number, why), map[string]any{"reason": why})
}

func (e *Engine) setFixManifest(ctx context.Context, r store.Run, m *FixManifest) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	_, err = e.d.Store.UpdateRun(context.WithoutCancel(ctx), r.ID, []string{store.RunRunning}, store.RunUpdate{Manifest: b})
	return err
}

// otherFixInFlight: another fix run of the project has its fix committed but
// not pushed yet. Fix runs of one folder go one at a time, so each verify and
// push sees exactly its own commits on top of the remote.
func (e *Engine) otherFixInFlight(ctx context.Context, r store.Run) bool {
	for _, state := range []string{store.RunRunning, store.RunHeld} {
		runs, err := e.d.Store.Runs(ctx, store.RunFilter{Kind: store.RunKindFix, ProjectID: r.ProjectID, State: state, Limit: 200})
		if err != nil {
			return true
		}
		for _, o := range runs {
			if o.ID == r.ID {
				continue
			}
			steps, err := e.d.Store.RunSteps(ctx, o.ID)
			if err != nil {
				return true
			}
			for _, s := range steps {
				if s.Step == StepFix && s.State != store.StepPending {
					return true // its job runs or its commits wait for verify / push
				}
			}
		}
	}
	return false
}

// directResult is the part of a direct fix job's result autopilot reads (runner.Result).
type directResult struct {
	Mode  string `json:"mode"`
	Agent *struct {
		Summary string `json:"summary"`
	} `json:"agent"`
	Local *struct {
		Branch   string `json:"branch"`
		StartSHA string `json:"startSha"`
		HeadSHA  string `json:"headSha"`
		Commits  []struct {
			SHA string `json:"sha"`
		} `json:"commits"`
		FixesRef bool   `json:"fixesRef"`
		Outcome  string `json:"outcome"`
		Pushed   bool   `json:"pushed"`
	} `json:"local"`
}

// fixJobStep: queue the autopilot fix job, then wait for it and judge it by
// its git facts.
func (e *Engine) fixJobStep(ctx context.Context, r store.Run, m *FixManifest, st store.Step) (bool, error) {
	if st.State == store.StepPending {
		code, err := e.d.Store.ItemCodeProject(ctx, m.ItemID)
		if err != nil {
			return false, err
		}
		if !code.Open {
			_, _ = e.transition(ctx, st, store.StepPending, store.StepSkipped, store.StepUpdate{Error: new("the item was closed")})
			e.endFix(ctx, r, m, store.RunCancelled, "the item was closed before the fix")
			return false, nil
		}
		e.queueMu.Lock()
		if e.d.Fixer == nil || e.otherFixInFlight(ctx, r) {
			e.queueMu.Unlock()
			return false, nil // waits for the folder's previous fix to be pushed
		}
		key := fmt.Sprintf("item:%d:attempt:%d", m.ItemID, st.Attempt+1)
		sending, err := e.transition(ctx, st, store.StepPending, store.StepSending, store.StepUpdate{IncAttempt: true, IdemKey: &key})
		e.queueMu.Unlock()
		if err != nil {
			return false, err
		}
		entry, job, err := e.d.Fixer.AutopilotFix(ctx, m.ItemID, m.Event, m.Triage != nil)
		switch {
		case err != nil:
			_, terr := e.transition(ctx, sending, store.StepSending, store.StepPending, store.StepUpdate{Error: new(err.Error())})
			return false, terr
		case job != nil:
			ref := strconv.FormatInt(job.ID, 10)
			_, err := e.transition(ctx, sending, store.StepSending, store.StepSending, store.StepUpdate{ExternalRef: &ref})
			return false, err
		}
		switch entry.Reason {
		case store.ReasonExists: // an unfinished fix job of the item: adopt it when it is ours
			if j, err := e.d.Store.AutopilotJob(ctx, m.ItemID, ""); err == nil && slices.Contains(store.ActiveJobStates, j.State) {
				ref := strconv.FormatInt(j.ID, 10)
				_, err := e.transition(ctx, sending, store.StepSending, store.StepSending, store.StepUpdate{ExternalRef: &ref})
				return false, err
			}
			_, err := e.transition(ctx, sending, store.StepSending, store.StepPending, store.StepUpdate{Error: new("another fix job of this item is open: waiting")})
			return false, err
		case store.ReasonTotalCap, store.ReasonDayCap, store.ReasonRuleCap: // over a cap: next window
			_, err := e.transition(ctx, sending, store.StepSending, store.StepPending, store.StepUpdate{Error: new("deferred: " + entry.Reason)})
			return false, err
		}
		if _, err := e.transition(ctx, sending, store.StepSending, store.StepFailed, store.StepUpdate{Error: new(entry.Reason)}); err != nil {
			return false, err
		}
		e.holdFix(ctx, r, m, "fix:"+entry.Reason, "")
		return false, nil
	}

	// sending: the job is queued or done.
	if st.ExternalRef == "" { // crashed between queueing and recording it
		if j, err := e.d.Store.AutopilotJob(ctx, m.ItemID, st.StartedAt); err == nil {
			ref := strconv.FormatInt(j.ID, 10)
			_, err := e.transition(ctx, st, store.StepSending, store.StepSending, store.StepUpdate{ExternalRef: &ref})
			return err == nil, err
		}
		_, err := e.transition(ctx, st, store.StepSending, store.StepPending, store.StepUpdate{Error: new("interrupted before the job was queued")})
		return err == nil, err
	}
	jobID, _ := strconv.ParseInt(st.ExternalRef, 10, 64)
	j, err := e.d.Store.Job(ctx, jobID)
	if err != nil {
		return false, err
	}
	switch j.State {
	case store.JobQueued, store.JobRunning:
		return false, nil
	case store.JobCancelled:
		_, _ = e.transition(ctx, st, store.StepSending, store.StepFailed, store.StepUpdate{Error: new("the fix job was cancelled")})
		e.endFix(ctx, r, m, store.RunCancelled, "the fix job was cancelled")
		return false, nil
	case store.JobFailed: // retry: the automation caps (maxAttempts) end it
		_, err := e.transition(ctx, st, store.StepSending, store.StepPending, store.StepUpdate{Error: new("job " + st.ExternalRef + " failed: " + j.Error)})
		return err == nil, err
	}
	var res directResult
	_ = json.Unmarshal(j.Result, &res)
	if res.Local == nil {
		_, _ = e.transition(ctx, st, store.StepSending, store.StepFailed, store.StepUpdate{Error: new("not a direct fix (autopilot needs direct mode)")})
		e.holdFix(ctx, r, m, "fix:"+"not_direct", "")
		return false, nil
	}
	loc := res.Local
	hold := ""
	switch {
	case loc.Outcome == "closed":
		_, _ = e.transition(ctx, st, store.StepSending, store.StepSkipped, store.StepUpdate{Error: new("the item was closed")})
		e.endFix(ctx, r, m, store.RunCancelled, "the item was closed")
		return false, nil
	case len(loc.Commits) == 0 && loc.Outcome == HeldNotReproduced:
		hold = HeldNotReproduced
	case len(loc.Commits) == 0 && loc.Outcome == HeldNeedsInfo:
		hold = HeldNeedsInfo
	case len(loc.Commits) == 0:
		hold = HeldNoCommit
	case loc.FixesRef:
		hold = HeldClosingKeyword
	}
	if len(loc.Commits) == 0 && replystyle.NonBug(loc.Outcome) {
		// Not a bug (thanks, a question, an idea): no release; the reporter gets
		// a short reply (autoReply, else a draft for the owner) and the run ends answered.
		if _, err := e.transition(ctx, st, store.StepSending, store.StepSent, store.StepUpdate{ExternalRef: &st.ExternalRef, Error: new(loc.Outcome)}); err != nil {
			return false, err
		}
		if res.Agent != nil {
			m.Notes = clipRunes(strings.TrimSpace(res.Agent.Summary), 1000)
		}
		return true, e.takePath(ctx, r, m, OutcomeAnswered, loc.Outcome, loc.Outcome)
	}
	if hold == HeldNeedsInfo || hold == HeldNotReproduced {
		// The fixing agent needs the reporter: ask (autoReply) and wait held;
		// the reporter's next comment starts the item again.
		if _, err := e.transition(ctx, st, store.StepSending, store.StepSent, store.StepUpdate{ExternalRef: &st.ExternalRef, Error: new(hold)}); err != nil {
			return false, err
		}
		if res.Agent != nil {
			m.Notes = clipRunes(strings.TrimSpace(res.Agent.Summary), 1000)
		}
		return true, e.takePath(ctx, r, m, OutcomeNeedsInfo, DraftNeedsInfo, hold)
	}
	if hold != "" {
		if _, err := e.transition(ctx, st, store.StepSending, store.StepFailed, store.StepUpdate{Error: new(hold)}); err != nil {
			return false, err
		}
		e.holdFix(ctx, r, m, hold, j.Error)
		return false, nil
	}
	m.JobID, m.Branch, m.StartSHA, m.HeadSHA, m.Commits = j.ID, loc.Branch, loc.StartSHA, loc.HeadSHA, nil
	for _, c := range loc.Commits {
		m.Commits = append(m.Commits, c.SHA)
	}
	if err := e.setFixManifest(ctx, r, m); err != nil {
		return false, err
	}
	_, err = e.transition(ctx, st, store.StepSending, store.StepSent, store.StepUpdate{ExternalRef: &st.ExternalRef, Error: new("")})
	return err == nil, err
}

// verifyStep runs the gate on the fix head in the folder (under the folder
// lock), then the diff limits.
func (e *Engine) verifyStep(ctx context.Context, r store.Run, m *FixManifest, st store.Step) (bool, error) {
	if st.State == store.StepSending { // a verify of a previous process: run it again
		n, err := e.transition(ctx, st, store.StepSending, store.StepPending, store.StepUpdate{Error: new("interrupted (app restart)")})
		if err != nil {
			return false, err
		}
		st = n
	}
	unlock, err := e.lockFolder(m.Folder)
	if err != nil {
		return false, nil // a job uses the folder: next tick
	}
	defer unlock()
	sending, err := e.transition(ctx, st, store.StepPending, store.StepSending, store.StepUpdate{IncAttempt: true})
	if err != nil {
		return false, err
	}
	ref, hold, verr := e.verifyFix(ctx, r, m)
	if ctx.Err() != nil && verr != nil {
		return false, ctx.Err() // shutting down: the restart redoes it
	}
	ctx = context.WithoutCancel(ctx)
	if hold != "" {
		msg := hold
		if verr != nil {
			msg = verr.Error()
		}
		if _, err := e.transition(ctx, sending, store.StepSending, store.StepFailed, store.StepUpdate{Error: &msg, ExternalRef: nilIfEmpty(ref)}); err != nil {
			return false, err
		}
		e.holdFix(ctx, r, m, hold, msg)
		return false, nil
	}
	if _, err := e.transition(ctx, sending, store.StepSending, store.StepSent, store.StepUpdate{ExternalRef: &ref, Error: new("")}); err != nil {
		return false, err
	}
	if !e.d.Settings().Agents.AutopilotFor(m.Project).AutoPush {
		e.itemEvent(ctx, r, m.ItemID, "fix.verified", store.SeverityAttention,
			fmt.Sprintf("Исправление #%d проверено: ждёт push (авто-push выключен)", m.Number), map[string]any{"head": m.HeadSHA, "job": m.JobID})
	}
	return true, nil
}

// verifyFix: default branch, folder at the fix head and clean, gate passes,
// diff within the limits.
func (e *Engine) verifyFix(ctx context.Context, r store.Run, m *FixManifest) (ref, hold string, err error) {
	branch := ""
	if e.d.DefaultBranch != nil {
		branch, _ = e.d.DefaultBranch(ctx, m.Repo)
	}
	if branch == "" {
		if h, err := e.d.Git.Run(ctx, m.Folder, nil, "symbolic-ref", "--short", "refs/remotes/origin/HEAD"); err == nil {
			branch = strings.TrimPrefix(h, "origin/")
		}
	}
	if branch == "" {
		branch = "main"
	}
	if m.Branch != branch {
		return "", HeldNotDefault, fmt.Errorf("the fix is on %q, autopilot pushes only the default branch %s", m.Branch, branch)
	}
	rm := m.asRelease()
	if err := e.folderAt(ctx, rm, m.HeadSHA); err != nil {
		return "", HeldFolderMoved, err
	}
	if e.d.Gate == nil {
		return "", HeldNoVerify, errors.New("no verify gate available")
	}
	logDir := filepath.Join(e.d.DataDir, "autopilot", "fix", strconv.FormatInt(r.ID, 10), "logs")
	res, ran := e.d.Gate(ctx, m.Project, m.Folder, m.Folder, logDir, e.folderExpander(m.Project, m.Folder, m.Repo)) //nolint:contextcheck // source reads git tags with its own timeout
	if !ran {
		return "", HeldNoVerify, errors.New("no verify gate: the project has neither Aegis nor a verify command")
	}
	if !res.OK {
		return "", HeldGate, fmt.Errorf("%s: exit %d: %s", res.Command, res.ExitCode, tail(res.Output, 2000))
	}
	if err := e.folderAt(ctx, rm, m.HeadSHA); err != nil {
		return "", HeldFolderMoved, fmt.Errorf("after the gate: %w", err)
	}
	dc, err := e.diffCheck(ctx, m)
	if err != nil {
		return "", "failed:verify", err
	}
	res.Output = ""
	b, _ := json.Marshal(map[string]any{"gate": res, "diff": dc})
	limit := e.d.Settings().Agents.AutopilotFor(m.Project).MaxDiffLines
	switch {
	case limit > 0 && dc.Lines > limit:
		return string(b), HeldDiffTooBig, fmt.Errorf("%d changed lines, the limit is %d (autopilot.maxDiffLines)", dc.Lines, limit)
	case len(dc.Flagged) > 0 || len(dc.Deleted) > 0:
		return string(b), HeldDiffReview, fmt.Errorf("needs review: %s", strings.Join(append(dc.Flagged, prefixAll("deleted ", dc.Deleted)...), ", "))
	}
	return string(b), "", nil
}

func prefixAll(p string, s []string) []string {
	out := make([]string, len(s))
	for i, v := range s {
		out[i] = p + v
	}
	return out
}

// diffCheck measures StartSHA..HeadSHA: changed lines, deleted files and the
// paths that always need review.
func (e *Engine) diffCheck(ctx context.Context, m *FixManifest) (DiffCheck, error) {
	base := m.StartSHA
	if base == "" {
		base = emptyTreeSHA
	}
	num, err := e.d.Git.Run(ctx, m.Folder, nil, "diff", "--numstat", "--no-renames", base, m.HeadSHA)
	if err != nil {
		return DiffCheck{}, err
	}
	var dc DiffCheck
	sensitive := e.profilePaths(m.Project)
	for line := range strings.SplitSeq(num, "\n") {
		f := strings.SplitN(line, "\t", 3)
		if len(f) != 3 {
			continue
		}
		dc.Files++
		a, _ := strconv.Atoi(f[0]) // "-" (binary) counts 0
		d, _ := strconv.Atoi(f[1])
		dc.Lines += a + d
		if p := f[2]; needsReview(p, sensitive) {
			dc.Flagged = append(dc.Flagged, p)
		}
	}
	del, err := e.d.Git.Run(ctx, m.Folder, nil, "diff", "--name-only", "--no-renames", "--diff-filter=D", base, m.HeadSHA)
	if err != nil {
		return DiffCheck{}, err
	}
	for p := range strings.SplitSeq(del, "\n") {
		if p = strings.TrimSpace(p); p != "" {
			dc.Deleted = append(dc.Deleted, p)
		}
	}
	return dc, nil
}

const emptyTreeSHA = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"

// profilePaths are the repository files the publish profile reads (version,
// changelog, build output folder).
func (e *Engine) profilePaths(project string) []string {
	p := e.d.Settings().Agents.Projects[project].PublishProfile
	var out []string
	if f := versionFile(source.VersionSource(p.Version)); f != "" {
		out = append(out, f)
	}
	if f := changelogFile(source.ChangelogSource(p.Changelog)); f != "" {
		out = append(out, f)
	}
	return out
}

// reviewNames are file names (any folder) a fix may not change unattended:
// build / release scripts, dependency manifests, git attributes / modules.
var reviewNames = []string{
	"makefile", "justfile", "taskfile.yml", "taskfile.yaml", "jenkinsfile", "dockerfile", "lefthook.yml", "lefthook.yaml",
	".pre-commit-config.yaml", ".gitlab-ci.yml", ".travis.yml", "appveyor.yml", "azure-pipelines.yml", ".goreleaser.yml", ".goreleaser.yaml",
	"go.mod", "go.sum", "package.json", "package-lock.json", "pnpm-lock.yaml", "yarn.lock", "bun.lockb", "requirements.txt",
	"pyproject.toml", "pipfile", "pipfile.lock", "poetry.lock", "cargo.toml", "cargo.lock", "gemfile", "gemfile.lock",
	"composer.json", "composer.lock", "pom.xml", "build.gradle", "build.gradle.kts", "settings.gradle", "packages.config",
	"directory.build.props", "directory.packages.props", "nuget.config", "rockspec", ".gitattributes", ".gitmodules",
}

// reviewExts are script / project file extensions (build and release scripts).
var reviewExts = []string{".ps1", ".psm1", ".bat", ".cmd", ".sh", ".csproj", ".fsproj", ".vbproj", ".sln", ".rockspec"}

// reviewDirs are folders whose files are CI / release configuration.
var reviewDirs = []string{".github/", ".circleci/", ".gitlab/", ".husky/", ".vscode/", ".devcontainer/"}

func needsReview(p string, profile []string) bool {
	p = filepath.ToSlash(p)
	lp := strings.ToLower(p)
	base := path.Base(lp)
	switch {
	case slices.ContainsFunc(profile, func(f string) bool { return strings.EqualFold(path.Clean(filepath.ToSlash(f)), path.Clean(p)) }):
		return true
	case slices.Contains(reviewNames, base), slices.Contains(reviewExts, path.Ext(base)):
		return true
	case strings.HasPrefix(base, "build.") || strings.HasPrefix(base, "release.") || strings.HasPrefix(base, "publish."):
		return true
	}
	return slices.ContainsFunc(reviewDirs, func(d string) bool { return strings.HasPrefix(lp, d) || strings.Contains(lp, "/"+d) })
}

// fixPushStep pushes the fix head from the app mirror (autoPush), or waits
// for the owner's «Push» (autoPush off). Never while autopilot is paused.
func (e *Engine) fixPushStep(ctx context.Context, r store.Run, m *FixManifest, st store.Step) (bool, error) {
	cfg := e.d.Settings()
	ap := cfg.Agents.AutopilotFor(m.Project)
	mir := newMirror(e.d.Git, e.d.DataDir, m.Repo)
	rm := m.asRelease()
	if st.State == store.StepSending { // crashed mid push: did it happen?
		pr, perr := e.probeFixPush(ctx, mir, rm, m)
		switch {
		case perr != nil:
			_, err := e.transition(ctx, st, store.StepSending, store.StepUnknown, store.StepUpdate{Error: new("probe: " + perr.Error())})
			if err == nil {
				e.holdFix(ctx, r, m, "check:push", perr.Error())
			}
			return false, err
		case pr.present:
			_, err := e.transition(ctx, st, store.StepSending, store.StepSent, store.StepUpdate{ExternalRef: &pr.ref, Error: new("")})
			return err == nil, err
		}
		n, err := e.transition(ctx, st, store.StepSending, store.StepPending, store.StepUpdate{Error: new("not pushed (probe); will push")})
		if err != nil {
			return false, err
		}
		st = n
	}
	if cfg.Agents.Autopilot.Paused || !ap.Enabled {
		return false, nil
	}
	if !ap.AutoPush {
		j, err := e.d.Store.Job(ctx, m.JobID)
		if err != nil {
			return false, err
		}
		var res directResult
		_ = json.Unmarshal(j.Result, &res)
		if res.Local == nil || !res.Local.Pushed {
			return false, nil // waits for the owner's «Push»
		}
		_, err = e.transition(ctx, st, store.StepPending, store.StepSent, store.StepUpdate{ExternalRef: new("pushed by the owner"), Error: new("")})
		return err == nil, err
	}
	pr, perr := e.probeFixPush(ctx, mir, rm, m)
	if perr != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		_, err := e.transition(ctx, st, store.StepPending, store.StepUnknown, store.StepUpdate{Error: new("probe: " + perr.Error())})
		if err == nil {
			e.holdFix(ctx, r, m, "check:push", perr.Error())
		}
		return false, err
	}
	if pr.present {
		_, err := e.transition(ctx, st, store.StepPending, store.StepSent, store.StepUpdate{ExternalRef: &pr.ref, Error: new("")})
		return err == nil, err
	}
	if hold, cerr := e.checkFixPush(ctx, mir, rm, m); hold != "" {
		if _, err := e.transition(ctx, st, store.StepPending, store.StepFailed, store.StepUpdate{Error: new(cerr.Error())}); err != nil {
			return false, err
		}
		e.holdFix(ctx, r, m, hold, cerr.Error())
		return false, nil
	}
	key := m.Branch + ":" + m.HeadSHA
	sending, err := e.transition(ctx, st, store.StepPending, store.StepSending, store.StepUpdate{IncAttempt: true, IdemKey: &key})
	if err != nil {
		return false, err
	}
	if e.hook != nil {
		if err := e.hook("before", sending); err != nil {
			return false, errCrash
		}
	}
	serr := e.sendFixPush(ctx, mir, rm, m)
	if e.hook != nil {
		if err := e.hook("after", sending); err != nil {
			return false, errCrash
		}
	}
	if serr != nil && ctx.Err() != nil {
		return false, ctx.Err()
	}
	ctx = context.WithoutCancel(ctx)
	if serr == nil {
		_, err := e.transition(ctx, sending, store.StepSending, store.StepSent, store.StepUpdate{ExternalRef: &m.HeadSHA, Error: new("")})
		return err == nil, err
	}
	pr, perr = e.probeFixPush(ctx, mir, rm, m)
	switch {
	case perr != nil:
		_, err := e.transition(ctx, sending, store.StepSending, store.StepUnknown, store.StepUpdate{Error: new(serr.Error() + "; probe: " + perr.Error())})
		if err == nil {
			e.holdFix(ctx, r, m, "check:push", serr.Error())
		}
		return false, err
	case pr.present:
		_, err := e.transition(ctx, sending, store.StepSending, store.StepSent, store.StepUpdate{ExternalRef: &pr.ref, Error: new("")})
		return err == nil, err
	}
	if _, err := e.transition(ctx, sending, store.StepSending, store.StepFailed, store.StepUpdate{Error: new(serr.Error())}); err != nil {
		return false, err
	}
	e.holdFix(ctx, r, m, "failed:push", serr.Error())
	return false, nil
}

// probeFixPush: the fix head is on the remote default branch.
func (e *Engine) probeFixPush(ctx context.Context, mir *mirror, rm *Manifest, m *FixManifest) (probeResult, error) {
	if err := mir.ensure(ctx); err != nil {
		return probeResult{}, err
	}
	if _, err := mir.run(ctx, nil, "cat-file", "-e", m.HeadSHA+"^{commit}"); err != nil {
		if err := mir.fetchFolder(ctx, m.Folder, m.Branch, m.HeadSHA); err != nil {
			return probeResult{}, err
		}
	}
	url, env, secret, err := e.remote(ctx, rm)
	if err != nil {
		return probeResult{}, err
	}
	head, err := mir.fetchRemote(ctx, url, env, m.Branch)
	if err != nil {
		return probeResult{}, redact(err, secret)
	}
	on, err := mir.isAncestor(ctx, m.HeadSHA, head)
	if err != nil {
		return probeResult{}, err
	}
	return probeResult{present: on, ref: head}, nil
}

// checkFixPush: fast-forward only, and every commit ahead of the remote is
// one of the fix's own commits (never the owner's other unpushed work).
func (e *Engine) checkFixPush(ctx context.Context, mir *mirror, _ *Manifest, m *FixManifest) (string, error) {
	if err := mir.fetchFolder(ctx, m.Folder, m.Branch, m.HeadSHA); err != nil {
		return HeldFolderMoved, err
	}
	head, err := mir.run(ctx, nil, "rev-parse", "refs/remotes/origin/"+m.Branch)
	if err != nil {
		return "check:push", err
	}
	if ff, err := mir.isAncestor(ctx, head, m.HeadSHA); err != nil || !ff {
		return HeldRemoteMoved, fmt.Errorf("the remote %s moved to %s: not a fast-forward of the fix", m.Branch, short(head))
	}
	out, err := mir.run(ctx, nil, "rev-list", head+".."+m.HeadSHA)
	if err != nil {
		return "check:push", err
	}
	for sha := range strings.SplitSeq(out, "\n") {
		if sha = strings.TrimSpace(sha); sha != "" && !slices.Contains(m.Commits, sha) {
			return HeldForeignCommits, fmt.Errorf("commit %s ahead of the remote is not part of this fix", short(sha))
		}
	}
	return "", nil
}

func (e *Engine) sendFixPush(ctx context.Context, mir *mirror, rm *Manifest, m *FixManifest) error {
	url, env, secret, err := e.remote(ctx, rm)
	if err != nil {
		return err
	}
	if _, err := mir.run(ctx, env, "push", "-q", "--no-verify", url, m.HeadSHA+":refs/heads/"+m.Branch); err != nil {
		return redact(err, secret)
	}
	return nil
}

// resumeFix continues a held fix run: failed steps run again (a fix job gets
// a new attempt), unclear ones (a push or reply that may have happened) are
// probed by their step before any send. A run held for the reporter's details
// (all its steps through) is looked at again from triage / the fix.
func (e *Engine) resumeFix(ctx context.Context, run store.Run) (store.Run, error) {
	m, err := fixManifestOf(run)
	if err != nil {
		return run, err
	}
	steps, err := e.d.Store.RunSteps(ctx, run.ID)
	if err != nil {
		return run, err
	}
	again := m.Outcome == OutcomeNeedsInfo
	for _, st := range steps {
		to := ""
		switch {
		case st.State == store.StepFailed:
			to = store.StepPending
		case st.State == store.StepUnknown && st.Step == StepPush:
			to = store.StepSending // fixPushStep probes a sending push first
		case st.State == store.StepUnknown:
			to = store.StepPending // reply: attempt > 0 → probed before a send
		case again && (st.Step == StepTriage || st.Step == StepFix || st.Step == StepVerify || st.Step == StepPush) && st.State != store.StepPending:
			to = store.StepPending
		}
		if to == "" {
			continue
		}
		if _, err := e.transition(ctx, st, st.State, to, store.StepUpdate{Error: new("resumed by the owner")}); err != nil {
			return run, err
		}
	}
	if again {
		m.Outcome, m.OutcomeReason, m.Notes, m.Triage = "", "", "", nil
		b, _ := json.Marshal(m)
		if _, err := e.d.Store.UpdateRun(ctx, run.ID, []string{store.RunHeld}, store.RunUpdate{Manifest: b}); err != nil {
			return run, err
		}
	}
	r, err := e.d.Store.UpdateRun(ctx, run.ID, []string{store.RunHeld}, store.RunUpdate{State: store.RunRunning})
	if err != nil {
		if errors.Is(err, store.ErrRunState) {
			return r, ErrState
		}
		return r, err
	}
	e.d.OnChange(run.ID)
	e.launchFix(run.ID) //nolint:contextcheck // the run outlives the request: the engine's context
	return r, nil
}

// fixFinished ends a run whose steps are all through: pushed for a fix, else
// the triage path's outcome (needs_info stays held for the reporter).
func (e *Engine) fixFinished(ctx context.Context, r store.Run, m *FixManifest) error {
	switch m.Outcome {
	case "":
		return e.fixPushed(ctx, r, m)
	case OutcomeNeedsInfo:
		reason := HeldNeedsInfo
		if m.OutcomeReason == HeldNotReproduced {
			reason = HeldNotReproduced
		}
		e.holdFix(ctx, r, m, reason, cmpOrStr(m.Notes, "waiting for the reporter's details"))
	case OutcomeAnswered:
		e.endFix(ctx, r, m, store.RunAnswered, "answered")
	case OutcomeDuplicate:
		e.endFix(ctx, r, m, store.RunDuplicate, fmt.Sprintf("duplicate of item %d (%s)", m.Original, m.OutcomeReason))
	default:
		e.endFix(ctx, r, m, store.RunIgnored, "ignored: "+m.OutcomeReason)
	}
	return nil
}

// fixPushed: every step went through → pushed; the fix job ends done (its
// commits are on the remote) and the coalescing timer gets a look.
func (e *Engine) fixPushed(ctx context.Context, r store.Run, m *FixManifest) error {
	ctx = context.WithoutCancel(ctx)
	nr, err := e.d.Store.UpdateRun(ctx, r.ID, []string{store.RunRunning}, store.RunUpdate{State: store.RunPushed})
	if err != nil {
		return err
	}
	e.d.OnChange(r.ID)
	if m.JobID != 0 {
		if j, err := e.d.Store.Job(ctx, m.JobID); err == nil && j.State == store.JobNeedsReview {
			var res map[string]any
			if json.Unmarshal(j.Result, &res) == nil {
				if loc, ok := res["local"].(map[string]any); ok {
					loc["pushed"] = true
					if loc["outcome"] == "fixed_local" {
						loc["outcome"] = "pushed"
					}
				}
				b, _ := json.Marshal(res)
				_, _ = e.d.Store.UpdateJob(ctx, j.ID, []string{store.JobNeedsReview}, store.JobChange{State: new(store.JobDone), Finished: true, Result: b})
			}
		}
	}
	e.itemEvent(ctx, nr, m.ItemID, "fix.pushed", store.SeverityInfo, fmt.Sprintf("Исправление #%d отправлено в %s", m.Number, m.Branch),
		map[string]any{"head": m.HeadSHA, "commits": len(m.Commits)})
	e.Kick()
	return nil
}
