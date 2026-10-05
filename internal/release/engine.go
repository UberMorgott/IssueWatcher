package release

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/config"
	"github.com/UberMorgott/issuewatcher/internal/provider"
	"github.com/UberMorgott/issuewatcher/internal/store"
)

// CmdResult is a command run in a job object (build, gate).
type CmdResult struct {
	Command    string `json:"command"`
	OK         bool   `json:"ok"`
	ExitCode   int    `json:"exitCode"`
	Output     string `json:"output,omitempty"` // tail
	DurationMS int64  `json:"durationMs"`
	TimedOut   bool   `json:"timedOut,omitempty"`
}

// FolderLocker is the mapped-folder lock shared with fix jobs (runner.AcquireFolder).
type FolderLocker interface {
	AcquireFolder(path string) (release func(), err error)
}

// Deps are the engine's collaborators (all injectable for tests).
type Deps struct {
	Store    *store.Store
	Settings func() config.Settings
	DataDir  string
	Git      Git // default ExecGit{}
	// Releaser creates GitHub releases and uploads assets; nil = githubRelease steps fail.
	Releaser provider.Releaser
	// Publishers returns the platform's publisher, nil when it cannot publish.
	Publishers func(platform string) provider.Publisher
	// DefaultBranch is the code repository's default branch (owner/repo).
	DefaultBranch func(ctx context.Context, repo string) (string, error)
	// GitToken is the token pushes use (GitHub user token).
	GitToken func(ctx context.Context) (string, error)
	// TokenEnv turns url + token into git environment entries (runner.TokenEnv).
	TokenEnv func(url, token string) (env []string, secret string)
	// GitURL maps the project web URL to the URL pushed to (default URL + ".git").
	GitURL  func(projectURL string) string
	Folders FolderLocker
	// Gate runs the project's verify gate in dir; ran = false when it has none.
	Gate func(ctx context.Context, project, localPath, dir, logDir string) (res CmdResult, ran bool)
	// Command runs a shell command in dir inside a job object (the build).
	Command func(ctx context.Context, dir, command, logDir string, timeout time.Duration) CmdResult
	Now     func() time.Time
	Log     *slog.Logger
	// OnChange runs after every change of a run or one of its steps (SSE autopilot.run).
	OnChange func(runID int64)
	// OnEvent runs after an activity log event was written.
	OnEvent func(store.AutopilotEvent)
	// PollEvery paces availability checks (default 1 min); AvailableFor is the
	// longest wait (default 72 h, then held).
	PollEvery    time.Duration
	AvailableFor time.Duration
}

// Engine runs release runs. One executor goroutine per running run.
type Engine struct {
	d Deps

	mu     sync.Mutex
	active map[int64]*active
	ctx    context.Context // set by Start; executors outlive requests
	wg     sync.WaitGroup

	// hook is called around every external send (tests simulate crashes):
	// point "before" (after sending is committed, before the call) and "after"
	// (after the call, before its result is committed). An error aborts the
	// executor leaving the step as it is.
	hook func(point string, st store.Step) error
}

type active struct {
	stop bool
	done chan struct{}
}

// Errors of the run actions.
var (
	ErrNotFound = store.ErrNotFound
	// ErrState: the run (or step) state does not allow the action.
	ErrState = errors.New("release: not allowed in this state")
	// ErrBadStep: skip of a step that is not publish/available of a target.
	ErrBadStep = errors.New("release: only publish:<target> / available:<target> can be skipped")
	// errCrash aborts an executor (tests).
	errCrash = errors.New("release: executor stopped")
)

// New builds an engine; Start reconciles and resumes runs.
func New(d Deps) *Engine {
	if d.Git == nil {
		d.Git = ExecGit{}
	}
	if d.GitURL == nil {
		d.GitURL = func(u string) string { return u + ".git" }
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Log == nil {
		d.Log = slog.New(slog.DiscardHandler)
	}
	if d.OnChange == nil {
		d.OnChange = func(int64) {}
	}
	if d.OnEvent == nil {
		d.OnEvent = func(store.AutopilotEvent) {}
	}
	if d.PollEvery <= 0 {
		d.PollEvery = time.Minute
	}
	if d.AvailableFor <= 0 {
		d.AvailableFor = 72 * time.Hour
	}
	if d.TokenEnv == nil {
		d.TokenEnv = func(string, string) ([]string, string) { return nil, "" }
	}
	return &Engine{d: d, active: map[int64]*active{}, ctx: context.Background()}
}

// Start reconciles unfinished runs left by a previous process (sending →
// unknown → probe) and continues the running ones in the background until ctx ends.
func (e *Engine) Start(ctx context.Context) error {
	e.mu.Lock()
	e.ctx = ctx
	e.mu.Unlock()
	ids, err := e.Reconcile(ctx)
	if err != nil {
		return err
	}
	for _, id := range ids {
		e.launch(id) //nolint:contextcheck // executors run under the engine's context (set above)
	}
	return nil
}

// Wait blocks until every executor has ended (after Start's ctx is done).
func (e *Engine) Wait() { e.wg.Wait() }

// launch runs Execute for id in the background unless it already runs.
func (e *Engine) launch(id int64) {
	e.mu.Lock()
	ctx := e.ctx
	e.mu.Unlock()
	e.wg.Go(func() {
		if err := e.Execute(ctx, id); err != nil && !errors.Is(err, errCrash) && !errors.Is(err, ErrState) {
			e.d.Log.Error("release: run", "run", id, "err", err)
		}
	})
}

// RunView is a run with its steps and items (GET /api/runs/{id}, SSE autopilot.run).
type RunView struct {
	Run   store.Run       `json:"run"`
	Steps []store.Step    `json:"steps"`
	Items []store.RunItem `json:"items"`
}

// View reads run id with its steps and items.
func (e *Engine) View(ctx context.Context, id int64) (RunView, error) {
	r, err := e.d.Store.Run(ctx, id)
	if err != nil {
		return RunView{}, err
	}
	steps, err := e.d.Store.RunSteps(ctx, id)
	if err != nil {
		return RunView{}, err
	}
	items, err := e.d.Store.RunItems(ctx, id)
	if err != nil {
		return RunView{}, err
	}
	return RunView{Run: r, Steps: steps, Items: items}, nil
}

// --- step bookkeeping ---------------------------------------------------------

func (e *Engine) transition(ctx context.Context, st store.Step, from, to string, u store.StepUpdate) (store.Step, error) {
	n, err := e.d.Store.TransitionStep(ctx, st.RunID, st.Step, st.Target, from, to, u)
	if err == nil {
		e.d.OnChange(st.RunID)
	}
	return n, err
}

func stepName(st store.Step) string {
	if st.Target == "" {
		return st.Step
	}
	return st.Step + ":" + st.Target
}

// targetStep: publish / available steps fail independently of each other.
func targetStep(st store.Step) bool { return st.Step == StepPublish || st.Step == StepAvailable }

func external(step string) bool {
	switch step {
	case StepPush, StepTag, StepGHRelease, StepGHAsset, StepPublish:
		return true
	}
	return false
}

// holdRun moves run id to held with reason and records an attention event.
func (e *Engine) holdRun(ctx context.Context, run store.Run, reason, detail string) {
	ctx = context.WithoutCancel(ctx)
	r, err := e.d.Store.UpdateRun(ctx, run.ID, []string{store.RunPending, store.RunRunning, store.RunHeld},
		store.RunUpdate{State: store.RunHeld, HeldReason: new(reason)})
	if err != nil {
		e.d.Log.Error("release: hold run", "run", run.ID, "err", err)
		return
	}
	e.d.OnChange(run.ID)
	e.event(ctx, r, "release.held", store.SeverityAttention,
		fmt.Sprintf("Релиз v%s остановлен: %s", r.Version, reason), map[string]any{"reason": reason, "detail": detail})
}

func (e *Engine) event(ctx context.Context, r store.Run, kind, severity, title string, detail map[string]any) {
	b, _ := json.Marshal(detail)
	ev, err := e.d.Store.AddEvent(context.WithoutCancel(ctx), store.AutopilotEvent{RunID: r.ID, ProjectID: r.ProjectID,
		Kind: kind, Severity: severity, Title: title, Detail: b})
	if err != nil {
		e.d.Log.Error("release: event", "run", r.ID, "err", err)
		return
	}
	e.d.OnEvent(ev)
}

func manifestOf(r store.Run) (*Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(r.Manifest, &m); err != nil {
		return nil, fmt.Errorf("release: run %d manifest: %w", r.ID, err)
	}
	return &m, nil
}

// --- executor ----------------------------------------------------------------

// rc is one execution's context.
type rc struct {
	run   store.Run
	m     *Manifest
	art   *Artifact
	mir   *mirror
	files map[string]string // target key → platform file name (probe → send)
	hold  string            // first hold reason of a target step (publish / available)
}

func (e *Engine) begin(id int64) (*active, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, ok := e.active[id]; ok {
		return nil, false
	}
	a := &active{done: make(chan struct{})}
	e.active[id] = a
	return a, true
}

func (e *Engine) end(id int64, a *active) {
	e.mu.Lock()
	delete(e.active, id)
	e.mu.Unlock()
	close(a.done)
}

func (e *Engine) stopping(id int64) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	a := e.active[id]
	return a != nil && a.stop
}

// stopExecutor asks a running executor of id to stop after its current step and waits for it.
func (e *Engine) stopExecutor(id int64) {
	e.mu.Lock()
	a := e.active[id]
	if a != nil {
		a.stop = true
	}
	e.mu.Unlock()
	if a != nil {
		<-a.done
	}
}

// Execute runs release run id from its first unfinished step until done or
// held (synchronously; Start / Resume call it in the background).
func (e *Engine) Execute(ctx context.Context, id int64) error {
	a, ok := e.begin(id)
	if !ok {
		return ErrState
	}
	defer e.end(id, a)
	run, err := e.d.Store.Run(ctx, id)
	if err != nil {
		return err
	}
	if run.State == store.RunPending {
		if run, err = e.d.Store.UpdateRun(ctx, id, []string{store.RunPending}, store.RunUpdate{State: store.RunRunning}); err != nil {
			return err
		}
		e.d.OnChange(id)
	}
	if run.State != store.RunRunning {
		return ErrState
	}
	m, err := manifestOf(run)
	if err != nil {
		_, _ = e.d.Store.UpdateRun(ctx, id, []string{store.RunRunning}, store.RunUpdate{State: store.RunFailed})
		return err
	}
	c := &rc{run: run, m: m, mir: newMirror(e.d.Git, e.d.DataDir, m.Repo), files: map[string]string{}}
	var problems []string
	for {
		if e.stopping(id) {
			return nil
		}
		steps, err := e.d.Store.RunSteps(ctx, id)
		if err != nil {
			return err
		}
		next, blocked := pickStep(steps)
		problems = blocked
		if next == nil {
			break
		}
		// refresh the run (bump fills the manifest)
		if run, err = e.d.Store.Run(ctx, id); err != nil {
			return err
		}
		if run.State != store.RunRunning {
			return nil // cancelled / held meanwhile
		}
		c.run = run
		if c.m, err = manifestOf(run); err != nil {
			return err
		}
		c.art = artifactOf(steps)
		hold, err := e.step(ctx, c, *next)
		if err != nil {
			return err
		}
		if hold != "" && !targetStep(*next) {
			e.holdRun(ctx, c.run, hold, next.Error)
			return nil
		}
		if hold != "" && c.hold == "" {
			c.hold = hold
		}
	}
	if len(problems) > 0 {
		e.holdRun(ctx, c.run, cmp.Or(c.hold, problems[0]), "")
		return nil
	}
	r, err := e.d.Store.UpdateRun(context.WithoutCancel(ctx), id, []string{store.RunRunning}, store.RunUpdate{State: store.RunDone})
	if err != nil {
		return err
	}
	e.d.OnChange(id)
	var targets []string
	for _, t := range m.Targets {
		targets = append(targets, t.Key)
	}
	e.event(ctx, r, "release.done", store.SeverityInfo, "Релиз v"+r.Version+" выпущен",
		map[string]any{"version": r.Version, "tag": m.Tag, "targets": targets})
	return nil
}

// pickStep returns the next pending step to run, and the hold reasons of
// failed / unknown target steps seen so far (they do not stop other targets).
func pickStep(steps []store.Step) (*store.Step, []string) {
	var problems []string
	published := map[string]bool{}
	for i := range steps {
		st := steps[i]
		switch st.State {
		case store.StepSent, store.StepSkipped:
			if st.Step == StepPublish && st.State == store.StepSent {
				published[st.Target] = true
			}
			continue
		case store.StepFailed, store.StepUnknown, store.StepSending:
			reason := "failed:" + stepName(st)
			if st.State != store.StepFailed {
				reason = "check:" + stepName(st)
			}
			if !targetStep(st) {
				return nil, append(problems, reason)
			}
			problems = append(problems, reason)
			continue
		}
		if st.Step == StepAvailable && !published[st.Target] {
			continue // its publish did not go through
		}
		return &steps[i], problems
	}
	return nil, problems
}

func artifactOf(steps []store.Step) *Artifact {
	for _, st := range steps {
		if st.Step == StepBuild && st.State == store.StepSent && st.ExternalRef != "" {
			var a Artifact
			if json.Unmarshal([]byte(st.ExternalRef), &a) == nil {
				return &a
			}
		}
	}
	return nil
}

// probeResult is a read-only check of whether an external action happened.
type probeResult struct {
	present bool
	ref     string
	hold    string // definite conflict: the step fails with this held reason
}

// step runs one pending step; hold != "" means the step ended failed / unknown.
func (e *Engine) step(ctx context.Context, c *rc, st store.Step) (hold string, err error) {
	if !external(st.Step) {
		return e.localStep(ctx, c, st)
	}
	x := e.ext(st.Step)
	pr, perr := x.probe(ctx, c, st)
	if perr != nil && ctx.Err() != nil {
		return "", errCrash
	}
	switch {
	case perr != nil:
		_, err := e.transition(ctx, st, store.StepPending, store.StepUnknown, store.StepUpdate{Error: new("probe: " + perr.Error())})
		return "check:" + stepName(st), err
	case pr.hold != "":
		_, err := e.transition(ctx, st, store.StepPending, store.StepFailed, store.StepUpdate{Error: new(pr.hold)})
		return pr.hold, err
	case pr.present:
		_, err := e.transition(ctx, st, store.StepPending, store.StepSent, store.StepUpdate{ExternalRef: new(pr.ref), Error: new("")})
		return "", err
	}
	if x.check != nil {
		if hold, cerr := x.check(ctx, c, st); hold != "" {
			msg := hold
			if cerr != nil {
				msg = cerr.Error()
			}
			_, err := e.transition(ctx, st, store.StepPending, store.StepFailed, store.StepUpdate{Error: new(msg)})
			return hold, err
		}
	}
	key := x.idem(c, st)
	sending, err := e.transition(ctx, st, store.StepPending, store.StepSending, store.StepUpdate{IncAttempt: true, IdemKey: &key})
	if err != nil {
		if errors.Is(err, store.ErrStepState) {
			return "", errCrash // another executor owns it
		}
		return "", err
	}
	if e.hook != nil {
		if err := e.hook("before", sending); err != nil {
			return "", errCrash
		}
	}
	ref, serr := x.send(ctx, c, sending)
	if e.hook != nil {
		if err := e.hook("after", sending); err != nil {
			return "", errCrash
		}
	}
	if serr != nil && ctx.Err() != nil { // shutting down: the restart reconciles it (probe)
		return "", errCrash
	}
	ctx = context.WithoutCancel(ctx)
	if serr == nil {
		_, err := e.transition(ctx, sending, store.StepSending, store.StepSent, store.StepUpdate{ExternalRef: &ref, Error: new("")})
		return "", err
	}
	var hf holdError
	if errors.As(serr, &hf) && hf.definite { // refused before anything left the app
		_, err := e.transition(ctx, sending, store.StepSending, store.StepFailed, store.StepUpdate{Error: new(serr.Error())})
		return hf.reason, err
	}
	// The send failed: did it happen anyway?
	pr, perr = x.probe(ctx, c, sending)
	switch {
	case perr != nil:
		_, err := e.transition(ctx, sending, store.StepSending, store.StepUnknown,
			store.StepUpdate{Error: new(serr.Error() + "; probe: " + perr.Error())})
		return "check:" + stepName(st), err
	case pr.present:
		_, err := e.transition(ctx, sending, store.StepSending, store.StepSent, store.StepUpdate{ExternalRef: new(pr.ref), Error: new("")})
		return "", err
	}
	reason := "failed:" + stepName(st)
	if errors.As(serr, &hf) {
		reason = hf.reason
	} else if pr.hold != "" {
		reason = pr.hold
	}
	_, err = e.transition(ctx, sending, store.StepSending, store.StepFailed, store.StepUpdate{Error: new(serr.Error())})
	return reason, err
}

// holdError is a send / check failure with a specific held reason; definite
// means nothing was sent (safe to retry after the owner fixes it).
type holdError struct {
	reason   string
	err      error
	definite bool
}

func (h holdError) Error() string {
	if h.err != nil {
		return h.reason + ": " + h.err.Error()
	}
	return h.reason
}

func (h holdError) Unwrap() error { return h.err }

// localStep runs bump / build / archive_check / gate / smoke / available.
func (e *Engine) localStep(ctx context.Context, c *rc, st store.Step) (string, error) {
	sending, err := e.transition(ctx, st, store.StepPending, store.StepSending, store.StepUpdate{IncAttempt: true})
	if err != nil {
		if errors.Is(err, store.ErrStepState) {
			return "", errCrash
		}
		return "", err
	}
	res := e.runLocal(ctx, c, sending)
	if ctx.Err() != nil && res.err != nil { // shutting down: the restart redoes it
		return "", errCrash
	}
	ctx = context.WithoutCancel(ctx)
	switch {
	case res.skip != "":
		_, err = e.transition(ctx, sending, store.StepSending, store.StepSkipped, store.StepUpdate{Error: new(res.skip)})
		return "", err
	case res.hold != "":
		msg := res.hold
		if res.err != nil {
			msg = res.err.Error()
		}
		to := store.StepFailed
		if strings.HasPrefix(res.hold, "check:") {
			to = store.StepUnknown
		}
		_, err = e.transition(ctx, sending, store.StepSending, to, store.StepUpdate{Error: new(msg), ExternalRef: nilIfEmpty(res.ref)})
		return res.hold, err
	case res.err != nil:
		_, err = e.transition(ctx, sending, store.StepSending, store.StepFailed, store.StepUpdate{Error: new(res.err.Error())})
		return "failed:" + stepName(st), err
	}
	_, err = e.transition(ctx, sending, store.StepSending, store.StepSent, store.StepUpdate{ExternalRef: &res.ref, Error: new(res.note)})
	return "", err
}

func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

type localResult struct {
	ref  string
	note string // kept in error for a passed step (informational)
	skip string // skipped with this note
	hold string // failed with this held reason
	err  error
}

func (e *Engine) runLocal(ctx context.Context, c *rc, st store.Step) localResult {
	switch st.Step {
	case StepBump:
		return e.bump(ctx, c)
	case StepBuild:
		return e.build(ctx, c)
	case StepArchiveCheck:
		return e.archiveCheck(ctx, c)
	case StepGate:
		return e.gate(ctx, c)
	case StepSmoke:
		return localResult{skip: "no smoke adapter yet (Phase 2)"}
	case StepAvailable:
		return e.available(ctx, c, st)
	}
	return localResult{err: fmt.Errorf("unknown step %s", st.Step)}
}

// --- run actions -------------------------------------------------------------

// Resume continues a held run: failed steps go back to pending, unknown ones
// are probed (present → sent, absent → pending, still unclear → held again).
func (e *Engine) Resume(ctx context.Context, id int64) (store.Run, error) {
	run, err := e.d.Store.Run(ctx, id)
	if err != nil {
		return run, err
	}
	if run.State != store.RunHeld {
		return run, ErrState
	}
	m, err := manifestOf(run)
	if err != nil {
		return run, err
	}
	steps, err := e.d.Store.RunSteps(ctx, id)
	if err != nil {
		return run, err
	}
	c := &rc{run: run, m: m, mir: newMirror(e.d.Git, e.d.DataDir, m.Repo), art: artifactOf(steps), files: map[string]string{}}
	for _, st := range steps {
		switch st.State {
		case store.StepFailed:
			if _, err := e.transition(ctx, st, store.StepFailed, store.StepPending, store.StepUpdate{}); err != nil {
				return run, err
			}
		case store.StepUnknown, store.StepSending:
			if hold, err := e.resolve(ctx, c, st); err != nil {
				return run, err
			} else if hold != "" {
				e.holdRun(ctx, run, hold, "")
				r, _ := e.d.Store.Run(ctx, id)
				return r, nil
			}
		}
	}
	r, err := e.d.Store.UpdateRun(ctx, id, []string{store.RunHeld}, store.RunUpdate{State: store.RunRunning})
	if err != nil {
		if errors.Is(err, store.ErrRunState) {
			return r, ErrState
		}
		return r, err
	}
	e.d.OnChange(id)
	e.launch(id) //nolint:contextcheck // the run outlives the request: the engine's context
	return r, nil
}

// resolve settles a step left sending / unknown by a crash or an unclear
// answer: external → probe; local → back to pending (bump: reconciled with
// the folder). hold != "" when it stays unclear.
func (e *Engine) resolve(ctx context.Context, c *rc, st store.Step) (string, error) {
	if st.State == store.StepSending {
		to := store.StepUnknown
		if !external(st.Step) && st.Step != StepBump {
			to = store.StepPending
		}
		n, err := e.transition(ctx, st, store.StepSending, to, store.StepUpdate{Error: new("interrupted (app restart)")})
		if err != nil {
			return "", err
		}
		st = n
		if to == store.StepPending {
			return "", nil
		}
	}
	if st.Step == StepBump {
		return e.reconcileBump(ctx, c, st)
	}
	if !external(st.Step) {
		_, err := e.transition(ctx, st, store.StepUnknown, store.StepPending, store.StepUpdate{})
		return "", err
	}
	pr, perr := e.ext(st.Step).probe(ctx, c, st)
	switch {
	case perr != nil:
		_, err := e.transition(ctx, st, store.StepUnknown, store.StepUnknown, store.StepUpdate{Error: new("probe: " + perr.Error())})
		return "check:" + stepName(st), err
	case pr.hold != "":
		_, err := e.transition(ctx, st, store.StepUnknown, store.StepFailed, store.StepUpdate{Error: new(pr.hold)})
		return pr.hold, err
	case pr.present:
		_, err := e.transition(ctx, st, store.StepUnknown, store.StepSent, store.StepUpdate{ExternalRef: new(pr.ref), Error: new("")})
		return "", err
	}
	// Proven absent: safe to send again.
	_, err := e.transition(ctx, st, store.StepUnknown, store.StepPending, store.StepUpdate{Error: new("not done (probe); will send")})
	return "", err
}

// Reconcile settles every unfinished release run after a restart: steps left
// sending are probed (see resolve); a run that stays unclear is held. It
// returns the runs to continue.
func (e *Engine) Reconcile(ctx context.Context) ([]int64, error) {
	runs, err := e.d.Store.UnfinishedRuns(ctx)
	if err != nil {
		return nil, err
	}
	var cont []int64
	for _, run := range runs {
		if run.Kind != store.RunKindRelease {
			continue
		}
		m, err := manifestOf(run)
		if err != nil {
			e.d.Log.Error("release: reconcile", "run", run.ID, "err", err)
			continue
		}
		steps, err := e.d.Store.RunSteps(ctx, run.ID)
		if err != nil {
			return nil, err
		}
		c := &rc{run: run, m: m, mir: newMirror(e.d.Git, e.d.DataDir, m.Repo), art: artifactOf(steps), files: map[string]string{}}
		hold := ""
		for _, st := range steps {
			settle := st.State == store.StepSending || (st.State == store.StepUnknown && run.State != store.RunHeld)
			if !settle {
				continue
			}
			h, err := e.resolve(ctx, c, st)
			if err != nil {
				return nil, err
			}
			if h != "" && hold == "" {
				hold = h
			}
		}
		switch {
		case run.State == store.RunHeld:
		case hold != "":
			e.holdRun(ctx, run, hold, "unclear after a restart")
		default:
			cont = append(cont, run.ID)
		}
	}
	return cont, nil
}

// Skip marks a target's publish (and its availability) or only its
// availability step skipped; the run must be held. Resume continues it.
func (e *Engine) Skip(ctx context.Context, id int64, step, target string) (store.Run, error) {
	if (step != StepPublish && step != StepAvailable) || target == "" {
		return store.Run{}, ErrBadStep
	}
	run, err := e.d.Store.Run(ctx, id)
	if err != nil {
		return run, err
	}
	if run.State != store.RunHeld && run.State != store.RunPending {
		return run, ErrState
	}
	steps, err := e.d.Store.RunSteps(ctx, id)
	if err != nil {
		return run, err
	}
	names := []string{step}
	if step == StepPublish {
		names = append(names, StepAvailable)
	}
	found := false
	for _, st := range steps {
		if st.Target != target || !slices.Contains(names, st.Step) {
			continue
		}
		found = true
		switch st.State {
		case store.StepSkipped:
			continue
		case store.StepSent:
			if st.Step == step {
				return run, ErrState // already published: skip its availability instead
			}
			continue
		case store.StepSending:
			return run, ErrState
		}
		if _, err := e.transition(ctx, st, st.State, store.StepSkipped, store.StepUpdate{Error: new("skipped by the owner")}); err != nil {
			return run, err
		}
	}
	if !found {
		return run, ErrNotFound
	}
	return e.d.Store.Run(ctx, id)
}

// CancelResult says what a cancel did.
type CancelResult struct {
	Run         store.Run `json:"run"`
	BumpDropped bool      `json:"bumpDropped"`
	Note        string    `json:"note,omitempty"`
}

// Cancel stops run id. Before any public step it drops the app's own bump
// commit when HEAD is that commit, the tree is clean and the remote provably
// lacks it; after a public step it only stops the remaining steps.
func (e *Engine) Cancel(ctx context.Context, id int64) (CancelResult, error) {
	e.stopExecutor(id)
	run, err := e.d.Store.Run(ctx, id)
	if err != nil {
		return CancelResult{}, err
	}
	switch run.State {
	case store.RunPending, store.RunRunning, store.RunHeld:
	default:
		return CancelResult{Run: run}, ErrState
	}
	m, err := manifestOf(run)
	if err != nil {
		return CancelResult{}, err
	}
	steps, err := e.d.Store.RunSteps(ctx, id)
	if err != nil {
		return CancelResult{}, err
	}
	public := false
	for _, st := range steps {
		if external(st.Step) && st.State != store.StepPending && st.State != store.StepSkipped && st.State != store.StepFailed {
			public = true
		}
	}
	res := CancelResult{}
	switch {
	case public:
		res.Note = "public steps already happened: the remaining steps are stopped, nothing is undone"
	case m.BumpSHA == "" || m.BumpSHA == m.Head:
		res.Note = "no bump commit to drop"
	default:
		res.BumpDropped, res.Note = e.dropBump(ctx, m)
	}
	r, err := e.d.Store.UpdateRun(context.WithoutCancel(ctx), id, []string{store.RunPending, store.RunRunning, store.RunHeld},
		store.RunUpdate{State: store.RunCancelled})
	if err != nil {
		return CancelResult{}, err
	}
	e.d.OnChange(id)
	e.event(ctx, r, "release.cancelled", store.SeverityInfo, "Релиз v"+r.Version+" отменён",
		map[string]any{"bumpDropped": res.BumpDropped, "note": res.Note})
	res.Run = r
	return res, nil
}
