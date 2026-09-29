package runner

import (
	"context"
	"errors"
	"io/fs"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/config"
	"github.com/UberMorgott/issuewatcher/internal/folders"
	"github.com/UberMorgott/issuewatcher/internal/store"
)

// Folder mode: the mapped folder is not a git clone of the project (no .git,
// or a clone of something else). The coder edits the folder in place; there is
// no worktree, commit, push or PR. The dispatcher judges the run by a file
// snapshot of the folder taken before and after the agent (path, size, mtime).

// ModeFolder is Result.Mode of a fix that ran in a folder without the project's git.
const ModeFolder = "folder"

// Outcomes of a folder-mode fix (LocalResult.Outcome; not_reproduced,
// needs_info and failed are shared with direct mode).
const (
	OutcomeChangedFolder = "changed_folder" // files in the folder changed; nothing to publish
	OutcomeNoChanges     = "no_changes"     // nothing changed and no reason given
)

// flowFixFolder selects the folder fix schema and prompt (the job flow stays fix).
const flowFixFolder = "fix-folder"

// folderFixPrompt is the task of a folder-mode fix (not a setting: it only
// adapts the direct task to a folder without git).
const folderFixPrompt = `Task from the maintainer: fix issue #{issue.number} "{issue.title}" of {repo} ({issue.url}).
You are working directly in the maintainer's folder ({localPath}). It is NOT a git clone of {repo} (there is no git repository, or it belongs to another project): this task has no branch, commit or push. Follow the folder's own rules if it has any (CLAUDE.md / AGENTS.md).

Issue:
{issue.body}

Discussion:
{comments}

Steps:
1. Reproduce or confirm the problem from the files. If it does not reproduce or is not a bug, change nothing and report not_reproduced; if the issue lacks information you need, change nothing and report needs_info.
2. Fix it with the smallest correct change, editing the files of this folder in place. Do not touch anything outside this folder.
3. Verify the change if the folder has a way to (build, tests, linters).
4. Do NOT use git here: no git init, add, commit, stash, checkout or reset. Do not push, create pull requests or post comments; the maintainer takes the changed files from this folder.
Finish with the structured result: status (fixed | not_reproduced | needs_info | failed), summary in the language of the issue (what was wrong, what changed, how it was verified), the files you changed (paths relative to the folder), the verify result, notes.`

// Snapshot bounds: a folder mapped at a huge tree must not stall the job.
const (
	maxSnapshotFiles = 200_000
	maxChangedFiles  = 500
)

// snapshotSkip are folders never walked (VCS data, dependency and cache trees).
var snapshotSkip = []string{".git", ".hg", ".svn", "node_modules", ".venv", "__pycache__", ".vs"}

type fileStamp struct {
	size  int64
	mtime int64
}

// folderSnapshot is the file list of a folder (relative slash paths).
type folderSnapshot struct {
	files     map[string]fileStamp
	truncated bool // maxSnapshotFiles reached: changes past it are not seen
}

// snapshotDir records every file under dir (size, mtime), skipping
// snapshotSkip folders and skip (the app's data folder, when it is inside dir).
func snapshotDir(ctx context.Context, dir, skip string) (folderSnapshot, error) {
	snap := folderSnapshot{files: map[string]fileStamp{}}
	skipKey := folderKey(skip)
	errStop := errors.New("stop")
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if path == dir {
				return err
			}
			return nil // unreadable entry: not ours to judge
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.IsDir() {
			if path != dir && (slices.Contains(snapshotSkip, d.Name()) || (skipKey != "" && folderKey(path) == skipKey)) {
				return fs.SkipDir
			}
			return nil
		}
		if len(snap.files) >= maxSnapshotFiles {
			snap.truncated = true
			return errStop
		}
		// A file gone meanwhile (or unreadable) is skipped.
		info, ierr := d.Info()
		rel, rerr := filepath.Rel(dir, path)
		if ierr == nil && rerr == nil {
			snap.files[filepath.ToSlash(rel)] = fileStamp{size: info.Size(), mtime: info.ModTime().UnixNano()}
		}
		return nil
	})
	if errors.Is(err, errStop) {
		err = nil
	}
	return snap, err
}

// changedFiles lists what differs from before to after as "A path", "M path",
// "D path" (sorted by path, at most maxChangedFiles plus a "…" line).
func changedFiles(before, after folderSnapshot) []string {
	var out []string
	for p, a := range after.files {
		b, ok := before.files[p]
		switch {
		case !ok:
			out = append(out, "A "+p)
		case a != b:
			out = append(out, "M "+p)
		}
	}
	for p := range before.files {
		if _, ok := after.files[p]; !ok && !after.truncated {
			out = append(out, "D "+p)
		}
	}
	slices.SortFunc(out, func(x, y string) int { return strings.Compare(x[2:], y[2:]) })
	if len(out) > maxChangedFiles {
		out = append(out[:maxChangedFiles], "… "+strconv.Itoa(len(out)-maxChangedFiles)+" more")
	}
	return out
}

// runFolder runs the coder in a mapped folder that is not a clone of the
// project and records which files changed.
func (r *Runner) runFolder(ctx context.Context, j *store.Job, res *Result, log *jobLog,
	prof config.AgentProfile, cfg config.Agents, in store.JobInput, files string, st folders.Status,
) (string, error) {
	dir := in.LocalPath
	res.Mode = ModeFolder
	loc := &LocalResult{Dir: dir, Commits: []LocalCommit{}, Changed: []string{}}
	res.Local = loc
	why := "no git repository"
	if st == folders.StatusMismatch {
		why = "its git remote is not " + in.CodeRepo()
	}
	log.addf(StepInfo, "folder mode: working in %s (%s): the agent edits it in place, no commit, no push", dir, why)
	before, err := snapshotDir(ctx, dir, r.opts.DataDir)
	if err != nil {
		return "", coded(CodeGit, err)
	}
	if before.truncated {
		log.addf(StepInfo, "warning: the folder has more than %d files: changes past them are not listed", maxSnapshotFiles)
	}
	r.saveResult(ctx, j, *res)
	if ctx.Err() != nil {
		return "", context.Cause(ctx)
	}

	r.phase(ctx, j, "agent")
	system, task := prompts(cfg, flowFixFolder, promptInput{in: in})
	kickoff := "Handle the IssueWatcher task described in the appended instructions: issue #" + strconv.Itoa(in.Number) + " of " + in.ProjectName + "."
	agent, agentErr := r.runAgent(ctx, agentSpec{item: j.ItemID, repo: j.ProjectKey, profile: prof, flow: flowFixFolder, dir: dir, workDir: files,
		system: system + "\n\n" + task, task: kickoff}, log)
	res.Agent = &agent

	// Facts first, whatever the agent said (also after a failure or cancel: it may have edited files).
	r.phase(ctx, j, "check")
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	defer cancel()
	after, ferr := snapshotDir(fctx, dir, r.opts.DataDir)
	if ferr == nil {
		loc.Changed = changedFiles(before, after)
		log.addf(StepInfo, "check: %d file(s) changed in the folder", len(loc.Changed))
	}
	switch {
	case len(loc.Changed) > 0:
		loc.Outcome = OutcomeChangedFolder
	case agent.Status == OutcomeNotReproduced || agent.Status == OutcomeNeedsInfo || agent.Status == OutcomeFailed:
		loc.Outcome = agent.Status
	default:
		loc.Outcome = OutcomeNoChanges
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
	if len(loc.Changed) == 0 {
		return store.JobNeedsReview, nil
	}

	pa := cfg.Projects[in.ProjectKey]
	if c := strings.TrimSpace(pa.Verify); c != "" {
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
			changes := "No git diff: the folder is not a git clone. Files changed in place (A added, M modified, D deleted), read them in the current directory:\n" +
				strings.Join(loc.Changed, "\n")
			system, task := prompts(cfg, flowReview, promptInput{in: in, diff: changes})
			rev, err := r.runAgent(ctx, agentSpec{item: j.ItemID, repo: j.ProjectKey, profile: vp, flow: flowReview, dir: dir, workDir: files, system: system, task: task, readOnly: true}, log)
			if err != nil {
				if errors.Is(err, ErrCancelled) {
					return "", err
				}
				rev.Error = err.Error()
			}
			res.Review = &rev
		}
	}
	// The change is already in place: nothing to push or publish.
	return store.JobDone, nil
}
