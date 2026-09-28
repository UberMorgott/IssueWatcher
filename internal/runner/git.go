package runner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// gitTimeout bounds one git command (fetch and push included).
const gitTimeout = 5 * time.Minute

// git runs the git executable with args in dir; env entries are added to the
// scrubbed environment. It returns trimmed stdout, or an error with stderr.
func (r *Runner) git(ctx context.Context, dir string, env []string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	full := append([]string{"-c", "core.longpaths=true", "-c", "core.quotepath=false"}, args...)
	cmd := exec.CommandContext(ctx, r.opts.Git, full...) //nolint:gosec // G204: fixed git binary, dispatcher-built args
	cmd.Dir = dir
	cmd.Env = append(agentEnv(), append([]string{"GIT_TERMINAL_PROMPT=0", "GCM_INTERACTIVE=never"}, env...)...)
	prepare(cmd)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = strings.TrimSpace(out.String())
		}
		return "", fmt.Errorf("git %s: %w: %s", args[0], err, clipRunes(msg, 1500))
	}
	return strings.TrimRight(out.String(), "\r\n"), nil
}

// gitOK reports whether a git command succeeds (rev-parse --verify probes).
func (r *Runner) gitOK(ctx context.Context, dir string, args ...string) bool {
	_, err := r.git(ctx, dir, nil, args...)
	return err == nil
}

var slugRe = regexp.MustCompile(`[^a-z0-9]+`)

// branchSlug makes a short ASCII branch fragment from an issue title.
func branchSlug(title string) string {
	s := strings.Trim(slugRe.ReplaceAllString(strings.ToLower(title), "-"), "-")
	if len(s) > 40 {
		s = strings.TrimRight(s[:40], "-")
	}
	if s == "" {
		s = "issue"
	}
	return s
}

// dirSlug is a file-system-safe short name for a project (owner/repo → repo).
func dirSlug(project string) string {
	if _, name, ok := strings.Cut(project, "/"); ok {
		project = name
	}
	s := strings.Trim(slugRe.ReplaceAllString(strings.ToLower(project), "-"), "-")
	if len(s) > 30 {
		s = s[:30]
	}
	if s == "" {
		s = "project"
	}
	return s
}

// baseRef picks the branch a fix starts from: the provider's default branch,
// else origin/HEAD, else main/master. It fetches it first (offline: the local
// remote-tracking ref is used, with a warning).
func (r *Runner) baseRef(ctx context.Context, repoDir, project string, log *jobLog) (ref, sha string, err error) {
	branch := ""
	if r.opts.Publisher != nil {
		b, err := r.opts.Publisher.DefaultBranch(ctx, project)
		if err != nil {
			log.addf(StepInfo, "default branch from the platform failed (%v); using the local clone's", err)
		}
		branch = b
	}
	if branch == "" {
		if head, err := r.git(ctx, repoDir, nil, "symbolic-ref", "--short", "refs/remotes/origin/HEAD"); err == nil {
			branch = strings.TrimPrefix(head, "origin/")
		}
	}
	if branch == "" {
		for _, b := range []string{"main", "master"} {
			if r.gitOK(ctx, repoDir, "rev-parse", "--verify", "--quiet", "refs/remotes/origin/"+b) ||
				r.gitOK(ctx, repoDir, "rev-parse", "--verify", "--quiet", "refs/heads/"+b) {
				branch = b
				break
			}
		}
	}
	if branch == "" {
		return "", "", errors.New("cannot tell the default branch of the local clone")
	}
	hasOrigin := r.gitOK(ctx, repoDir, "remote", "get-url", "origin")
	if hasOrigin {
		log.addf(StepInfo, "git fetch origin %s", branch)
		if _, err := r.git(ctx, repoDir, nil, "fetch", "--no-tags", "origin", branch); err != nil {
			log.addf(StepInfo, "fetch failed, using the last fetched state: %v", err)
		}
	}
	for _, ref := range []string{"refs/remotes/origin/" + branch, "refs/heads/" + branch} {
		if sha, err := r.git(ctx, repoDir, nil, "rev-parse", "--verify", "--quiet", ref+"^{commit}"); err == nil {
			return branch, sha, nil
		}
	}
	return "", "", fmt.Errorf("branch %s not found in the local clone", branch)
}

// addWorktree creates branch at sha in a new worktree at path. A taken branch
// name gets the job id appended.
func (r *Runner) addWorktree(ctx context.Context, repoDir, path, branch, sha string, jobID int64) (string, error) {
	if r.gitOK(ctx, repoDir, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch) {
		branch += "-j" + strconv.FormatInt(jobID, 10)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return "", fmt.Errorf("worktree dir: %w", err)
	}
	if _, err := os.Stat(path); err == nil { // leftover of a crashed attempt
		_ = r.removeWorktree(ctx, repoDir, path, "") // best effort; worktree add reports what is left
	}
	if _, err := r.git(ctx, repoDir, nil, "worktree", "add", "--no-track", "-b", branch, path, sha); err != nil {
		return "", err
	}
	return branch, nil
}

// removeWorktree deletes the job's worktree and its local branch (iw/… only).
// Errors are returned joined but every step is tried.
func (r *Runner) removeWorktree(ctx context.Context, repoDir, path, branch string) error {
	var errs []error
	if path != "" {
		if _, err := r.git(ctx, repoDir, nil, "worktree", "remove", "--force", path); err != nil {
			if _, statErr := os.Stat(path); statErr == nil {
				if rmErr := os.RemoveAll(path); rmErr != nil {
					errs = append(errs, fmt.Errorf("remove %s: %w", path, rmErr))
				}
			}
			_, _ = r.git(ctx, repoDir, nil, "worktree", "prune")
		}
	}
	if strings.HasPrefix(branch, branchPrefix) && r.gitOK(ctx, repoDir, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch) {
		if _, err := r.git(ctx, repoDir, nil, "branch", "-D", branch); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// FileChange is one changed path of a fix.
type FileChange struct {
	Path    string `json:"path"`
	Status  string `json:"status"` // A, M, D, R, …
	Added   int    `json:"added"`  // -1 = binary
	Deleted int    `json:"deleted"`
}

// DiffSummary is the dispatcher's own view of what a fix changed.
type DiffSummary struct {
	Files     []FileChange `json:"files"`
	Added     int          `json:"added"`
	Deleted   int          `json:"deleted"`
	Bytes     int          `json:"bytes"`
	Truncated bool         `json:"truncated"` // stored diff cut at maxStoredDiff
	Commits   int          `json:"commits"`   // commits the agent made itself
}

const maxStoredDiff = 5 << 20

// collectDiff stages everything in the worktree (dispatcher-owned) and diffs it
// against base, including files the agent committed or left untracked.
func (r *Runner) collectDiff(ctx context.Context, wt, base string) (DiffSummary, string, error) {
	sum := DiffSummary{Files: []FileChange{}}
	if _, err := r.git(ctx, wt, nil, "add", "-A"); err != nil {
		return sum, "", err
	}
	if n, err := r.git(ctx, wt, nil, "rev-list", "--count", base+"..HEAD"); err == nil {
		sum.Commits, _ = strconv.Atoi(n)
	}
	status, err := r.git(ctx, wt, nil, "diff", "--cached", "--name-status", "-M", base)
	if err != nil {
		return sum, "", err
	}
	st := map[string]string{}
	for line := range strings.SplitSeq(status, "\n") {
		f := strings.Split(line, "\t")
		if len(f) >= 2 {
			st[f[len(f)-1]] = f[0][:1]
		}
	}
	num, err := r.git(ctx, wt, nil, "diff", "--cached", "--numstat", "-M", base)
	if err != nil {
		return sum, "", err
	}
	for line := range strings.SplitSeq(num, "\n") {
		f := strings.SplitN(line, "\t", 3)
		if len(f) != 3 {
			continue
		}
		path := f[2]
		if i := strings.Index(path, " => "); i >= 0 { // rename: a => b, {a => b}/c
			path = renamedPath(path)
		}
		fc := FileChange{Path: path, Status: st[path], Added: -1, Deleted: -1}
		if a, err := strconv.Atoi(f[0]); err == nil {
			fc.Added, fc.Deleted = a, atoi(f[1])
			sum.Added += a
			sum.Deleted += fc.Deleted
		}
		if fc.Status == "" {
			fc.Status = "M"
		}
		sum.Files = append(sum.Files, fc)
	}
	diff, err := r.git(ctx, wt, nil, "diff", "--cached", "-M", base)
	if err != nil {
		return sum, "", err
	}
	sum.Bytes = len(diff)
	if len(diff) > maxStoredDiff {
		diff, sum.Truncated = clipHead(diff, maxStoredDiff, "(diff truncated)"), true
	}
	return sum, diff, nil
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

// renamedPath resolves numstat's rename notation to the new path.
func renamedPath(p string) string {
	if i, j := strings.Index(p, "{"), strings.Index(p, "}"); i >= 0 && j > i {
		inner := p[i+1 : j]
		_, to, _ := strings.Cut(inner, " => ")
		return strings.ReplaceAll(p[:i]+to+p[j+1:], "//", "/")
	}
	_, to, _ := strings.Cut(p, " => ")
	return to
}
