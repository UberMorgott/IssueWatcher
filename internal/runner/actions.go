package runner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/config"
	"github.com/UberMorgott/issuewatcher/internal/provider"
	"github.com/UberMorgott/issuewatcher/internal/store"
)

// Queued is one item's outcome of Enqueue.
type Queued struct {
	ItemID int64      `json:"itemId"`
	Job    *store.Job `json:"job,omitempty"`
	// Error: "exists" (an unfinished job of this flow; Job is that one),
	// "not_found", "no_folder" (fix without a usable local folder; no job),
	// "no_labels" (label job of a mod page item: labels exist on code project
	// items only; no job), or a message.
	Error string `json:"error,omitempty"`
	// Hint with no_folder: "link_mod" = a mod page item whose page is not linked
	// to a code project (link the mod to a project with a folder).
	Hint string `json:"hint,omitempty"`
	// Dirty with dirty_folder (CodeDirtyFolder): `git status --porcelain` lines
	// of the folder a direct fix would commit in; no job.
	Dirty []string `json:"dirty,omitempty"`
}

// enqueueDirty is the pre-flight of a fix of item id: the uncommitted changes
// of its folder when the fix would run there in direct mode. A folder with a
// job running in it is skipped (its agent's changes are in flight); runDirect
// checks again when the job starts.
func (r *Runner) enqueueDirty(ctx context.Context, cfg config.Agents, id int64) ([]string, error) {
	in, err := r.opts.Store.JobInput(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	key := folderKey(in.LocalPath)
	busy := false
	r.mu.Lock()
	for _, a := range r.running {
		busy = busy || (key != "" && a.folder == key)
	}
	r.mu.Unlock()
	if busy {
		return nil, nil
	}
	return r.directDirty(ctx, cfg, in)
}

// HintLinkMod: link the mod page to a code project with a folder first.
const HintLinkMod = "link_mod"

// ErrorNoLabels (Queued.Error): a label job of a mod page item.
const ErrorNoLabels = "no_labels"

// errMod wraps err (ErrBadRequest / ErrNotAllowed): the label flow never
// touches a mod page item; the shared labeler would write to a GitHub issue
// named after the mod page.
func errMod(err error) error {
	return fmt.Errorf("%w: labels exist on code project (GitHub) issues only, not on mod page items", err)
}

// MaxBatch bounds one Enqueue call.
const MaxBatch = 500

// Enqueue queues one job per item for flow ("fix" | "reply" | "label") with
// profileID ("" = the flow's role: coder / responder; label uses the responder).
func (r *Runner) Enqueue(ctx context.Context, itemIDs []int64, flow, profileID string) ([]Queued, error) {
	if flow != flowFix && flow != flowReply && flow != flowLabel {
		return nil, fmt.Errorf("%w: flow must be fix, reply or label", ErrBadRequest)
	}
	if flow == flowLabel && r.opts.Labels == nil {
		return nil, ErrUnavailable
	}
	if len(itemIDs) == 0 || len(itemIDs) > MaxBatch {
		return nil, fmt.Errorf("%w: 1–%d items", ErrBadRequest, MaxBatch)
	}
	cfg := r.opts.Settings().Agents
	if profileID == "" {
		profileID = cfg.Roles.Coder
		if flow == flowReply || flow == flowLabel {
			profileID = cfg.Roles.Responder
		}
	}
	if _, ok := cfg.Profile(profileID); !ok {
		return nil, fmt.Errorf("%w: no agent profile %q (Settings › Agents)", ErrBadRequest, profileID)
	}
	out := make([]Queued, 0, len(itemIDs))
	seen := map[int64]bool{}
	noFolder, noLabels, nDirty := 0, 0, 0
	for _, id := range itemIDs {
		if seen[id] {
			continue
		}
		seen[id] = true
		q := Queued{ItemID: id}
		if flow == flowFix || flow == flowLabel { // a fix runs in the mapped folder; labels exist on code project items only
			it, err := r.opts.Store.AutomationItemFacts(ctx, id)
			switch {
			case errors.Is(err, store.ErrNotFound):
				q.Error = "not_found"
			case err != nil:
				return out, err
			case flow == flowLabel:
				if it.Mod {
					q.Error = ErrorNoLabels
					noLabels++
				}
			case folderError("", it.LocalPath, it.ProjectURL) != nil:
				q.Error = CodeNoFolder
				if it.NeedsLink {
					q.Hint = HintLinkMod
				}
				noFolder++
			}
			if q.Error == "" && flow == flowFix {
				dirty, err := r.enqueueDirty(ctx, cfg, id)
				if err != nil {
					return out, err
				}
				if len(dirty) > 0 {
					q.Error, q.Dirty = CodeDirtyFolder, dirty
					nDirty++
				}
			}
			if q.Error != "" {
				out = append(out, q)
				continue
			}
		}
		j, err := r.opts.Store.CreateJob(ctx, id, flow, profileID, store.OriginManual, "")
		switch {
		case err == nil:
			q.Job = &j
			r.opts.OnJob(j)
		case errors.Is(err, store.ErrJobExists):
			q.Job, q.Error = &j, "exists"
		case errors.Is(err, store.ErrNotFound):
			q.Error = "not_found"
		default:
			return out, err
		}
		out = append(out, q)
	}
	if nDirty > 0 && nDirty+noFolder == len(out) {
		return out, ErrDirtyFolder // nothing queued: a direct fix would mix with the folder's uncommitted changes
	}
	if noFolder > 0 && noFolder == len(out) {
		return out, ErrNoFolder // nothing queued: every item lacks a usable folder
	}
	if noLabels > 0 && noLabels == len(out) {
		return out, errMod(ErrBadRequest) // nothing queued: every item is on a mod page
	}
	r.kick()
	return out, nil
}

// Cancel stops a queued or running job. A running job ends asynchronously
// (process tree killed, worktree removed); the returned row may still say running.
func (r *Runner) Cancel(ctx context.Context, id int64) (store.Job, error) {
	r.mu.Lock()
	a := r.running[id]
	r.mu.Unlock()
	if a != nil {
		a.cancel(ErrCancelled)
		return r.opts.Store.Job(ctx, id)
	}
	j, err := r.opts.Store.UpdateJob(ctx, id, []string{store.JobQueued}, store.JobChange{
		State: new(store.JobCancelled), Error: new(ErrCancelled.Error()), Finished: true,
	})
	if errors.Is(err, store.ErrJobState) {
		return j, ErrNotAllowed
	}
	if err == nil {
		r.opts.OnJob(j)
	}
	return j, err
}

// Dismiss («Отклонить») drops a result under review (or a failed attempt, or
// a finished folder-mode fix): state cancelled, worktree and branch removed.
func (r *Runner) Dismiss(ctx context.Context, id int64) (store.Job, error) {
	j, err := r.opts.Store.Job(ctx, id)
	if err != nil {
		return j, err
	}
	from := []string{store.JobNeedsReview, store.JobFailed}
	if folderDone(j) {
		from = append(from, store.JobDone)
	}
	if !slices.Contains(from, j.State) {
		return j, ErrNotAllowed
	}
	if j, err = r.claim(ctx, id, from, "dismiss"); err != nil {
		return j, err
	}
	ctx = context.WithoutCancel(ctx) // claimed: a closed tab must not leave the job locked
	res := parseResult(j)
	c := store.JobChange{State: new(store.JobCancelled), Error: new("dismissed"), Phase: new("")}
	if err := r.cleanup(ctx, j); err != nil {
		res.CleanupError = err.Error()
	} else {
		c.Worktree, res.CleanupError = new(""), ""
	}
	c.Result = encode(res)
	nj, err := r.opts.Store.UpdateJob(ctx, id, []string{store.JobRunning}, c)
	if err == nil {
		r.opts.OnJob(nj)
	}
	return nj, err
}

// claim moves job id from one of the states from into running/phase, so a
// concurrent Dismiss, Retry or publish (lockPublish) of the same job gets
// ErrNotAllowed instead of racing its cleanup. The caller ends the claim
// with an update from running.
func (r *Runner) claim(ctx context.Context, id int64, from []string, phase string) (store.Job, error) {
	nj, err := r.opts.Store.UpdateJob(ctx, id, from, store.JobChange{State: new(store.JobRunning), Phase: new(phase)})
	if errors.Is(err, store.ErrJobState) {
		return nj, ErrNotAllowed
	}
	if err == nil {
		r.opts.OnJob(nj)
	}
	return nj, err
}

// Retry queues a new attempt of a failed, cancelled or unpublished job from a
// clean start (the old worktree is removed). Published (done) jobs cannot be
// retried, except a folder-mode fix: nothing was published, the new attempt
// runs in the same folder.
func (r *Runner) Retry(ctx context.Context, id int64) (store.Job, error) {
	j, err := r.opts.Store.Job(ctx, id)
	if err != nil {
		return j, err
	}
	from := []string{store.JobFailed, store.JobCancelled, store.JobNeedsReview}
	if folderDone(j) {
		from = append(from, store.JobDone)
	}
	if !slices.Contains(from, j.State) {
		return j, ErrNotAllowed
	}
	prev := j // the attempt as it ended: its snapshot and the state a failed Retry restores
	state, phase := j.State, j.Phase
	if j, err = r.claim(ctx, id, from, "retry"); err != nil {
		return j, err
	}
	ctx = context.WithoutCancel(ctx)                // claimed: a closed tab must not leave the job locked
	release := func(err error) (store.Job, error) { // back to the state before the claim
		if nj, uerr := r.opts.Store.UpdateJob(ctx, id, []string{store.JobRunning}, store.JobChange{State: &state, Phase: &phase}); uerr == nil {
			r.opts.OnJob(nj)
			j = nj
		}
		return j, err
	}
	if err := r.cleanup(ctx, j); err != nil {
		return release(fmt.Errorf("remove the previous worktree: %w", err))
	}
	if err := writeAttempt(r.opts.DataDir, prev); err != nil {
		return release(err)
	}
	nj, err := r.opts.Store.UpdateJob(ctx, id, []string{store.JobRunning}, store.JobChange{
		State: new(store.JobQueued), NextAttempt: true, Phase: new(""), Error: new(""),
		Branch: new(""), Worktree: new(""), BaseSHA: new(""), Result: encode(Result{}),
	})
	if err != nil {
		return nj, err
	}
	r.opts.OnJob(nj)
	r.kick()
	return nj, nil
}

// folderDone: a fix that ran in a folder without the project's git and ended
// done («Изменено в папке»). Nothing was published, so it can be retried or
// dismissed like a result under review.
func folderDone(j store.Job) bool {
	return j.Flow == flowFix && j.State == store.JobDone && parseResult(j).Mode == ModeFolder
}

// lockPublish moves a needs_review job of flow into running/publish so two
// clicks cannot publish twice.
func (r *Runner) lockPublish(ctx context.Context, id int64, flow string) (store.Job, error) {
	j, err := r.opts.Store.Job(ctx, id)
	if err != nil {
		return j, err
	}
	if j.Flow != flow || j.State != store.JobNeedsReview {
		return j, ErrNotAllowed
	}
	nj, err := r.opts.Store.UpdateJob(ctx, id, []string{store.JobNeedsReview}, store.JobChange{
		State: new(store.JobRunning), Phase: new("publish"),
	})
	if errors.Is(err, store.ErrJobState) {
		return nj, ErrNotAllowed
	}
	if err == nil {
		r.opts.OnJob(nj)
	}
	return nj, err
}

// unlockPublish ends a publish: done (with cleanup) or back to needs_review with the error.
func (r *Runner) unlockPublish(ctx context.Context, j store.Job, res Result, perr error) (store.Job, error) {
	ctx = context.WithoutCancel(ctx) // a closed tab must not leave the job locked
	c := store.JobChange{Phase: new("")}
	if perr != nil {
		res.PublishError = perr.Error()
		c.State = new(store.JobNeedsReview)
	} else {
		res.PublishError = ""
		c.State, c.Finished = new(store.JobDone), true
		if err := r.cleanup(ctx, j); err != nil {
			res.CleanupError = err.Error()
		} else {
			c.Worktree = new("")
		}
	}
	c.Result = encode(res)
	nj, err := r.opts.Store.UpdateJob(ctx, j.ID, []string{store.JobRunning}, c)
	if err != nil {
		return nj, err
	}
	r.opts.OnJob(nj)
	if perr != nil {
		return nj, perr
	}
	return nj, nil
}

// CreatePR («Создать PR») commits the worktree if the agent left changes
// uncommitted, pushes the branch with the user's token and opens a draft PR
// that says "Fixes #N". Synchronous; an existing PR for the branch is reused.
func (r *Runner) CreatePR(ctx context.Context, id int64) (store.Job, error) {
	if r.opts.Publisher == nil {
		return store.Job{}, ErrUnavailable
	}
	if j, err := r.opts.Store.Job(ctx, id); err != nil {
		return j, err
	} else if parseResult(j).Mode == ModeFolder {
		return j, ErrNotAllowed // folder mode: no git, nothing to push
	} else if err := r.modPushAllowed(j); err != nil {
		return j, err
	}
	j, err := r.lockPublish(ctx, id, flowFix)
	if err != nil {
		return j, err
	}
	res := parseResult(j)
	log, lerr := openAppendLog(r.opts.DataDir, j.ID, j.Attempt, func(s Step) { r.opts.OnSteps(j.ID, j.Attempt, []Step{s}) })
	if lerr != nil {
		return r.unlockPublish(ctx, j, res, lerr)
	}
	defer log.close()
	pr, perr := r.publish(ctx, j, &res, log)
	if perr != nil {
		log.add(StepError, "publish failed: "+perr.Error())
	} else {
		res.PR = &pr
		log.addf(StepInfo, "draft PR #%d: %s", pr.Number, pr.URL)
	}
	return r.unlockPublish(ctx, j, res, perr)
}

func (r *Runner) publish(ctx context.Context, j store.Job, res *Result, log *jobLog) (PRResult, error) {
	pub := r.opts.Publisher
	if j.Worktree == "" || j.Branch == "" {
		return PRResult{}, errors.New("the job has no worktree (dismissed or cleaned up)")
	}
	if _, err := os.Stat(j.Worktree); err != nil {
		return PRResult{}, fmt.Errorf("worktree %s is gone", j.Worktree)
	}
	in, err := r.opts.Store.JobInput(ctx, j.ItemID)
	if err != nil {
		return PRResult{}, err
	}
	wt := j.Worktree
	if _, err := r.git(ctx, wt, nil, "add", "-A"); err != nil {
		return PRResult{}, err
	}
	if !r.gitOK(ctx, wt, "diff", "--cached", "--quiet") {
		summary := ""
		if res.Agent != nil {
			summary = res.Agent.Summary
		}
		msg := "fix: " + oneLineTitle(in.Title) + " (#" + strconv.Itoa(in.Number) + ")"
		if in.Mod { // #N is the mod page's local number, not an issue of the code repo
			msg = "fix: " + oneLineTitle(in.Title) + "\n\nReported on " + in.URL
		}
		if summary != "" {
			msg += "\n\n" + summary
		}
		var ident []string
		if email, _ := r.git(ctx, wt, nil, "config", "user.email"); email == "" {
			login := pub.Login()
			if login == "" {
				login = "issuewatcher"
			}
			ident = []string{"-c", "user.name=" + login, "-c", "user.email=" + login + "@users.noreply.github.com"}
		}
		log.add(StepInfo, "git commit: "+strings.SplitN(msg, "\n", 2)[0])
		msgFile := filepath.Join(jobDir(r.opts.DataDir, j.ID), "commit-msg.txt")
		if err := os.WriteFile(msgFile, []byte(msg+"\n"), 0o600); err != nil {
			return PRResult{}, err
		}
		if _, err := r.git(ctx, wt, nil, append(ident, "commit", "-q", "-F", msgFile)...); err != nil {
			return PRResult{}, err
		}
	}
	head, err := r.git(ctx, wt, nil, "rev-parse", "HEAD")
	if err != nil {
		return PRResult{}, err
	}
	if n, _ := r.git(ctx, wt, nil, "rev-list", "--count", j.BaseSHA+"..HEAD"); n == "0" {
		return PRResult{}, errors.New("no changes to publish")
	}
	token, err := pub.GitToken(ctx)
	if err != nil {
		return PRResult{}, err
	}
	url := r.opts.GitURL(in.ProjectURL)
	log.addf(StepInfo, "git push %s %s", url, j.Branch)
	// The token reaches git through its environment (GIT_CONFIG_*), never the
	// command line; credential helpers are off for this push.
	env, auth := tokenEnv(url, token)
	if _, err := r.git(ctx, wt, env, "push", "--no-verify", url, "+HEAD:refs/heads/"+j.Branch); err != nil {
		return PRResult{}, errors.New(strings.ReplaceAll(err.Error(), auth, "***"))
	}
	if pr, ok, err := pub.FindPullRequest(ctx, in.CodeRepo(), j.Branch); err == nil && ok {
		log.addf(StepInfo, "PR for %s already exists", j.Branch)
		return PRResult{Number: pr.Number, URL: pr.URL, Commit: head}, nil
	}
	base := res.BaseBranch
	if base == "" {
		base, _ = pub.DefaultBranch(ctx, in.CodeRepo())
	}
	body, title := "Fixes #"+strconv.Itoa(in.Number), oneLineTitle(in.Title)+" (#"+strconv.Itoa(in.Number)+")"
	if in.Mod {
		body, title = "Fix for a report on "+in.ProjectName+": "+in.URL, oneLineTitle(in.Title)
	}
	if res.Agent != nil && res.Agent.Summary != "" {
		body += "\n\n" + res.Agent.Summary
	}
	if res.Verify != nil {
		mark := "passed"
		if !res.Verify.OK {
			mark = "FAILED"
		}
		body += "\n\nVerify (`" + res.Verify.Command + "`): " + mark
	}
	body += "\n\n_Drafted by a local agent through IssueWatcher; reviewed by the maintainer before publishing._"
	pr, err := pub.CreatePullRequest(ctx, in.CodeRepo(), provider.NewPullRequest{
		Title: title, Head: j.Branch, Base: base, Body: body, Draft: true,
	})
	if err != nil {
		return PRResult{}, err
	}
	return PRResult{Number: pr.Number, URL: pr.URL, Commit: head}, nil
}

// modPushAllowed refuses publishing a mod-page fix (its commit is in the
// linked code project) unless agents.modPush allows it for that mod page.
func (r *Runner) modPushAllowed(j store.Job) error {
	if j.Mod && !r.opts.Settings().Agents.ModPushFor(j.ProjectKey) {
		return ErrModItem
	}
	return nil
}

// SendReply («Отправить») posts the (edited) reply of a reply job.
func (r *Runner) SendReply(ctx context.Context, id int64, body string) (store.Job, error) {
	if r.opts.Reply == nil {
		return store.Job{}, ErrUnavailable
	}
	if strings.TrimSpace(body) == "" {
		return store.Job{}, fmt.Errorf("%w: empty reply", ErrBadRequest)
	}
	j, err := r.lockPublish(ctx, id, flowReply)
	if err != nil {
		return j, err
	}
	res := parseResult(j)
	c, perr := r.opts.Reply(ctx, j.ItemID, body)
	if perr == nil {
		res.Comment, res.Draft = &c, body
	}
	return r.unlockPublish(ctx, j, res, perr)
}

// VerifyResult is the verify command's outcome.
type VerifyResult struct {
	Command    string `json:"command"`
	OK         bool   `json:"ok"`
	ExitCode   int    `json:"exitCode"`
	Output     string `json:"output"` // tail
	DurationMS int64  `json:"durationMs"`
	TimedOut   bool   `json:"timedOut,omitempty"`
}

const (
	verifyTimeout   = 30 * time.Minute
	maxVerifyOutput = 12 << 10
)

// verifyCommand picks the check: `aegis verify` when the mapped folder has
// Aegis enabled (unless turned off for the project), else the project's command.
// aegisVerifyLabel is verifyCommand's label of the Aegis gate.
const aegisVerifyLabel = "aegis verify"

func (r *Runner) verifyCommand(localPath, wt string, pa config.ProjectAgent) (argv []string, label string) {
	if !pa.NoAegis {
		if st, err := os.Stat(filepath.Join(localPath, ".aegis")); err == nil && st.IsDir() {
			if exe, err := r.opts.LookPath("aegis"); err == nil {
				return []string{exe, "verify", "-root", wt}, aegisVerifyLabel
			}
		}
	}
	if c := strings.TrimSpace(pa.Verify); c != "" {
		return append(append([]string{}, r.opts.Shell...), c), c
	}
	return nil, ""
}

func (r *Runner) verify(ctx context.Context, dir, jobFiles string, argv []string, label string, log *jobLog) VerifyResult {
	v := VerifyResult{Command: label, ExitCode: -1}
	log.add(StepInfo, "verify: "+label)
	ctx, cancel := context.WithTimeout(ctx, verifyTimeout)
	defer cancel()
	cmd := exec.Command(argv[0], argv[1:]...) //nolint:gosec,noctx // G204: the user's configured verify command; killed via its job object
	cmd.Dir = dir
	cmd.Env = agentEnv()
	var (
		mu  sync.Mutex
		out bytes.Buffer
	)
	line := func(b []byte) {
		mu.Lock()
		out.Write(b)
		out.WriteByte('\n')
		if out.Len() > 4*maxVerifyOutput { // keep the tail only
			tail := append([]byte(nil), out.Bytes()[out.Len()-maxVerifyOutput:]...)
			out.Reset()
			out.Write(tail)
		}
		mu.Unlock()
	}
	start := time.Now()
	p, err := startProc(cmd, "", line, line)
	if err != nil {
		v.Output = err.Error()
		log.add(StepError, "verify: "+err.Error())
		return v
	}
	stopTrack := trackProcs(p.tree, filepath.Join(jobFiles, procsFile))
	select {
	case <-p.done:
	case <-ctx.Done():
		p.tree.kill()
		<-p.done
		v.TimedOut = ctx.Err() == context.DeadlineExceeded
	}
	if err := p.finish(); err != nil {
		log.add(StepError, "verify: "+err.Error())
	}
	stopTrack()
	v.DurationMS = time.Since(start).Milliseconds()
	if cmd.ProcessState != nil {
		v.ExitCode = cmd.ProcessState.ExitCode()
	}
	v.OK = v.ExitCode == 0 && !v.TimedOut
	mu.Lock()
	o := out.String()
	mu.Unlock()
	if len(o) > maxVerifyOutput {
		o = "…\n" + o[len(o)-maxVerifyOutput:]
	}
	v.Output = o
	kind := StepInfo
	if !v.OK {
		kind = StepError
	}
	log.addf(kind, "verify %s: exit %d in %s", map[bool]string{true: "passed", false: "failed"}[v.OK], v.ExitCode, time.Duration(v.DurationMS)*time.Millisecond)
	return v
}

// openAppendLog continues an attempt's log (publishing).
func openAppendLog(dataDir string, id int64, attempt int, emit func(Step)) (*jobLog, error) {
	if err := os.MkdirAll(jobDir(dataDir, id), 0o750); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(logPath(dataDir, id, attempt), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	return &jobLog{f: f, emit: emit}, nil
}
