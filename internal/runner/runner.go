// Package runner dispatches issues to local AI agent CLIs (Claude Code,
// Codex): a SQLite-backed job queue with per-project and global concurrency,
// one git worktree per fix job, streamed logs, a verify step and publishing
// (branch push + draft PR, or a posted reply) that only ever happens on the
// user's click. docs/ARCHITECTURE.md → Runner.
package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/config"
	"github.com/UberMorgott/issuewatcher/internal/folders"
	"github.com/UberMorgott/issuewatcher/internal/provider"
	"github.com/UberMorgott/issuewatcher/internal/store"
)

// branchPrefix marks branches the runner owns (and may delete).
const branchPrefix = "iw/"

// Error codes in Result.ErrorCode (the UI links the fix for some).
const (
	CodeNoFolder     = "no_folder"      // project has no working local folder → Settings › Projects and folders
	CodeModItem      = "mod_item"       // push / PR of a mod-page fix while agents.modPush is off
	CodeNoProfile    = "no_profile"     // profile missing → Settings › Agents
	CodeNoCLI        = "no_cli"         // executable not found → Settings › Agents
	CodeTimeout      = "timeout"        // profile time limit
	CodeAgent        = "agent_failed"   // CLI exited with an error
	CodeAgentAuth    = "agent_auth"     // CLI could not sign in (expired session, bad key) → sign in to the CLI again
	CodeGit          = "git"            // worktree / diff failed
	CodeInterrupted  = "interrupted"    // app stopped while running
	CodeReplyTooLong = "reply_too_long" // «Отправить»: the reply is longer than the platform accepts
	CodeDirtyFolder  = "dirty_folder"   // direct fix refused: the folder has uncommitted changes (commit or remove them first)
)

// Publisher is the platform side of publishing a fix (GitHub: internal/provider/github).
type Publisher interface {
	DefaultBranch(ctx context.Context, repo string) (string, error)
	// GitToken is the user token git pushes with (never given to the agent).
	GitToken(ctx context.Context) (string, error)
	FindPullRequest(ctx context.Context, repo, head string) (provider.PullRequest, bool, error)
	CreatePullRequest(ctx context.Context, repo string, pr provider.NewPullRequest) (provider.PullRequest, error)
	Login() string
}

// Options wires the runner.
type Options struct {
	Store    *store.Store
	Settings func() config.Settings
	DataDir  string
	// Publisher pushes branches and opens draft PRs; nil disables «Создать PR».
	Publisher Publisher
	// Reply posts an approved reply draft (syncer.Reply); nil disables «Отправить».
	Reply func(ctx context.Context, itemID int64, body string) (store.Comment, error)
	// ReplyThreaded reports whether a reply on platform lands inside the item's
	// thread (provider Capabilities.ReplyThreaded); off, a reply draft starts
	// with @author. nil = every platform is threaded.
	ReplyThreaded func(platform string) bool
	// ReplyMarkdown reports whether platform renders Markdown in a reply
	// (provider Capabilities.Markdown); off or nil, the draft is plain text.
	ReplyMarkdown func(platform string) bool
	// MaxReply is the longest reply platform accepts in characters (provider
	// Capabilities.MaxReply); 0 or nil = no limit of its own, the prompt says none.
	MaxReply func(platform string) int
	// Labels lists repository labels and adds labels to issues; nil disables the label flow.
	Labels provider.Labeler
	// OnJob runs after every change of a job row (SSE job.changed).
	OnJob func(store.Job)
	// OnSteps streams new log steps of a running attempt (SSE job.log), batched.
	OnSteps func(jobID int64, attempt int, steps []Step)
	// OnFinished runs when an agent run ends (needs_review / failed): tray card.
	OnFinished func(store.Job)
	Log        *slog.Logger
	LookPath   func(string) (string, error) // default exec.LookPath
	Git        string                       // default "git"
	// GitURL maps a project web URL to the URL pushed to (default URL + ".git").
	GitURL func(projectURL string) string
	// Verify runs the verify command (tests override the shell); default cmd.exe /c.
	Shell []string
	// Now is the automation clock (24 h caps); default time.Now.
	Now func() time.Time
	// Exe is the app's own executable: agent runs get `<Exe> mcp --item <id>`
	// as a per-run MCP server (agents.jobMcp); "" = none.
	Exe string
}

// Runner owns the queue.
type Runner struct {
	opts Options

	minute time.Duration // unit of profile timeouts (tests shrink it)

	autoMu sync.Mutex // Automate batches

	mu      sync.Mutex
	running map[int64]*activeRun
	wake    chan struct{}
	wg      sync.WaitGroup
}

type activeRun struct {
	cancel  context.CancelCauseFunc
	project int64
	profile string
	folder  string // folderKey of the mapped folder (one job per folder)
}

var (
	// ErrCancelled ends a running job on the user's request.
	ErrCancelled = errors.New("cancelled by the user")
	// ErrNotAllowed: the job's state or flow does not allow the action.
	ErrNotAllowed = errors.New("runner: not allowed in this state")
	// ErrUnavailable: the action needs a publisher/replier that is not configured.
	ErrUnavailable = errors.New("runner: publishing is not available")
	// ErrBadRequest: invalid flow, profile or input.
	ErrBadRequest = errors.New("runner: bad request")
	// ErrNoFolder: a fix job was asked for items whose project has no usable
	// local folder (CodeNoFolder); no job was created.
	ErrNoFolder = errors.New("no usable local folder is mapped to the project; map it in Settings › Projects and folders")
	// ErrModItem: push / PR of a mod-page fix while agents.modPush is off for
	// that mod page (CodeModItem); the commit stays in the linked code folder.
	ErrModItem = errors.New("pushing fixes of mod-page items is off: the commit stays local (Settings › Agents › mod pages: allow push)")
	// ErrDirtyFolder: a direct fix (commits in the mapped folder itself) was
	// asked for a folder with uncommitted changes (CodeDirtyFolder); no job was
	// created: the agent's work would mix with them.
	ErrDirtyFolder = errors.New("the project folder has uncommitted changes: commit or remove them, then run again")
)

// New prepares a runner; Start runs it.
func New(opts Options) *Runner {
	if opts.Log == nil {
		opts.Log = slog.New(slog.DiscardHandler)
	}
	if opts.LookPath == nil {
		opts.LookPath = exec.LookPath
	}
	if opts.Git == "" {
		opts.Git = "git"
	}
	if opts.GitURL == nil {
		opts.GitURL = func(u string) string { return u + ".git" }
	}
	if len(opts.Shell) == 0 {
		opts.Shell = []string{"cmd.exe", "/d", "/s", "/c"}
	}
	for _, f := range []*func(store.Job){&opts.OnJob, &opts.OnFinished} {
		if *f == nil {
			*f = func(store.Job) {}
		}
	}
	if opts.OnSteps == nil {
		opts.OnSteps = func(int64, int, []Step) {}
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Runner{opts: opts, minute: time.Minute, running: map[int64]*activeRun{}, wake: make(chan struct{}, 1)}
}

// Start marks jobs interrupted by a previous run and schedules the queue until
// ctx ends; it returns once the loop runs. Wait blocks until runs finish.
func (r *Runner) Start(ctx context.Context) error {
	// A crash may have left agent process trees behind: end them first.
	if n, err := killOrphans(r.opts.DataDir); err != nil || n > 0 {
		r.opts.Log.Warn("runner: orphaned processes of a previous run", "killed", n, "err", err)
	}
	recovered, err := r.opts.Store.RecoverJobs(ctx)
	if err != nil {
		return err
	}
	for _, j := range recovered {
		r.setResultCode(ctx, j, CodeInterrupted)
		r.opts.Log.Warn("runner: job interrupted by restart", "job", j.ID)
	}
	r.wg.Go(func() { r.loop(ctx) })
	r.kick()
	return nil
}

// Wait blocks until the loop and every run have ended (after ctx is done).
func (r *Runner) Wait() { r.wg.Wait() }

// Refresh re-checks the queue and finished direct fixes soon (after a sync).
func (r *Runner) Refresh() { r.kick() }

func (r *Runner) kick() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

func (r *Runner) loop(ctx context.Context) {
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.wake:
		case <-tick.C:
		}
		r.schedule(ctx)
	}
}

// schedule starts queued jobs within the limits: one per project, the
// profile's maxParallel, agents.maxParallel overall. Oldest first.
func (r *Runner) schedule(ctx context.Context) {
	r.closeResolved(ctx)
	queued, err := r.opts.Store.JobsInState(ctx, store.JobQueued)
	if err != nil {
		r.opts.Log.Error("runner: queue", "err", err)
		return
	}
	cfg := r.opts.Settings().Agents
	for _, j := range queued {
		r.mu.Lock()
		busyProject, perProfile := false, 0
		folder := folderKey(j.LocalPath)
		for _, a := range r.running {
			busyProject = busyProject || a.project == j.ProjectID || (folder != "" && a.folder == folder)
			if a.profile == j.ProfileID {
				perProfile++
			}
		}
		full := len(r.running) >= cfg.MaxParallel
		r.mu.Unlock()
		if full {
			return
		}
		prof, ok := cfg.Profile(j.ProfileID)
		if busyProject || (ok && perProfile >= prof.MaxParallel) {
			continue
		}
		r.launch(ctx, j)
	}
}

func (r *Runner) launch(parent context.Context, j store.Job) {
	nj, err := r.opts.Store.UpdateJob(parent, j.ID, []string{store.JobQueued}, store.JobChange{
		State: new(store.JobRunning), Phase: new("prepare"), Error: new(""), Result: json.RawMessage("{}"), Started: true,
	})
	if err != nil {
		if !errors.Is(err, store.ErrJobState) {
			r.opts.Log.Error("runner: start job", "job", j.ID, "err", err)
		}
		return
	}
	ctx, cancel := context.WithCancelCause(parent)
	r.mu.Lock()
	r.running[j.ID] = &activeRun{cancel: cancel, project: j.ProjectID, profile: j.ProfileID, folder: folderKey(j.LocalPath)}
	r.mu.Unlock()
	r.opts.OnJob(nj)
	r.wg.Go(func() {
		defer func() {
			cancel(nil)
			r.mu.Lock()
			delete(r.running, j.ID)
			r.mu.Unlock()
			r.kick()
		}()
		r.run(ctx, nj)
	})
}

// Result is the JSON stored in jobs.result.
type Result struct {
	ErrorCode    string         `json:"errorCode,omitempty"`
	Agent        *AgentResult   `json:"agent,omitempty"`
	Diff         *DiffSummary   `json:"diff,omitempty"`
	Verify       *VerifyResult  `json:"verify,omitempty"`
	Review       *AgentResult   `json:"review,omitempty"`
	Draft        string         `json:"draft,omitempty"` // reply flow: the agent's reply
	PR           *PRResult      `json:"pr,omitempty"`
	Comment      *store.Comment `json:"comment,omitempty"`
	PublishError string         `json:"publishError,omitempty"`
	CleanupError string         `json:"cleanupError,omitempty"`
	BaseBranch   string         `json:"baseBranch,omitempty"`
	// Mode is the fix run mode (config.ModeDirect | ModeWorktreePR; "" = a
	// worktree job of an older build); Local holds the facts of a direct run.
	Mode  string       `json:"mode,omitempty"`
	Local *LocalResult `json:"local,omitempty"`
	// Label flow: Labels = the agent's picks that exist in the repository
	// (canonical names), DroppedLabels = picks that do not, AppliedLabels = the
	// names actually added to the issue (not already on it).
	Labels        []string `json:"labels,omitempty"`
	DroppedLabels []string `json:"droppedLabels,omitempty"`
	AppliedLabels []string `json:"appliedLabels,omitempty"`
	// Triage: the checked ranking and the fix jobs it queued.
	Triage *TriageResult `json:"triage,omitempty"`
}

// PRResult is the published draft PR.
type PRResult struct {
	Number int    `json:"number"`
	URL    string `json:"url"`
	Commit string `json:"commit"`
}

func parseResult(j store.Job) Result {
	var res Result
	_ = json.Unmarshal(j.Result, &res)
	return res
}

func encode(res Result) json.RawMessage {
	b, _ := json.Marshal(res) // plain structs
	return b
}

func (r *Runner) setResultCode(ctx context.Context, j store.Job, code string) {
	res := parseResult(j)
	res.ErrorCode = code
	nj, err := r.opts.Store.UpdateJob(ctx, j.ID, nil, store.JobChange{Result: encode(res)})
	if err == nil {
		r.opts.OnJob(nj)
	}
}

// codedError carries a Result.ErrorCode.
type codedError struct {
	code string
	err  error
}

func (e *codedError) Error() string { return e.err.Error() }
func (e *codedError) Unwrap() error { return e.err }

// coded tags err with code; a code err already carries (runAgent's
// CodeAgentAuth under a caller's CodeAgent) is the more precise one and stays.
func coded(code string, err error) error {
	if _, ok := errors.AsType[*codedError](err); ok {
		return err
	}
	return &codedError{code: code, err: err}
}

// ErrorCode is the error code err carries for the UI ("" = none): the API
// sends it along so the UI shows its own text and a link to the fix.
func ErrorCode(err error) string {
	var ce *codedError
	switch {
	case errors.As(err, &ce):
		return ce.code
	case errors.Is(err, ErrModItem):
		return CodeModItem
	case errors.Is(err, ErrNoFolder):
		return CodeNoFolder
	case errors.Is(err, ErrDirtyFolder):
		return CodeDirtyFolder
	}
	return ""
}

// say picks the app language's text of a user-facing job log line.
func (r *Runner) say(ru, en string) string {
	if r.opts.Settings().General.Language == "en" {
		return en
	}
	return ru
}

// run executes one attempt of job j and records the outcome.
func (r *Runner) run(ctx context.Context, j store.Job) {
	id, attempt := j.ID, j.Attempt // j is updated by the flow while the batcher's timer reads these
	batch := newStepBatcher(func(steps []Step) { r.opts.OnSteps(id, attempt, steps) })
	log, err := openJobLog(r.opts.DataDir, j.ID, j.Attempt, batch.add)
	if err != nil {
		r.finish(ctx, j, Result{ErrorCode: CodeGit}, err, "")
		batch.close()
		return
	}
	res := Result{}
	var state string
	switch j.Flow {
	case flowFix:
		state, err = r.runFix(ctx, &j, &res, log)
	case flowReply:
		state, err = r.runReply(ctx, &j, &res, log)
	case flowLabel:
		state, err = r.runLabel(ctx, &j, &res, log)
	case flowTriage:
		state, err = r.runTriage(ctx, &j, &res, log)
	default:
		err = fmt.Errorf("unknown flow %q", j.Flow)
	}
	if cause := context.Cause(ctx); err != nil && cause != nil {
		err = cause // a killed git/agent step reports its own error; the reason is the cancel
	}
	switch {
	case errors.Is(err, ErrCancelled):
		log.add(StepInfo, r.say("отменено пользователем", "cancelled by the user"))
	case err != nil:
		log.add(StepError, err.Error())
	case state == store.JobDone:
		log.add(StepInfo, r.say("готово", "done"))
	default:
		log.add(StepInfo, r.say("готово к проверке", "ready for review"))
	}
	log.close()
	batch.close()
	r.finish(ctx, j, res, err, state)
}

// finish stores the outcome: needs_review on success, cancelled when the user
// cancelled (worktree removed), failed otherwise (worktree kept for a look).
func (r *Runner) finish(ctx context.Context, j store.Job, res Result, err error, state string) {
	// The run's context may be cancelled (user, shutdown): the outcome is still stored.
	ctx = context.WithoutCancel(ctx)
	c := store.JobChange{Phase: new(""), Finished: true}
	switch {
	case errors.Is(err, ErrCancelled):
		c.State, c.Error = new(store.JobCancelled), new(ErrCancelled.Error())
		if cerr := r.cleanup(ctx, j); cerr != nil {
			res.CleanupError = cerr.Error()
		} else {
			c.Worktree = new("")
		}
	case err != nil:
		var ce *codedError
		switch {
		case errors.Is(err, context.Canceled):
			res.ErrorCode, err = CodeInterrupted, errors.New("interrupted: the app stopped while the job was running")
		case errors.Is(err, errTimeout):
			res.ErrorCode = CodeTimeout
		case errors.As(err, &ce):
			res.ErrorCode = ce.code
		}
		c.State, c.Error = new(store.JobFailed), new(err.Error())
	default:
		c.State = new(state)
	}
	c.Result = encode(res)
	// Background context: a shutdown must still record the outcome.
	nj, uerr := r.opts.Store.UpdateJob(ctx, j.ID, []string{store.JobRunning}, c)
	if uerr != nil {
		r.opts.Log.Error("runner: finish job", "job", j.ID, "err", uerr)
		return
	}
	r.opts.Log.Info("runner: job finished", "job", j.ID, "state", nj.State, "err", err)
	r.opts.OnJob(nj)
	if nj.State != store.JobCancelled {
		r.opts.OnFinished(nj)
	}
}

func (r *Runner) phase(ctx context.Context, j *store.Job, name string) {
	nj, err := r.opts.Store.UpdateJob(ctx, j.ID, []string{store.JobRunning}, store.JobChange{Phase: new(name)})
	if err == nil {
		*j = nj
		r.opts.OnJob(nj)
	}
}

// profileFor returns the job's agent profile.
func (r *Runner) profileFor(j store.Job) (config.AgentProfile, config.Agents, error) {
	cfg := r.opts.Settings().Agents
	p, ok := cfg.Profile(j.ProfileID)
	if !ok {
		return p, cfg, coded(CodeNoProfile, fmt.Errorf("agent profile %q does not exist (Settings › Agents)", j.ProfileID))
	}
	return p, cfg, nil
}

func (r *Runner) jobFiles(j store.Job) (string, error) {
	dir := jobDir(r.opts.DataDir, j.ID)
	return dir, os.MkdirAll(dir, 0o750)
}

func (r *Runner) worktreePath(j store.Job) string {
	return filepath.Join(r.opts.DataDir, "worktrees", dirSlug(j.Repo), strconv.FormatInt(j.ID, 10))
}

// folderError is the CodeNoFolder error of a fix job whose project has no
// mapped folder or whose folder is gone, nil when the folder exists (a git
// clone of the project or any other folder: folders.Status.Exists). Enqueue
// rejects such items up front; runFix checks again (the mapping may change meanwhile).
func folderError(project, localPath, projectURL string) error {
	st := folders.Check(localPath, projectURL)
	if st.Exists() {
		return nil
	}
	msg := "no local folder is mapped to " + project
	if localPath != "" {
		msg = fmt.Sprintf("the local folder %s of %s is not usable (%s)", localPath, project, st)
	}
	return coded(CodeNoFolder, errors.New(msg+"; map it in Settings › Projects and folders"))
}

// runFix: worktree from the mapped clone → agent → diff → verify → review.
func (r *Runner) runFix(ctx context.Context, j *store.Job, res *Result, log *jobLog) (string, error) {
	prof, cfg, err := r.profileFor(*j)
	if err != nil {
		return "", err
	}
	in, err := r.opts.Store.JobInput(ctx, j.ItemID)
	if err != nil {
		return "", err
	}
	if in.Mod && in.CodeProject == "" {
		return "", coded(CodeNoFolder, errors.New("the mod page "+in.ProjectName+" is not linked to a code project; link the mod to a project with a local folder (Projects)"))
	}
	if err := folderError(in.CodeRepo(), in.LocalPath, in.ProjectURL); err != nil {
		return "", err
	}
	if _, err := r.resolveExe(prof); err != nil {
		return "", coded(CodeNoCLI, err)
	}
	files, err := r.jobFiles(*j)
	if err != nil {
		return "", coded(CodeGit, err)
	}
	if st := folders.Check(in.LocalPath, in.ProjectURL); st != folders.StatusOK {
		// Not a clone of the project (no .git, or another remote): the agent edits
		// the folder in place; no worktree, commit, push or PR.
		return r.runFolder(ctx, j, res, log, prof, cfg, in, files, st)
	}
	if cfg.ModeFor(in.CodeKey) == config.ModeDirect {
		return r.runDirect(ctx, j, res, log, prof, cfg, in, files)
	}
	res.Mode = config.ModeWorktreePR

	baseBranch, sha, err := r.baseRef(ctx, in.LocalPath, in.CodeRepo(), log)
	if err != nil {
		return "", coded(CodeGit, err)
	}
	res.BaseBranch = baseBranch
	wt := r.worktreePath(*j)
	want := branchPrefix + strconv.Itoa(in.Number) + "-" + branchSlug(in.Title)
	log.addf(StepInfo, "git worktree add %s (%s from %s %s)", wt, want, baseBranch, shortSHA(sha))
	branch, err := r.addWorktree(ctx, in.LocalPath, wt, want, sha, j.ID)
	if err != nil {
		return "", coded(CodeGit, err)
	}
	if nj, err := r.opts.Store.UpdateJob(ctx, j.ID, nil, store.JobChange{
		Branch: new(branch), Worktree: new(wt), BaseSHA: new(sha),
	}); err == nil {
		*j = nj
		r.opts.OnJob(nj)
	}
	if ctx.Err() != nil {
		return "", context.Cause(ctx)
	}

	r.phase(ctx, j, "agent")
	pin := promptInput{in: in, branch: branch}
	system, task := prompts(cfg, flowFix, pin)
	agent, err := r.runAgent(ctx, agentSpec{item: j.ItemID, repo: j.ProjectKey, profile: prof, flow: flowFix, dir: wt, workDir: files, system: system, task: task}, log)
	res.Agent = &agent
	if err != nil {
		if errors.Is(err, errTimeout) || errors.Is(err, ErrCancelled) {
			return "", err
		}
		return "", coded(CodeAgent, err)
	}

	diffSum, diff, err := r.collectDiff(ctx, wt, sha)
	if err != nil {
		return "", coded(CodeGit, err)
	}
	res.Diff = &diffSum
	if err := os.WriteFile(diffPath(r.opts.DataDir, j.ID, j.Attempt), []byte(diff), 0o600); err != nil {
		return "", coded(CodeGit, err)
	}
	log.addf(StepInfo, "diff: %d files, +%d −%d", len(diffSum.Files), diffSum.Added, diffSum.Deleted)
	if len(diffSum.Files) == 0 {
		return store.JobNeedsReview, nil // nothing changed: the agent's summary says why
	}

	pa := cfg.Projects[in.CodeKey]
	if cmd, label := r.verifyCommand(in.LocalPath, wt, pa); len(cmd) > 0 {
		r.phase(ctx, j, "verify")
		v := r.verify(ctx, wt, files, cmd, label, log)
		res.Verify = &v
		if ctx.Err() != nil {
			return "", context.Cause(ctx)
		}
	}

	if vid := cfg.Roles.Verifier; vid != "" {
		if vp, ok := cfg.Profile(vid); ok {
			r.phase(ctx, j, "review")
			pin.diff = diff
			system, task := prompts(cfg, flowReview, pin)
			rev, err := r.runAgent(ctx, agentSpec{item: j.ItemID, repo: j.ProjectKey, profile: vp, flow: flowReview, dir: wt, workDir: files, system: system, task: task, readOnly: true}, log)
			if err != nil {
				if errors.Is(err, ErrCancelled) {
					return "", err
				}
				rev.Error = err.Error()
			}
			res.Review = &rev
		}
	}
	return store.JobNeedsReview, nil
}

func shortSHA(s string) string {
	if len(s) > 10 {
		return s[:10]
	}
	return s
}

func diffPath(dataDir string, id int64, attempt int) string {
	return filepath.Join(jobDir(dataDir, id), strconv.Itoa(attempt)+".diff")
}

// ReadDiff returns the stored diff of attempt of job id ("" when none).
func ReadDiff(dataDir string, id int64, attempt int) (string, error) {
	b, err := os.ReadFile(diffPath(dataDir, id, attempt))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	return string(b), err
}

// runReply: the responder drafts a reply, read-only, in the mapped folder (or
// an empty scratch folder without one).
func (r *Runner) runReply(ctx context.Context, j *store.Job, res *Result, log *jobLog) (string, error) {
	prof, cfg, err := r.profileFor(*j)
	if err != nil {
		return "", err
	}
	in, err := r.opts.Store.JobInput(ctx, j.ItemID)
	if err != nil {
		return "", err
	}
	if _, err := r.resolveExe(prof); err != nil {
		return "", coded(CodeNoCLI, err)
	}
	files, dir, err := r.readOnlyDir(*j, in, log)
	if err != nil {
		return "", err
	}
	r.phase(ctx, j, "agent")
	system, task := prompts(cfg, flowReply, promptInput{in: in})
	mention := r.needsMention(in)
	if mention {
		task += "\n\n" + in.Platform + " has no reply threads: the reply is posted as a new comment that starts with @" + in.Author +
			" (IssueWatcher adds it; do not write it yourself)."
	}
	markdown := r.opts.ReplyMarkdown != nil && r.opts.ReplyMarkdown(in.Platform)
	task += replyStyleNote(in.Platform, markdown)
	task += replyLimitNote(r.maxReply(in.Platform), in, mention)
	agent, err := r.runAgent(ctx, agentSpec{item: j.ItemID, repo: j.ProjectKey, profile: prof, flow: flowReply, dir: dir, workDir: files, system: system, task: task, readOnly: true}, log)
	res.Agent = &agent
	if err != nil {
		if errors.Is(err, errTimeout) || errors.Is(err, ErrCancelled) {
			return "", err
		}
		return "", coded(CodeAgent, err)
	}
	res.Draft = agent.Reply
	if res.Draft == "" {
		res.Draft = agent.Final
	}
	if res.Draft == "" {
		return "", coded(CodeAgent, errors.New("the agent returned no reply text"))
	}
	if !markdown {
		res.Draft = stripBold(res.Draft)
	}
	if mention {
		res.Draft = provider.WithMention(res.Draft, in.Author)
	}
	return store.JobNeedsReview, nil
}

// replyStyleNote tells the agent how a reply reads: the maintainer's own short
// comment, and plain text where platform shows no Markdown (it posts as typed).
func replyStyleNote(platform string, markdown bool) string {
	note := "\n\nWrite the reply as the maintainer typing a comment yourself: first person, casual and direct, like a person, not a support bot. " +
		"Keep it short, usually a few sentences in one or two conversational paragraphs; answer the point without restating the question. " +
		"No headings, no \"Short answer:\" or \"TL;DR\" lead-ins, no bullet lists or bold labels, no sign-off or signature, " +
		"and no stock phrases such as \"Great question\", \"I hope this helps\" or \"Feel free to\"."
	if markdown {
		return note + "\n" + platform + " renders Markdown, but keep the reply plain prose: use Markdown only for code or a link when it is really needed."
	}
	return note + "\n" + platform + " shows comments as plain text, not Markdown: write plain text only, with no **, #, backticks, > quotes or list markers; give links as bare URLs."
}

// boldRe matches **text** within a line, text starting and ending with a
// non-space as Markdown requires (2 ** 3 ** 4 is no bold).
var boldRe = regexp.MustCompile(`\*\*(\S(?:[^*\n]*\S)?)\*\*`)

// stripBold drops the ** markers around bold text of a draft for a platform
// that shows them literally; unpaired or multi-line ** stay as written.
func stripBold(s string) string { return boldRe.ReplaceAllString(s, "$1") }

// needsMention: a reply to in goes to a platform without reply threads, so
// the draft addresses the item's author with @author (not the owner's own item).
func (r *Runner) needsMention(in store.JobInput) bool {
	return r.opts.ReplyThreaded != nil && !r.opts.ReplyThreaded(in.Platform) && in.Author != "" && !in.Mine
}

// maxReply is Options.MaxReply of platform (0 = no limit of its own).
func (r *Runner) maxReply(platform string) int {
	if r.opts.MaxReply == nil {
		return 0
	}
	return r.opts.MaxReply(platform)
}

// replyLimitNote tells the agent the platform's reply limit, less the
// «@author » IssueWatcher prepends when mention is on; "" without a limit.
func replyLimitNote(limit int, in store.JobInput, mention bool) string {
	if limit <= 0 {
		return ""
	}
	if mention {
		limit -= provider.ReplyLength("@" + in.Author + " ")
	}
	return "\n\n" + in.Platform + " refuses longer comments: the reply text must be at most " + strconv.Itoa(limit) +
		" characters (count them; be brief, cut rather than exceed)."
}

// readOnlyDir returns the job files folder and where a read-only agent runs:
// the mapped folder, or an empty scratch folder without one.
func (r *Runner) readOnlyDir(j store.Job, in store.JobInput, log *jobLog) (files, dir string, err error) {
	if files, err = r.jobFiles(j); err != nil {
		return "", "", err
	}
	dir = in.LocalPath
	if !folders.Check(in.LocalPath, in.ProjectURL).Exists() {
		dir = filepath.Join(files, "empty")
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return "", "", err
		}
		log.add(StepInfo, r.say("папка проекта не выбрана: агент отвечает только по тексту issue", "no local folder mapped: the agent answers from the issue text only"))
	}
	return files, dir, nil
}

// cleanup removes a fix job's worktree and local branch.
func (r *Runner) cleanup(ctx context.Context, j store.Job) error {
	if j.Flow != flowFix || (j.Worktree == "" && j.Branch == "") {
		return nil
	}
	in, err := r.opts.Store.JobInput(ctx, j.ItemID)
	if err != nil {
		return err
	}
	if in.LocalPath == "" {
		if j.Worktree != "" {
			return os.RemoveAll(j.Worktree)
		}
		return nil
	}
	return r.removeWorktree(ctx, in.LocalPath, j.Worktree, j.Branch)
}

// stepBatcher coalesces log steps into one SSE message per 250 ms (or 40 steps),
// so a chatty agent cannot overflow a tab's event buffer.
type stepBatcher struct {
	mu      sync.Mutex
	pending []Step
	flush   func([]Step)
	timer   *time.Timer
	closed  bool
}

func newStepBatcher(flush func([]Step)) *stepBatcher { return &stepBatcher{flush: flush} }

func (b *stepBatcher) add(s Step) {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	b.pending = append(b.pending, s)
	if len(b.pending) >= 40 {
		out := b.take()
		b.mu.Unlock()
		b.flush(out)
		return
	}
	if b.timer == nil {
		b.timer = time.AfterFunc(250*time.Millisecond, func() {
			b.mu.Lock()
			out := b.take()
			b.mu.Unlock()
			if len(out) > 0 {
				b.flush(out)
			}
		})
	}
	b.mu.Unlock()
}

// take returns and clears the pending steps (mu held).
func (b *stepBatcher) take() []Step {
	out := b.pending
	b.pending = nil
	if b.timer != nil {
		b.timer.Stop()
		b.timer = nil
	}
	return out
}

func (b *stepBatcher) close() {
	b.mu.Lock()
	out := b.take()
	b.closed = true
	b.mu.Unlock()
	if len(out) > 0 {
		b.flush(out)
	}
}
