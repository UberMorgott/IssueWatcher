package release

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/store"
)

// Auto-triage (docs/AUTOPILOT.md → Fix run state machine, Phase 4): with
// autoTriage on, a fix run starts with a read-only classify agent; its verdict
// picks the path: bug → fix, duplicate → attached to the original's open run
// (or answered with the release that fixed it), question → reply, bug without
// enough information → reply asking for it (held needs_info), feature / spam /
// other → ignored. A regression of the latest release may trip the breaker.

// Triage kinds (Classification.Kind).
const (
	KindBug       = "bug"
	KindQuestion  = "question"
	KindFeature   = "feature"
	KindDuplicate = "duplicate"
	KindSpam      = "spam"
	KindOther     = "other"
	KindFeedback  = "feedback" // thanks / praise: nothing broken, nothing asked → a short reply
)

var triageKinds = []string{KindBug, KindQuestion, KindFeature, KindFeedback, KindDuplicate, KindSpam, KindOther}

// Fix run outcomes besides a fix (FixManifest.Outcome; "" = fix → push).
const (
	OutcomeAnswered  = "answered"   // question replied → store.RunAnswered
	OutcomeDuplicate = "duplicate"  // attached / answered with the original's release → store.RunDuplicate
	OutcomeNeedsInfo = "needs_info" // details asked (or the owner asked to) → held needs_info
	OutcomeIgnored   = "ignored"    // nothing to do → store.RunIgnored
)

// Held / ignore reasons of the triage step.
const (
	HeldTriageFailed = "triage_failed" // the classify agent failed maxTriageAttempts times
	IgnoredFiltered  = "filtered"      // a bug, but the project's own fix rules do not cover it
)

// maxTriageAttempts: a failing classify agent is retried this often, then the run is held.
const maxTriageAttempts = 3

// Classification is the classify agent's verdict (checked by the app).
type Classification struct {
	Kind        string `json:"kind"`                  // bug | question | feature | feedback | duplicate | spam | other
	DuplicateOf int64  `json:"duplicateOf,omitempty"` // item id of the original (one of the candidates)
	Severity    string `json:"severity,omitempty"`    // critical | high | medium | low
	Actionable  bool   `json:"actionable"`            // a bug with enough information to fix
	Regression  bool   `json:"regression"`            // broke in the latest released version
	Missing     string `json:"missing,omitempty"`     // not actionable: what the reporter should add
	Language    string `json:"language,omitempty"`    // the reporter's language (BCP 47, e.g. "ru", "en")
	Reason      string `json:"reason"`
}

// Candidate is an item the new report may duplicate (shown to the classify agent).
type Candidate struct {
	ItemID   int64  `json:"itemId"`
	Platform string `json:"platform"`
	Number   int    `json:"number"`
	Title    string `json:"title"`
	Body     string `json:"body,omitempty"`
	Open     bool   `json:"open"`
	Status   string `json:"status,omitempty"` // "fixing" | "fixed in vX.Y.Z" | ""
}

// ClassifyRequest is one classify run.
type ClassifyRequest struct {
	ItemID        int64
	Candidates    []Candidate
	LatestVersion string // the project's latest released version ("" = never released)
	LogPath       string // the agent's step log (JSONL)
}

// Triager classifies an incoming item with a read-only agent (runner.Runner).
type Triager interface {
	Classify(ctx context.Context, req ClassifyRequest) (Classification, error)
}

// fixStepsTriage are a new fix run's steps with auto-triage.
func fixStepsTriage() []store.NewStep {
	return append([]store.NewStep{{Step: StepTriage}}, fixSteps()...)
}

// triageOn: the project classifies its incoming items before acting.
func (e *Engine) triageOn(project string) bool {
	return e.d.Triager != nil && e.d.Settings().Agents.AutopilotFor(project).AutoTriage
}

// triageStep runs the classify agent once (read-only: an interrupted run is
// simply run again), stores the verdict and takes its path.
func (e *Engine) triageStep(ctx context.Context, r store.Run, m *FixManifest, st store.Step) (bool, error) {
	if st.State == store.StepSending { // interrupted (app restart): run it again
		n, err := e.transition(ctx, st, store.StepSending, store.StepPending, store.StepUpdate{Error: new("interrupted (app restart)")})
		if err != nil {
			return false, err
		}
		st = n
	}
	if e.d.Triager == nil {
		_, err := e.transition(ctx, st, store.StepPending, store.StepSkipped, store.StepUpdate{Error: new("auto-triage is not available")})
		return err == nil, err
	}
	cfg := e.d.Settings()
	if limit := cfg.Agents.AutomationFor(m.Project).TotalPerDay; limit > 0 {
		if n, err := e.d.Store.TriagesSince(ctx, e.d.Now().Add(-24*time.Hour)); err != nil || n >= limit {
			if err == nil && st.Error != "deferred: daily cap" {
				_, _ = e.transition(ctx, st, store.StepPending, store.StepPending, store.StepUpdate{Error: new("deferred: daily cap")})
			}
			return false, err // the next window
		}
	}
	req, err := e.classifyRequest(ctx, r, m)
	if err != nil {
		return false, err
	}
	sending, err := e.transition(ctx, st, store.StepPending, store.StepSending, store.StepUpdate{IncAttempt: true})
	if err != nil {
		return false, err
	}
	cls, cerr := e.d.Triager.Classify(ctx, req)
	if cerr != nil && ctx.Err() != nil {
		return false, ctx.Err() // shutting down: the restart runs it again
	}
	ctx = context.WithoutCancel(ctx)
	if cerr != nil {
		if sending.Attempt < maxTriageAttempts {
			_, err := e.transition(ctx, sending, store.StepSending, store.StepPending, store.StepUpdate{Error: new(cerr.Error())})
			return false, err // retried next tick
		}
		if _, err := e.transition(ctx, sending, store.StepSending, store.StepFailed, store.StepUpdate{Error: new(cerr.Error())}); err != nil {
			return false, err
		}
		e.holdFix(ctx, r, m, HeldTriageFailed, cerr.Error())
		return false, nil
	}
	cls = checkClassification(cls, req.Candidates)
	m.Triage = &cls
	if err := e.setFixManifest(ctx, r, m); err != nil {
		return false, err
	}
	b, _ := json.Marshal(cls)
	if _, err := e.transition(ctx, sending, store.StepSending, store.StepSent, store.StepUpdate{ExternalRef: new(string(b)), Error: new("")}); err != nil {
		return false, err
	}
	e.itemEvent(ctx, r, m.ItemID, "triage.classified", store.SeverityInfo,
		fmt.Sprintf("#%d разобран: %s", m.Number, triageLabel(cls)), map[string]any{"triage": cls, "url": m.ItemURL})
	return true, e.applyTriage(ctx, r, m)
}

func triageLabel(c Classification) string {
	s := c.Kind
	if c.Kind == KindBug && !c.Actionable {
		s += ", нужны подробности"
	}
	if c.Regression {
		s += ", регрессия"
	}
	if c.DuplicateOf != 0 {
		s += fmt.Sprintf(", дубликат %d", c.DuplicateOf)
	}
	return s
}

// checkClassification normalizes the agent's verdict: unknown kinds → other,
// a duplicateOf outside the candidates is dropped (then a duplicate without an
// original counts as a bug when actionable, else other).
func checkClassification(c Classification, cands []Candidate) Classification {
	c.Kind = strings.ToLower(strings.TrimSpace(c.Kind))
	if !slices.Contains(triageKinds, c.Kind) {
		c.Kind = KindOther
	}
	c.Severity = strings.ToLower(strings.TrimSpace(c.Severity))
	if !slices.Contains([]string{"critical", "high", "medium", "low"}, c.Severity) {
		c.Severity = ""
	}
	if c.DuplicateOf != 0 && !slices.ContainsFunc(cands, func(x Candidate) bool { return x.ItemID == c.DuplicateOf }) {
		c.DuplicateOf = 0
	}
	if c.Kind == KindDuplicate && c.DuplicateOf == 0 {
		c.Kind = KindOther
		if c.Actionable {
			c.Kind = KindBug
		}
	}
	if c.Kind != KindDuplicate && !c.Regression {
		c.DuplicateOf = 0
	}
	c.Language = strings.ToLower(strings.TrimSpace(c.Language))
	if len(c.Language) > 16 {
		c.Language = ""
	}
	c.Reason = clipRunes(strings.TrimSpace(c.Reason), 500)
	c.Missing = clipRunes(strings.TrimSpace(c.Missing), 500)
	return c
}

func clipRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

// classifyRequest gathers the candidates (the code project's items with a fix
// run, then its open ones) and the latest released version.
func (e *Engine) classifyRequest(ctx context.Context, r store.Run, m *FixManifest) (ClassifyRequest, error) {
	req := ClassifyRequest{ItemID: m.ItemID, LogPath: e.agentLog(r.ID, StepTriage)}
	list, err := e.d.Store.DuplicateCandidates(ctx, m.ProjectID, m.ItemID, 40, 300)
	if err != nil {
		return req, err
	}
	for _, c := range list {
		status := ""
		switch {
		case c.Version != "":
			status = "fixed in v" + c.Version
		case slices.Contains([]string{store.RunPending, store.RunRunning, store.RunHeld, store.RunPushed}, c.FixState):
			status = "fixing"
		}
		req.Candidates = append(req.Candidates, Candidate{ItemID: c.ItemID, Platform: c.Platform, Number: c.Number, Title: c.Title,
			Body: c.Body, Open: c.Open, Status: status})
	}
	if rel, err := e.d.Store.LatestRelease(ctx, r.ProjectID); err == nil {
		req.LatestVersion = rel.Version
	}
	return req, nil
}

// agentLog is where an autopilot agent step of run id writes its log.
func (e *Engine) agentLog(id int64, step string) string {
	return filepath.Join(e.d.DataDir, "autopilot", "fix", strconv.FormatInt(id, 10), step+".log")
}

// applyTriage takes the verdict's path: the fix steps stay for a bug; any
// other path skips them, adds a reply step when one is due and sets the
// outcome that ends the run once its steps are through.
func (e *Engine) applyTriage(ctx context.Context, r store.Run, m *FixManifest) error {
	c := m.Triage
	if c.Regression {
		e.regression(ctx, r, m)
	}
	switch {
	case c.Kind == KindDuplicate && !c.Regression:
		if done, err := e.duplicate(ctx, r, m); done || err != nil {
			return err
		}
		if !c.Actionable { // the original is neither fixing nor released, and this report alone is not enough
			return e.takePath(ctx, r, m, OutcomeNeedsInfo, DraftNeedsInfo, "")
		}
	case c.Kind == KindQuestion:
		return e.takePath(ctx, r, m, OutcomeAnswered, DraftQuestion, "")
	case c.Kind == KindFeedback:
		return e.takePath(ctx, r, m, OutcomeAnswered, DraftFeedback, KindFeedback)
	case c.Kind == KindBug && !c.Actionable:
		return e.takePath(ctx, r, m, OutcomeNeedsInfo, DraftNeedsInfo, "")
	case c.Kind != KindBug && c.Kind != KindDuplicate:
		return e.takePath(ctx, r, m, OutcomeIgnored, "", c.Kind)
	}
	// A bug: fixed unless the project's own fix rules filter it out.
	if e.d.Fixer != nil {
		match, scoped, err := e.d.Fixer.AutopilotMatch(ctx, m.ItemID, m.Event)
		if err != nil {
			return err
		}
		if scoped && !match {
			return e.takePath(ctx, r, m, OutcomeIgnored, "", IgnoredFiltered)
		}
	}
	return nil
}

// takePath sets outcome, skips the fix steps still pending and adds a reply
// step (draft kind) when autoReply is on and the item's platform replies;
// otherwise the reply waits for the owner (attention event with the draft).
func (e *Engine) takePath(ctx context.Context, r store.Run, m *FixManifest, outcome, draft, why string) error {
	m.Outcome, m.OutcomeReason = outcome, why
	if err := e.setFixManifest(ctx, r, m); err != nil {
		return err
	}
	steps, err := e.d.Store.RunSteps(ctx, r.ID)
	if err != nil {
		return err
	}
	for _, s := range steps {
		if (s.Step == StepFix || s.Step == StepVerify || s.Step == StepPush) && s.State == store.StepPending {
			if _, err := e.transition(ctx, s, store.StepPending, store.StepSkipped, store.StepUpdate{Error: new(outcome)}); err != nil {
				return err
			}
		}
	}
	if draft == "" {
		return nil
	}
	ap := e.d.Settings().Agents.AutopilotFor(m.Project)
	if ap.AutoReply && e.d.Items != nil && e.d.Items.CanReply(ctx, m.ItemID) {
		req, _ := json.Marshal(replyState{Kind: draft})
		_, err := e.d.Store.AddSteps(ctx, r.ID, "", "", []store.NewStep{{Step: StepReply, Target: strconv.FormatInt(m.ItemID, 10), Request: req}})
		return err
	}
	// No automatic reply: the draft goes to the owner.
	body, _ := e.draftFor(ctx, r, m, draft, "")
	e.itemEvent(ctx, r, m.ItemID, "reply.pending", store.SeverityAttention,
		fmt.Sprintf("#%d: ответ ждёт отправки (%s)", m.Number, outcome),
		map[string]any{"draft": body, "url": m.ItemURL, "reason": "autoReply is off or the platform cannot reply: reply from the item"})
	return nil
}

// duplicate attaches the item to the original's open fix run, or (the
// original's fix already released) answers it with that release. done =
// handled; false = the original has no usable run (fixed as a bug).
func (e *Engine) duplicate(ctx context.Context, r store.Run, m *FixManifest) (bool, error) {
	orig := m.Triage.DuplicateOf
	m.Original = orig
	if open, err := e.d.Store.OpenFixRun(ctx, orig); err == nil && open.ID != r.ID {
		if err := e.d.Store.AttachDuplicate(ctx, open.ID, m.ItemID); err == nil {
			m.OriginalRun = open.ID
			e.d.OnChange(open.ID)
			e.itemEvent(ctx, r, m.ItemID, "fix.duplicate", store.SeverityInfo,
				fmt.Sprintf("#%d — дубликат: ответ придёт с исправлением оригинала (запуск %d)", m.Number, open.ID),
				map[string]any{"original": orig, "run": open.ID})
			return true, e.takePath(ctx, r, m, OutcomeDuplicate, "", "attached")
		} else if !errors.Is(err, store.ErrRunState) {
			return false, err
		}
	} else if err != nil && !errors.Is(err, store.ErrNotFound) {
		return false, err
	}
	last, err := e.d.Store.LatestFixRun(ctx, orig)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if last.State != store.RunReleased || last.ReleaseID == 0 {
		return false, nil
	}
	rel, err := e.d.Store.Run(ctx, last.ReleaseID)
	if err != nil || rel.State != store.RunDone {
		return false, err
	}
	m.OriginalRun, m.ReleaseRun = last.ID, rel.ID
	return true, e.takePath(ctx, r, m, OutcomeDuplicate, DraftReleased, "released")
}

// regression evaluates the breaker after a report flagged as a regression of
// the latest release: ≥ 2 distinct reporters (or one GitHub collaborator)
// within regressionWindowHours of it → the project's autopilot is switched
// off (enabled = false) and an attention event says why; fewer → the report is
// only noted (attention event).
func (e *Engine) regression(ctx context.Context, r store.Run, m *FixManifest) {
	ctx = context.WithoutCancel(ctx)
	ap := e.d.Settings().Agents.AutopilotFor(m.Project)
	rel, err := e.d.Store.LatestRelease(ctx, r.ProjectID)
	if err != nil {
		e.itemEvent(ctx, r, m.ItemID, "regression.reported", store.SeverityAttention,
			fmt.Sprintf("#%d: сообщение о регрессии, но автопилот ещё не выпускал релизов", m.Number), map[string]any{"url": m.ItemURL})
		return
	}
	since := e.d.Now().Add(-time.Duration(ap.RegressionWindowHrs) * time.Hour)
	if at, err := time.Parse(time.RFC3339Nano, rel.UpdatedAt); err == nil && at.After(since) {
		since = at
	}
	reps, err := e.d.Store.RegressionReports(ctx, r.ProjectID, since)
	if err != nil {
		e.d.Log.Error("autopilot: regression reports", "run", r.ID, "err", err)
		return
	}
	authors := map[string]bool{}
	collaborator := false
	for _, rp := range reps {
		if a := strings.ToLower(strings.TrimSpace(rp.Author)); a != "" {
			authors[rp.Platform+":"+a] = true
		}
		if !collaborator && rp.Platform == store.CodePlatform && e.d.Collaborator != nil {
			if ok, err := e.d.Collaborator(ctx, rp.ItemID); err == nil && ok {
				collaborator = true
			}
		}
	}
	detail := map[string]any{"code": "v" + rel.Version, "version": rel.Version, "reporters": len(authors), "collaborator": collaborator,
		"windowHours": ap.RegressionWindowHrs, "url": m.ItemURL}
	if len(authors) < 2 && !collaborator {
		e.itemEvent(ctx, r, m.ItemID, "regression.reported", store.SeverityAttention,
			fmt.Sprintf("#%d: возможная регрессия в v%s (одно сообщение: автопилот продолжает)", m.Number, rel.Version), detail)
		return
	}
	if !ap.Enabled {
		return // already off
	}
	if e.d.PauseProject == nil {
		e.d.Log.Error("autopilot: regression breaker: no way to pause the project", "project", m.Project)
		return
	}
	if err := e.d.PauseProject(ctx, m.Project); err != nil {
		e.d.Log.Error("autopilot: regression breaker", "project", m.Project, "err", err)
		return
	}
	e.attentionOnce(ctx, store.AutopilotEvent{RunID: r.ID, ProjectID: r.ProjectID, ItemID: m.ItemID, Kind: "autopilot.regression_paused",
		Severity: store.SeverityAttention,
		Title:    fmt.Sprintf("%s: автопилот выключен — регрессия в v%s (%d сообщ.)", m.Project, rel.Version, len(reps))}, detail)
}

// reporterAnswered: a comment on an item whose run waits for the reporter
// (held needs_info / not_reproduced) ends that run, so the event starts a new
// one (triaged or matched again with the new details).
func (e *Engine) reporterAnswered(ctx context.Context, itemID int64) bool {
	open, err := e.d.Store.OpenFixRun(ctx, itemID)
	if err != nil || open.State != store.RunHeld || (open.HeldReason != HeldNeedsInfo && open.HeldReason != HeldNotReproduced) {
		return false
	}
	m, err := fixManifestOf(open)
	if err != nil {
		return false
	}
	e.endFix(ctx, open, m, store.RunCancelled, "the reporter answered: looked at again")
	return true
}
