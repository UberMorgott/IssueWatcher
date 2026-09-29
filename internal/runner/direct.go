package runner

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/config"
	"github.com/UberMorgott/issuewatcher/internal/store"
)

// Direct mode (config.ModeDirect, the default): the coder runs in the mapped
// folder itself with the user's own CLI settings, project rules and MCP
// servers, plus our task prompt. It commits "Fixes #N" and never pushes; the
// dispatcher judges the run by git facts, not by the agent's claim.

// Outcomes of a direct fix (LocalResult.Outcome).
const (
	OutcomeFixedLocal    = "fixed_local"    // commit(s) made, not on the remote yet: waits for push
	OutcomePushed        = "pushed"         // commit is on a remote branch; waits for the issue to close
	OutcomeClosed        = "closed"         // the issue closed on the platform
	OutcomeNotReproduced = "not_reproduced" // no commit, the agent could not reproduce it
	OutcomeNeedsInfo     = "needs_info"     // no commit, the issue lacks information
	OutcomeNoCommit      = "no_commit"      // no commit and no reason given (or "fixed" without a commit)
	OutcomeFailed        = "failed"         // the agent reported failure
)

// LocalResult is what a direct fix did to the mapped folder (Result.Local).
type LocalResult struct {
	Dir         string        `json:"dir"`
	Branch      string        `json:"branch"` // checked out at the end ("" = detached HEAD)
	StartSHA    string        `json:"startSha"`
	HeadSHA     string        `json:"headSha"`
	Commits     []LocalCommit `json:"commits"`  // StartSHA..HeadSHA, oldest first
	FixesRef    bool          `json:"fixesRef"` // a commit message closes the issue (Fixes #N)
	DirtyBefore []string      `json:"dirtyBefore,omitempty"`
	DirtyAfter  []string      `json:"dirtyAfter,omitempty"`
	Outcome     string        `json:"outcome"`
	Pushed      bool          `json:"pushed"`
	PushedAt    string        `json:"pushedAt,omitempty"`
	Closed      bool          `json:"closed"`
}

// LocalCommit is one commit the run added.
type LocalCommit struct {
	SHA     string `json:"sha"`
	Subject string `json:"subject"`
	Fixes   bool   `json:"fixes"`
}

// flowFixDirect selects the direct fix schema and prompt (the job flow stays fix).
const flowFixDirect = "fix-direct"

const maxStatusLines = 200

// runDirect runs the coder in the mapped folder and records the git facts.
func (r *Runner) runDirect(ctx context.Context, j *store.Job, res *Result, log *jobLog,
	prof config.AgentProfile, cfg config.Agents, in store.JobInput, files string,
) (string, error) {
	dir := in.LocalPath
	res.Mode = config.ModeDirect
	loc := &LocalResult{Dir: dir, Commits: []LocalCommit{}}
	res.Local = loc
	loc.StartSHA, _ = r.git(ctx, dir, nil, "rev-parse", "--verify", "--quiet", "HEAD")
	loc.Branch, _ = r.git(ctx, dir, nil, "symbolic-ref", "--short", "-q", "HEAD")
	before, err := r.status(ctx, dir)
	if err != nil {
		return "", coded(CodeGit, err)
	}
	loc.DirtyBefore = before
	log.addf(StepInfo, "direct mode: working in %s (branch %s at %s)", dir, orDash(loc.Branch), shortSHA(loc.StartSHA))
	if len(before) > 0 {
		log.addf(StepInfo, "warning: the folder has %d uncommitted change(s); the agent is told to leave them alone", len(before))
	}
	r.saveResult(ctx, j, *res)
	if ctx.Err() != nil {
		return "", context.Cause(ctx)
	}

	r.phase(ctx, j, "agent")
	system, task := prompts(cfg, flowFixDirect, promptInput{in: in})
	if len(before) > 0 {
		task += "\n\nUncommitted changes that were already in the folder before you started (the maintainer's work in progress: do not commit, revert or reformat them):\n" +
			strings.Join(before, "\n")
	}
	// Claude: our whole prompt is appended to the user's own system prompt; the
	// user turn only starts the task. Codex gets both in its input.
	kickoff := "Handle the IssueWatcher task described in the appended instructions: issue #" + strconv.Itoa(in.Number) + " of " + in.ProjectName + "."
	agent, agentErr := r.runAgent(ctx, agentSpec{profile: prof, flow: flowFixDirect, dir: dir, workDir: files,
		system: system + "\n\n" + task, task: kickoff}, log)
	res.Agent = &agent

	// Facts first, whatever the agent said (also after a failure or cancel: it may have committed).
	r.phase(ctx, j, "check")
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	defer cancel()
	diff, ferr := r.localFacts(fctx, j, in, loc, log)
	switch {
	case loc.Pushed:
		loc.Outcome = OutcomePushed
	case len(loc.Commits) > 0:
		loc.Outcome = OutcomeFixedLocal
	case agent.Status == "not_reproduced" || agent.Status == "needs_info" || agent.Status == "failed":
		loc.Outcome = agent.Status
	default:
		loc.Outcome = OutcomeNoCommit
	}
	if agentErr != nil {
		if errors.Is(agentErr, errTimeout) || errors.Is(agentErr, ErrCancelled) {
			return "", agentErr
		}
		return "", coded(CodeAgent, agentErr)
	}
	if ferr != nil {
		return "", coded(CodeGit, ferr)
	}
	if loc.Outcome == OutcomeFailed {
		msg := agent.Summary
		if msg == "" {
			msg = "the agent reported that it could not fix the issue"
		}
		return "", coded(CodeAgent, errors.New(msg))
	}
	if len(loc.Commits) == 0 {
		return store.JobNeedsReview, nil
	}

	pa := cfg.Projects[in.ProjectName]
	if c := strings.TrimSpace(pa.Verify); c != "" { // explicit project check only; the agent verified by the project's rules
		r.phase(ctx, j, "verify")
		v := r.verify(ctx, dir, files, append(append([]string{}, r.opts.Shell...), c), c, log)
		res.Verify = &v
		if ctx.Err() != nil {
			return "", context.Cause(ctx)
		}
	}
	if vid := cfg.Roles.Verifier; vid != "" {
		if vp, ok := cfg.Profile(vid); ok {
			r.phase(ctx, j, "review")
			system, task := prompts(cfg, flowReview, promptInput{in: in, diff: diff})
			rev, err := r.runAgent(ctx, agentSpec{profile: vp, flow: flowReview, dir: dir, workDir: files, system: system, task: task, readOnly: true}, log)
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

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// saveResult stores res on the running job (the UI shows warnings early).
func (r *Runner) saveResult(ctx context.Context, j *store.Job, res Result) {
	if nj, err := r.opts.Store.UpdateJob(ctx, j.ID, []string{store.JobRunning}, store.JobChange{Result: encode(res)}); err == nil {
		*j = nj
		r.opts.OnJob(nj)
	}
}

// status lists `git status --porcelain` lines of dir (clipped).
func (r *Runner) status(ctx context.Context, dir string) ([]string, error) {
	out, err := r.git(ctx, dir, nil, "status", "--porcelain")
	if err != nil {
		return nil, err
	}
	var lines []string
	for l := range strings.SplitSeq(out, "\n") {
		if strings.TrimSpace(l) == "" {
			continue
		}
		if len(lines) == maxStatusLines {
			lines = append(lines, "…")
			break
		}
		lines = append(lines, l)
	}
	return lines, nil
}

// closesRe matches GitHub's closing keywords for issue n ("Fixes #12",
// "closes owner/repo#12").
func closesRe(n int) *regexp.Regexp {
	return regexp.MustCompile(`(?i)\b(?:close[sd]?|fix(?:e[sd])?|resolve[sd]?):?\s+(?:[\w.-]+/[\w.-]+)?#` + strconv.Itoa(n) + `\b`)
}

// localFacts fills loc from git: commits since the start, "Fixes #N", the
// working tree afterwards, whether the head is on a remote branch; it stores
// the commits' diff and returns it.
func (r *Runner) localFacts(ctx context.Context, j *store.Job, in store.JobInput, loc *LocalResult, log *jobLog) (string, error) {
	dir := loc.Dir
	loc.Branch, _ = r.git(ctx, dir, nil, "symbolic-ref", "--short", "-q", "HEAD")
	head, err := r.git(ctx, dir, nil, "rev-parse", "--verify", "--quiet", "HEAD")
	if err != nil {
		return "", err
	}
	loc.HeadSHA = head
	after, err := r.status(ctx, dir)
	if err != nil {
		return "", err
	}
	loc.DirtyAfter = after
	rng := "HEAD"
	if loc.StartSHA != "" {
		rng = loc.StartSHA + "..HEAD"
	}
	out, err := r.git(ctx, dir, nil, "log", "--reverse", "--format=%H%x1f%s%x1f%B%x1e", rng)
	if err != nil {
		return "", err
	}
	re := closesRe(in.Number)
	loc.Commits = []LocalCommit{}
	for rec := range strings.SplitSeq(out, "\x1e") {
		f := strings.SplitN(strings.TrimLeft(rec, "\r\n"), "\x1f", 3)
		if len(f) < 3 {
			continue
		}
		c := LocalCommit{SHA: f[0], Subject: f[1], Fixes: re.MatchString(f[2])}
		loc.FixesRef = loc.FixesRef || c.Fixes
		loc.Commits = append(loc.Commits, c)
	}
	log.addf(StepInfo, "check: %d new commit(s), Fixes #%d: %t, uncommitted after: %d", len(loc.Commits), in.Number, loc.FixesRef, len(after))
	if len(loc.Commits) == 0 {
		return "", nil
	}
	if !loc.FixesRef {
		log.addf(StepInfo, "warning: no commit message says \"Fixes #%d\": pushing will not close the issue", in.Number)
	}
	from := loc.StartSHA
	if from == "" {
		from = emptyTree
	}
	sum, diff, err := r.diffOf(ctx, dir, from, head)
	if err != nil {
		return "", err
	}
	sum.Commits = len(loc.Commits)
	if err := os.WriteFile(diffPath(r.opts.DataDir, j.ID, j.Attempt), []byte(diff), 0o600); err != nil {
		return "", err
	}
	log.addf(StepInfo, "diff: %d files, +%d −%d", len(sum.Files), sum.Added, sum.Deleted)
	loc.Pushed = r.onRemote(ctx, dir, head, log)
	return diff, nil
}

// emptyTree is git's empty tree (diff base of a repository's first commit).
const emptyTree = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"

// onRemote fetches origin and reports whether sha is on a remote branch.
func (r *Runner) onRemote(ctx context.Context, dir, sha string, log *jobLog) bool {
	if !r.gitOK(ctx, dir, "remote", "get-url", "origin") {
		return false
	}
	if _, err := r.git(ctx, dir, nil, "fetch", "--no-tags", "-q", "origin"); err != nil {
		log.addf(StepInfo, "fetch failed, remote state may be stale: %v", err)
	}
	out, err := r.git(ctx, dir, nil, "branch", "-r", "--contains", sha)
	return err == nil && strings.TrimSpace(out) != ""
}

// Push («Push») pushes the commits of a direct fix from the mapped folder to
// its branch on the platform with the user's token. Never automatic.
func (r *Runner) Push(ctx context.Context, id int64) (store.Job, error) {
	j, err := r.opts.Store.Job(ctx, id)
	if err != nil {
		return j, err
	}
	res := parseResult(j)
	if res.Mode != config.ModeDirect || res.Local == nil || len(res.Local.Commits) == 0 || res.Local.Pushed {
		return j, ErrNotAllowed
	}
	if r.opts.Publisher == nil {
		return j, ErrUnavailable
	}
	j, err = r.lockPublish(ctx, id, flowFix)
	if err != nil {
		return j, err
	}
	res = parseResult(j)
	log, lerr := openAppendLog(r.opts.DataDir, j.ID, j.Attempt, func(s Step) { r.opts.OnSteps(j.ID, j.Attempt, []Step{s}) })
	if lerr != nil {
		return r.unlockPublish(ctx, j, res, lerr)
	}
	defer log.close()
	var perr error
	if in, err := r.opts.Store.JobInput(ctx, j.ItemID); err != nil {
		perr = err
	} else {
		perr = r.pushLocal(ctx, res.Local, r.opts.GitURL(in.ProjectURL), log)
	}
	if perr != nil {
		log.add(StepError, "push failed: "+perr.Error())
	} else {
		res.Local.Pushed, res.Local.PushedAt = true, time.Now().UTC().Format(time.RFC3339)
		if res.Local.Outcome == OutcomeFixedLocal {
			res.Local.Outcome = OutcomePushed
		}
		log.addf(StepInfo, "pushed %s to %s", shortSHA(res.Local.HeadSHA), res.Local.Branch)
	}
	nj, err := r.unlockPublish(ctx, j, res, perr)
	if perr == nil {
		r.markCarried(context.WithoutCancel(ctx), j, res.Local, log)
	}
	return nj, err
}

// markCarried ends the other direct fixes of the same project, folder and
// branch whose commits went along with a push (their head is an ancestor of
// the pushed one); a fix of another project sharing the clone, or on another
// branch, did not reach its own target. The push targets the project URL, not
// origin, so the remote-tracking refs that onRemote / closeResolved read never
// learn about it.
func (r *Runner) markCarried(ctx context.Context, pushedJob store.Job, pushed *LocalResult, log *jobLog) {
	jobs, err := r.opts.Store.JobsInState(ctx, store.JobNeedsReview)
	if err != nil {
		r.opts.Log.Error("runner: carried pushes", "err", err)
		return
	}
	for _, j := range jobs {
		res := parseResult(j)
		loc := res.Local
		if j.ID == pushedJob.ID || j.ProjectID != pushedJob.ProjectID || j.Flow != flowFix || res.Mode != config.ModeDirect ||
			loc == nil || len(loc.Commits) == 0 || loc.Pushed || loc.Branch != pushed.Branch || folderKey(loc.Dir) != folderKey(pushed.Dir) ||
			!r.gitOK(ctx, pushed.Dir, "merge-base", "--is-ancestor", loc.HeadSHA, pushed.HeadSHA) {
			continue
		}
		loc.Pushed, loc.PushedAt = true, pushed.PushedAt
		if loc.Outcome == OutcomeFixedLocal {
			loc.Outcome = OutcomePushed
		}
		nj, err := r.opts.Store.UpdateJob(ctx, j.ID, []string{store.JobNeedsReview},
			store.JobChange{State: new(store.JobDone), Finished: true, Result: encode(res)})
		if err != nil {
			continue // changed meanwhile (pushing, dismissed)
		}
		log.addf(StepInfo, "job %d: its commit %s went along with this push", j.ID, shortSHA(loc.HeadSHA))
		r.opts.OnJob(nj)
	}
}

// pushLocal pushes to url (the project's own push URL), not to origin: the
// token must only reach the platform, whatever origin points at now.
func (r *Runner) pushLocal(ctx context.Context, loc *LocalResult, url string, log *jobLog) error {
	if loc.Branch == "" {
		return errors.New("the folder was on a detached HEAD: push it by hand")
	}
	if _, err := os.Stat(filepath.Join(loc.Dir, ".git")); err != nil {
		return fmt.Errorf("folder %s is not a git clone any more", loc.Dir)
	}
	if !r.gitOK(ctx, loc.Dir, "cat-file", "-e", loc.HeadSHA+"^{commit}") {
		return fmt.Errorf("commit %s is gone from %s", shortSHA(loc.HeadSHA), loc.Dir)
	}
	token, err := r.opts.Publisher.GitToken(ctx)
	if err != nil {
		return err
	}
	env, secret := tokenEnv(url, token)
	log.addf(StepInfo, "git push %s %s:%s (%s)", url, shortSHA(loc.HeadSHA), loc.Branch, loc.Dir)
	// The user's own pre-push hooks run: this is their working copy.
	if _, err := r.git(ctx, loc.Dir, env, "push", url, loc.HeadSHA+":refs/heads/"+loc.Branch); err != nil {
		return errors.New(strings.ReplaceAll(err.Error(), secret, "***"))
	}
	return nil
}

// tokenEnv passes the user's token to git through its environment (never the
// command line), with credential helpers off; secret is the value to redact.
// The header is scoped to url (http.<url>.extraHeader): git matches it against
// the URL it actually talks to, after url.*.insteadOf / pushInsteadOf, so a
// rewrite in the clone's config never carries the token to another host.
func tokenEnv(url, token string) (env []string, secret string) {
	auth := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
	return []string{"GIT_CONFIG_COUNT=2",
		"GIT_CONFIG_KEY_0=http." + url + ".extraHeader", "GIT_CONFIG_VALUE_0=Authorization: Basic " + auth,
		"GIT_CONFIG_KEY_1=credential.helper", "GIT_CONFIG_VALUE_1="}, auth
}

// closeResolved marks direct fixes whose issue is closed now (the pushed
// "Fixes #N" commit closed it): outcome closed, needs_review → done. A commit
// that is still only local (the issue was closed another way) keeps the job
// under review, so it can still be pushed or dismissed.
func (r *Runner) closeResolved(ctx context.Context) {
	jobs, err := r.opts.Store.ClosedDirectFixes(ctx)
	if err != nil {
		r.opts.Log.Error("runner: closed direct fixes", "err", err)
		return
	}
	for _, j := range jobs {
		res := parseResult(j)
		if res.Local == nil {
			res.Local = &LocalResult{Commits: []LocalCommit{}}
		}
		res.Local.Closed = true
		c := store.JobChange{}
		localOnly := false
		if j.State == store.JobNeedsReview && len(res.Local.Commits) > 0 && !res.Local.Pushed {
			// Pushed by hand? Local remote-tracking refs only: no fetch in the scheduler.
			out, err := r.git(ctx, res.Local.Dir, nil, "branch", "-r", "--contains", res.Local.HeadSHA)
			if err == nil && strings.TrimSpace(out) != "" {
				res.Local.Pushed = true
			} else {
				localOnly = true
			}
		}
		if !localOnly {
			res.Local.Outcome = OutcomeClosed
			if j.State == store.JobNeedsReview {
				c.State, c.Finished = new(store.JobDone), true
			}
		}
		c.Result = encode(res)
		nj, err := r.opts.Store.UpdateJob(ctx, j.ID, []string{j.State}, c)
		if err != nil {
			continue // changed meanwhile (pushing, dismissed)
		}
		r.opts.Log.Info("runner: issue closed", "job", j.ID, "localOnly", localOnly)
		r.opts.OnJob(nj)
	}
}

// folderKey compares mapped folders (case-insensitive paths on Windows).
func folderKey(p string) string {
	if strings.TrimSpace(p) == "" {
		return ""
	}
	return strings.ToLower(filepath.Clean(p))
}
