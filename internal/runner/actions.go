package runner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
	// "not_found", or a message.
	Error string `json:"error,omitempty"`
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
	for _, id := range itemIDs {
		if seen[id] {
			continue
		}
		seen[id] = true
		j, err := r.opts.Store.CreateJob(ctx, id, flow, profileID, store.OriginManual, "")
		q := Queued{ItemID: id}
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

// Dismiss («Отклонить») drops a result under review (or a failed attempt):
// state cancelled, worktree and branch removed.
func (r *Runner) Dismiss(ctx context.Context, id int64) (store.Job, error) {
	j, err := r.opts.Store.Job(ctx, id)
	if err != nil {
		return j, err
	}
	if j.State != store.JobNeedsReview && j.State != store.JobFailed {
		return j, ErrNotAllowed
	}
	res := parseResult(j)
	c := store.JobChange{State: new(store.JobCancelled), Error: new("dismissed"), Phase: new("")}
	if err := r.cleanup(ctx, j); err != nil {
		res.CleanupError = err.Error()
	} else {
		c.Worktree, res.CleanupError = new(""), ""
	}
	c.Result = encode(res)
	nj, err := r.opts.Store.UpdateJob(ctx, id, []string{store.JobNeedsReview, store.JobFailed}, c)
	if errors.Is(err, store.ErrJobState) {
		return nj, ErrNotAllowed
	}
	if err == nil {
		r.opts.OnJob(nj)
	}
	return nj, err
}

// Retry queues a new attempt of a failed, cancelled or unpublished job from a
// clean start (the old worktree is removed). Published (done) jobs cannot be retried.
func (r *Runner) Retry(ctx context.Context, id int64) (store.Job, error) {
	j, err := r.opts.Store.Job(ctx, id)
	if err != nil {
		return j, err
	}
	from := []string{store.JobFailed, store.JobCancelled, store.JobNeedsReview}
	if j.State != store.JobFailed && j.State != store.JobCancelled && j.State != store.JobNeedsReview {
		return j, ErrNotAllowed
	}
	if err := r.cleanup(ctx, j); err != nil {
		return j, fmt.Errorf("remove the previous worktree: %w", err)
	}
	if err := writeAttempt(r.opts.DataDir, j); err != nil {
		return j, err
	}
	nj, err := r.opts.Store.UpdateJob(ctx, id, from, store.JobChange{
		State: new(store.JobQueued), NextAttempt: true, Phase: new(""), Error: new(""),
		Branch: new(""), Worktree: new(""), BaseSHA: new(""), Result: encode(Result{}),
	})
	if errors.Is(err, store.ErrJobState) {
		return nj, ErrNotAllowed
	}
	if err != nil {
		return nj, err
	}
	r.opts.OnJob(nj)
	r.kick()
	return nj, nil
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
	env, auth := tokenEnv(token)
	if _, err := r.git(ctx, wt, env, "push", "--no-verify", url, "+HEAD:refs/heads/"+j.Branch); err != nil {
		return PRResult{}, errors.New(strings.ReplaceAll(err.Error(), auth, "***"))
	}
	if pr, ok, err := pub.FindPullRequest(ctx, in.ProjectName, j.Branch); err == nil && ok {
		log.addf(StepInfo, "PR for %s already exists", j.Branch)
		return PRResult{Number: pr.Number, URL: pr.URL, Commit: head}, nil
	}
	base := res.BaseBranch
	if base == "" {
		base, _ = pub.DefaultBranch(ctx, in.ProjectName)
	}
	body := "Fixes #" + strconv.Itoa(in.Number)
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
	pr, err := pub.CreatePullRequest(ctx, in.ProjectName, provider.NewPullRequest{
		Title: oneLineTitle(in.Title) + " (#" + strconv.Itoa(in.Number) + ")", Head: j.Branch, Base: base, Body: body, Draft: true,
	})
	if err != nil {
		return PRResult{}, err
	}
	return PRResult{Number: pr.Number, URL: pr.URL, Commit: head}, nil
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
func (r *Runner) verifyCommand(localPath, wt string, pa config.ProjectAgent) (argv []string, label string) {
	if !pa.NoAegis {
		if st, err := os.Stat(filepath.Join(localPath, ".aegis")); err == nil && st.IsDir() {
			if exe, err := r.opts.LookPath("aegis"); err == nil {
				return []string{exe, "verify", "-root", wt}, "aegis verify"
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
