package release

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/config"
	"github.com/UberMorgott/issuewatcher/internal/provider"
	"github.com/UberMorgott/issuewatcher/internal/provider/factorio"
	"github.com/UberMorgott/issuewatcher/internal/provider/nexus"
	"github.com/UberMorgott/issuewatcher/internal/release/source"
	"github.com/UberMorgott/issuewatcher/internal/store"
)

const (
	buildTimeout = 30 * time.Minute
	bumpPrefix   = "chore(release): "
)

// runDir is data\release\<run>: the build worktree, logs and the artifact.
func (e *Engine) runDir(id int64) string {
	return filepath.Join(e.d.DataDir, "release", fmt.Sprint(id))
}

// folderGit runs git in the mapped folder (the owner's config; hooks are
// turned off where it writes).
func (e *Engine) folderGit(ctx context.Context, m *Manifest, args ...string) (string, error) {
	return e.d.Git.Run(ctx, m.Folder, nil, args...)
}

func (e *Engine) lockFolder(path string) (func(), error) {
	if e.d.Folders == nil {
		return func() {}, nil
	}
	return e.d.Folders.AcquireFolder(path)
}

// folderAt checks the mapped folder: on the branch, HEAD == want, clean.
func (e *Engine) folderAt(ctx context.Context, m *Manifest, want string) error {
	branch, err := e.folderGit(ctx, m, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return err
	}
	if branch != m.Branch {
		return fmt.Errorf("the folder is on %s, not %s", branch, m.Branch)
	}
	head, err := e.folderGit(ctx, m, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	if head != want {
		return fmt.Errorf("the folder's HEAD is %s, expected %s", short(head), short(want))
	}
	st, err := e.folderGit(ctx, m, "status", "--porcelain")
	if err != nil {
		return err
	}
	if st != "" {
		return errors.New("the folder has uncommitted changes")
	}
	return nil
}

// subjects are the commit subjects of base..head (all of head without a base).
func (e *Engine) subjects(ctx context.Context, dir, base, head string) []string {
	spec := head
	if base != "" {
		spec = base + ".." + head
	}
	out, err := e.d.Git.Run(ctx, dir, nil, "log", "--no-merges", "--format=%s", spec)
	if err != nil || out == "" {
		return nil
	}
	var subs []string
	for s := range strings.SplitSeq(out, "\n") {
		if s = strings.TrimSpace(s); s != "" && !strings.HasPrefix(s, bumpPrefix) {
			subs = append(subs, s)
		}
	}
	return subs
}

// changelogPreview computes the release body and the files the bump would
// change, on a temporary copy of the version and changelog files (the folder
// is not touched).
func (e *Engine) changelogPreview(dir string, p config.PublishProfile, version string, subjects []string) (text string, vfiles, cfiles []string, err error) {
	tmp, err := os.MkdirTemp("", "iw-release-")
	if err != nil {
		return "", nil, nil, err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	vs, cs := source.VersionSource(p.Version), source.ChangelogSource(p.Changelog)
	for _, rel := range []string{versionFile(vs), changelogFile(cs)} {
		if rel == "" {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel))) //nolint:gosec // G304: the profile's files in the mapped folder
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", nil, nil, err
		}
		dst := filepath.Join(tmp, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
			return "", nil, nil, err
		}
		if err := os.WriteFile(dst, b, 0o600); err != nil {
			return "", nil, nil, err
		}
	}
	if vs.Kind != source.KindGitTag {
		if vfiles, err = source.WriteVersion(tmp, vs, version); err != nil {
			return "", nil, nil, err
		}
	}
	text, cfiles, err = source.Entry(tmp, cs, version, subjects, e.d.Now())
	return text, vfiles, cfiles, err
}

func versionFile(vs source.VersionSource) string {
	switch vs.Kind {
	case source.KindFactorioInfo:
		return cmpOrStr(vs.Path, source.DefaultInfoJSON)
	case source.KindJSON, source.KindRegex:
		return vs.Path
	}
	return ""
}

func changelogFile(cs source.ChangelogSource) string {
	switch cs.Kind {
	case source.ChangelogFactorio:
		return cmpOrStr(cs.Path, source.DefaultFactorioChangelog)
	case source.ChangelogKeepAChangelog:
		return cmpOrStr(cs.Path, source.DefaultKeepAChangelog)
	}
	return ""
}

func cmpOrStr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// setManifest stores the bump facts in the run's manifest (filled once).
func (e *Engine) setManifest(ctx context.Context, c *rc, bumpSHA, changelog string) error {
	m := *c.m
	m.BumpSHA, m.Changelog = bumpSHA, changelog
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	r, err := e.d.Store.UpdateRun(context.WithoutCancel(ctx), c.run.ID, []string{store.RunRunning, store.RunHeld, store.RunPending},
		store.RunUpdate{Manifest: b})
	if err != nil {
		return err
	}
	c.run, c.m = r, &m
	e.d.OnChange(r.ID)
	return nil
}

// --- bump --------------------------------------------------------------------

// bump writes the version and changelog entry in the mapped folder and commits
// them as chore(release): vX.Y.Z (hooks off); nothing to change → bump sha = HEAD.
func (e *Engine) bump(ctx context.Context, c *rc) localResult {
	m := c.m
	if m.BumpSHA != "" {
		return localResult{ref: m.BumpSHA}
	}
	unlock, err := e.lockFolder(m.Folder)
	if err != nil {
		return localResult{hold: "folder_busy", err: err}
	}
	defer unlock()
	if err := e.folderAt(ctx, m, m.Head); err != nil {
		return localResult{hold: HeldFolderMoved, err: err}
	}
	vs, cs := source.VersionSource(m.Profile.Version), source.ChangelogSource(m.Profile.Changelog)
	var files []string
	if vs.Kind != source.KindGitTag {
		ch, err := source.WriteVersion(m.Folder, vs, m.Version)
		if err != nil {
			return localResult{err: err}
		}
		files = append(files, ch...)
	}
	text, ch, err := source.Entry(m.Folder, cs, m.Version, e.subjects(ctx, m.Folder, m.BaseTag, m.Head), e.d.Now())
	files = append(files, ch...)
	if err != nil {
		e.restore(ctx, m, files)
		return localResult{err: err}
	}
	sha := m.Head
	if len(files) > 0 {
		if sha, err = e.commitBump(ctx, c, files); err != nil {
			e.restore(ctx, m, files)
			return localResult{err: err}
		}
	}
	if err := e.setManifest(ctx, c, sha, text); err != nil {
		return localResult{err: err}
	}
	return localResult{ref: sha}
}

func (e *Engine) commitBump(ctx context.Context, c *rc, files []string) (string, error) {
	m := c.m
	if err := c.mir.ensure(ctx); err != nil { // its empty hooks dir
		return "", err
	}
	if _, err := e.folderGit(ctx, m, append([]string{"add", "--"}, files...)...); err != nil {
		return "", err
	}
	args := []string{"-c", "core.hooksPath=" + c.mir.hooks, "-c", "user.name=" + m.Identity[0], "-c", "user.email=" + m.Identity[1],
		"-c", "commit.gpgsign=false", "commit", "-q", "--no-verify", "-m", bumpPrefix + m.Tag, "--"}
	if _, err := e.folderGit(ctx, m, append(args, files...)...); err != nil {
		return "", err
	}
	return e.folderGit(ctx, m, "rev-parse", "HEAD")
}

// restore undoes the bump's file edits after a failed commit (best effort).
func (e *Engine) restore(ctx context.Context, m *Manifest, files []string) {
	if len(files) == 0 {
		return
	}
	_, _ = e.folderGit(ctx, m, append([]string{"reset", "-q", "--"}, files...)...)
	_, _ = e.folderGit(ctx, m, append([]string{"checkout", "-q", "--"}, files...)...)
}

// reconcileBump settles a bump interrupted by a crash from the folder's state.
func (e *Engine) reconcileBump(ctx context.Context, c *rc, st store.Step) (string, error) {
	m := c.m
	unclear := func(msg string) (string, error) {
		_, err := e.transition(ctx, st, store.StepUnknown, store.StepUnknown, store.StepUpdate{Error: new(msg)})
		return "check:bump", err
	}
	if m.BumpSHA != "" {
		_, err := e.transition(ctx, st, store.StepUnknown, store.StepSent, store.StepUpdate{ExternalRef: &m.BumpSHA, Error: new("")})
		return "", err
	}
	unlock, err := e.lockFolder(m.Folder)
	if err != nil {
		return unclear(err.Error())
	}
	defer unlock()
	if e.folderAt(ctx, m, m.Head) == nil {
		_, err := e.transition(ctx, st, store.StepUnknown, store.StepPending, store.StepUpdate{Error: new("")})
		return "", err
	}
	head, _ := e.folderGit(ctx, m, "rev-parse", "HEAD")
	parent, _ := e.folderGit(ctx, m, "rev-parse", "HEAD^")
	subject, _ := e.folderGit(ctx, m, "log", "-1", "--format=%s", "HEAD")
	if head == "" || parent != m.Head || subject != bumpPrefix+m.Tag || e.folderAt(ctx, m, head) != nil {
		return unclear("the folder is neither at the release head nor at its bump commit")
	}
	text, _, _, err := e.changelogPreview(m.Folder, m.Profile, m.Version, e.subjects(ctx, m.Folder, m.BaseTag, m.Head))
	if err != nil {
		return unclear("changelog: " + err.Error())
	}
	if err := e.setManifest(ctx, c, head, text); err != nil {
		return "", err
	}
	_, err = e.transition(ctx, st, store.StepUnknown, store.StepSent, store.StepUpdate{ExternalRef: &head, Error: new("bump commit found after a restart")})
	return "", err
}

// dropBump removes the app's own bump commit on cancel (see Cancel).
func (e *Engine) dropBump(ctx context.Context, m *Manifest) (bool, string) {
	unlock, err := e.lockFolder(m.Folder)
	if err != nil {
		return false, "bump commit kept: " + err.Error()
	}
	defer unlock()
	if err := e.folderAt(ctx, m, m.BumpSHA); err != nil {
		return false, "bump commit kept: " + err.Error()
	}
	mir := newMirror(e.d.Git, e.d.DataDir, m.Repo)
	if err := mir.ensure(ctx); err != nil {
		return false, "bump commit kept: " + err.Error()
	}
	if err := mir.fetchFolder(ctx, m.Folder, m.Branch, m.BumpSHA); err != nil {
		return false, "bump commit kept: " + err.Error()
	}
	url, env, secret, err := e.remote(ctx, m)
	if err != nil {
		return false, "bump commit kept: " + err.Error()
	}
	remoteHead, err := mir.fetchRemote(ctx, url, env, m.Branch)
	if err != nil {
		return false, "bump commit kept: cannot read the remote: " + redact(err, secret).Error()
	}
	on, err := mir.isAncestor(ctx, m.BumpSHA, remoteHead)
	if err != nil || on {
		return false, "bump commit kept: it may be on the remote"
	}
	if _, err := e.folderGit(ctx, m, "reset", "-q", "--keep", m.Head); err != nil {
		return false, "bump commit kept: " + err.Error()
	}
	return true, "bump commit " + short(m.BumpSHA) + " dropped"
}

// --- build -------------------------------------------------------------------

func expandOutput(p string, m *Manifest) string {
	return strings.NewReplacer("{name}", m.ModName, "{version}", m.Version).Replace(p)
}

// build checks the bump sha out of the mirror into data\release\<run>\src,
// runs the profile's build command there (job object, 30 min) and copies the
// declared output next to it; the worktree must stay clean but for it.
func (e *Engine) build(ctx context.Context, c *rc) localResult {
	m, p := c.m, c.m.Profile.Build
	dir := e.runDir(c.run.ID)
	if err := c.mir.ensure(ctx); err != nil {
		return localResult{err: err}
	}
	if err := c.mir.fetchFolder(ctx, m.Folder, m.Branch, m.BumpSHA); err != nil {
		return localResult{err: err}
	}
	wt := filepath.Join(dir, "src")
	e.removeWorktree(ctx, c.mir, wt)
	if _, err := c.mir.run(ctx, nil, "worktree", "add", "-q", "--detach", "--force", wt, m.BumpSHA); err != nil {
		return localResult{err: err}
	}
	defer e.removeWorktree(context.WithoutCancel(ctx), c.mir, wt)

	var src string
	var cmd CmdResult
	switch {
	case p.Path != "":
		src = p.Path
		if !filepath.IsAbs(src) {
			src = filepath.Join(m.Folder, filepath.FromSlash(src))
		}
	case p.Command != "" && e.d.Command != nil:
		cmd = e.d.Command(ctx, wt, p.Command, filepath.Join(dir, "logs"), buildTimeout)
		if !cmd.OK {
			return localResult{hold: "build_failed", err: fmt.Errorf("build exit %d: %s", cmd.ExitCode, tail(cmd.Output, 2000))}
		}
		out := filepath.ToSlash(filepath.Clean(expandOutput(p.Output, m)))
		src = filepath.Join(wt, filepath.FromSlash(out))
		status, err := c.mir.git.Run(ctx, wt, c.mir.env(), "status", "--porcelain", "--untracked-files=all")
		if err != nil {
			return localResult{err: err}
		}
		for line := range strings.SplitSeq(status, "\n") {
			if len(line) < 4 {
				continue
			}
			path := strings.Trim(line[3:], `"`)
			if path != out {
				return localResult{hold: HeldDirtyBuild, err: fmt.Errorf("the build changed %s besides %s", path, out)}
			}
		}
	default:
		return localResult{hold: CodeNoProfile, err: errors.New("no build command or archive path in the publish profile")}
	}
	art, err := copyArtifact(src, filepath.Join(dir, "artifact", m.Asset))
	if err != nil {
		return localResult{hold: "build_failed", err: err}
	}
	if _, err := e.d.Store.UpdateRun(context.WithoutCancel(ctx), c.run.ID, []string{store.RunRunning},
		store.RunUpdate{ArtifactSHA256: &art.SHA256}); err != nil {
		return localResult{err: err}
	}
	b, _ := json.Marshal(art)
	return localResult{ref: string(b)}
}

func (e *Engine) removeWorktree(ctx context.Context, mir *mirror, wt string) {
	if _, err := os.Stat(wt); err == nil {
		if _, err := mir.run(ctx, nil, "worktree", "remove", "--force", wt); err != nil {
			_ = os.RemoveAll(wt)
		}
	}
	_, _ = mir.run(ctx, nil, "worktree", "prune")
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}

// --- gate --------------------------------------------------------------------

// gate runs the project's verify gate in the mapped folder at the bump sha,
// under the shared folder lock.
func (e *Engine) gate(ctx context.Context, c *rc) localResult {
	m := c.m
	unlock, err := e.lockFolder(m.Folder)
	if err != nil {
		return localResult{hold: "folder_busy", err: err}
	}
	defer unlock()
	if err := e.folderAt(ctx, m, m.BumpSHA); err != nil {
		return localResult{hold: HeldFolderMoved, err: err}
	}
	if e.d.Gate == nil {
		return localResult{skip: "no verify gate available"}
	}
	res, ran := e.d.Gate(ctx, m.Project, m.Folder, m.Folder, filepath.Join(e.runDir(c.run.ID), "logs"))
	if !ran {
		return localResult{skip: "no verify gate configured (aegis or the project's verify command)"}
	}
	if !res.OK {
		return localResult{hold: HeldGate, err: fmt.Errorf("%s: exit %d: %s", res.Command, res.ExitCode, tail(res.Output, 2000))}
	}
	if err := e.folderAt(ctx, m, m.BumpSHA); err != nil {
		return localResult{hold: HeldFolderMoved, err: fmt.Errorf("after the gate: %w", err)}
	}
	res.Output = ""
	b, _ := json.Marshal(res)
	return localResult{ref: string(b)}
}

// --- remote ------------------------------------------------------------------

// remote is the push URL with the token environment; secret is redacted from errors.
func (e *Engine) remote(ctx context.Context, m *Manifest) (url string, env []string, secret string, err error) {
	url = e.d.GitURL(m.RepoURL)
	if e.d.GitToken == nil {
		return url, nil, "", nil
	}
	tok, err := e.d.GitToken(ctx)
	if err != nil {
		return "", nil, "", err
	}
	env, secret = e.d.TokenEnv(url, tok)
	return url, env, secret, nil
}

func redact(err error, secret string) error {
	if err == nil || secret == "" {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), secret, "***"))
}

// extStep is an external action: a read-only probe, an optional check before
// sending (refusals that leave nothing sent), the send and its idempotency key.
type extStep struct {
	probe func(ctx context.Context, c *rc, st store.Step) (probeResult, error)
	check func(ctx context.Context, c *rc, st store.Step) (hold string, err error)
	send  func(ctx context.Context, c *rc, st store.Step) (ref string, err error)
	idem  func(c *rc, st store.Step) string
}

func (e *Engine) ext(step string) extStep {
	switch step {
	case StepPush:
		return extStep{probe: e.probePush, check: e.checkPush, send: e.sendPush,
			idem: func(c *rc, _ store.Step) string { return c.m.Branch + ":" + c.m.BumpSHA }}
	case StepTag:
		return extStep{probe: e.probeTag, send: e.sendTag,
			idem: func(c *rc, _ store.Step) string { return c.m.Tag + ":" + c.m.BumpSHA }}
	case StepGHRelease:
		return extStep{probe: e.probeRelease, send: e.sendRelease,
			idem: func(c *rc, _ store.Step) string { return c.m.Tag + ":" + c.m.BumpSHA }}
	case StepGHAsset:
		return extStep{probe: e.probeAsset, check: e.checkArtifact, send: e.sendAsset,
			idem: func(c *rc, st store.Step) string { return c.m.Tag + ":" + c.m.BumpSHA + ":" + st.Target }}
	case StepPublish:
		return extStep{probe: e.probePublish, check: e.checkPublish, send: e.sendPublish,
			idem: func(c *rc, st store.Step) string { return st.Target + ":" + c.m.Version + ":" + artSHA(c) }}
	}
	return extStep{
		probe: func(context.Context, *rc, store.Step) (probeResult, error) {
			return probeResult{}, fmt.Errorf("unknown step %s", step)
		},
		send: func(context.Context, *rc, store.Step) (string, error) { return "", fmt.Errorf("unknown step %s", step) },
		idem: func(*rc, store.Step) string { return "" },
	}
}

func artSHA(c *rc) string {
	if c.art == nil {
		return ""
	}
	return c.art.SHA256
}

// --- push / tag --------------------------------------------------------------

// probePush: the bump sha is on the remote default branch.
func (e *Engine) probePush(ctx context.Context, c *rc, _ store.Step) (probeResult, error) {
	m := c.m
	if err := c.mir.ensure(ctx); err != nil {
		return probeResult{}, err
	}
	if _, err := c.mir.run(ctx, nil, "cat-file", "-e", m.BumpSHA+"^{commit}"); err != nil {
		if err := c.mir.fetchFolder(ctx, m.Folder, m.Branch, m.BumpSHA); err != nil {
			return probeResult{}, err
		}
	}
	url, env, secret, err := e.remote(ctx, m)
	if err != nil {
		return probeResult{}, err
	}
	head, err := c.mir.fetchRemote(ctx, url, env, m.Branch)
	if err != nil {
		return probeResult{}, redact(err, secret)
	}
	on, err := c.mir.isAncestor(ctx, m.BumpSHA, head)
	if err != nil {
		return probeResult{}, err
	}
	return probeResult{present: on, ref: head}, nil
}

// checkPush: fast-forward only, and the commits ahead of the remote must be
// the manifest's (Phase 1: the bump commit only).
func (e *Engine) checkPush(ctx context.Context, c *rc, _ store.Step) (string, error) {
	m := c.m
	head, err := c.mir.run(ctx, nil, "rev-parse", "refs/remotes/origin/"+m.Branch)
	if err != nil {
		return "check:push", err
	}
	if ff, err := c.mir.isAncestor(ctx, head, m.BumpSHA); err != nil || !ff {
		return HeldRemoteMoved, fmt.Errorf("the remote %s moved to %s: not a fast-forward of the release", m.Branch, short(head))
	}
	out, err := c.mir.run(ctx, nil, "rev-list", head+".."+m.BumpSHA)
	if err != nil {
		return "check:push", err
	}
	for sha := range strings.SplitSeq(out, "\n") {
		if sha = strings.TrimSpace(sha); sha != "" && sha != m.BumpSHA {
			return HeldForeignCommits, fmt.Errorf("commit %s ahead of the remote is not part of the release", short(sha))
		}
	}
	return "", nil
}

func (e *Engine) sendPush(ctx context.Context, c *rc, _ store.Step) (string, error) {
	m := c.m
	url, env, secret, err := e.remote(ctx, m)
	if err != nil {
		return "", holdError{reason: "auth:github", err: err, definite: true}
	}
	if _, err := c.mir.run(ctx, env, "push", "-q", "--no-verify", url, m.BumpSHA+":refs/heads/"+m.Branch); err != nil {
		return "", redact(err, secret)
	}
	return m.BumpSHA, nil
}

// probeTag: the remote tag (peeled) == bump sha; another sha → tag_conflict.
func (e *Engine) probeTag(ctx context.Context, c *rc, _ store.Step) (probeResult, error) {
	m := c.m
	url, env, secret, err := e.remote(ctx, m)
	if err != nil {
		return probeResult{}, err
	}
	if err := c.mir.ensure(ctx); err != nil {
		return probeResult{}, err
	}
	sha, err := c.mir.remoteTag(ctx, url, env, m.Tag)
	switch {
	case err != nil:
		return probeResult{}, redact(err, secret)
	case sha == "":
		return probeResult{}, nil
	case sha == m.BumpSHA:
		return probeResult{present: true, ref: sha}, nil
	}
	return probeResult{hold: HeldTagConflict}, nil
}

func (e *Engine) sendTag(ctx context.Context, c *rc, _ store.Step) (string, error) {
	m := c.m
	url, env, secret, err := e.remote(ctx, m)
	if err != nil {
		return "", holdError{reason: "auth:github", err: err, definite: true}
	}
	if cur, err := c.mir.run(ctx, nil, "rev-parse", "-q", "--verify", "refs/tags/"+m.Tag+"^{commit}"); err != nil || cur != m.BumpSHA {
		if err == nil { // a stale local tag of the mirror (never pushed: the probe saw none)
			_, _ = c.mir.run(ctx, nil, "tag", "-d", m.Tag)
		}
		if _, err := c.mir.run(ctx, nil, "-c", "user.name="+m.Identity[0], "-c", "user.email="+m.Identity[1], "-c", "tag.gpgSign=false",
			"tag", "-a", m.Tag, "-m", m.Tag, m.BumpSHA); err != nil {
			return "", err
		}
	}
	if _, err := c.mir.run(ctx, env, "push", "-q", "--no-verify", url, "refs/tags/"+m.Tag+":refs/tags/"+m.Tag); err != nil {
		return "", redact(err, secret)
	}
	return m.BumpSHA, nil
}

// --- GitHub release -----------------------------------------------------------

func (e *Engine) probeRelease(ctx context.Context, c *rc, _ store.Step) (probeResult, error) {
	if e.d.Releaser == nil {
		return probeResult{}, errors.New("no GitHub releaser")
	}
	rel, found, err := e.d.Releaser.GetReleaseByTag(ctx, c.m.Repo, c.m.Tag)
	if err != nil || !found {
		return probeResult{}, err
	}
	return probeResult{present: true, ref: rel.HTMLURL}, nil
}

func (e *Engine) sendRelease(ctx context.Context, c *rc, _ store.Step) (string, error) {
	m := c.m
	rel, err := e.d.Releaser.CreateRelease(ctx, m.Repo, provider.NewRelease{TagName: m.Tag, TargetCommitish: m.BumpSHA,
		Name: m.Tag, Body: m.Changelog})
	if err != nil {
		return "", err
	}
	return rel.HTMLURL, nil
}

// probeAsset: the asset is uploaded with the artifact's size (and digest when reported).
func (e *Engine) probeAsset(ctx context.Context, c *rc, st store.Step) (probeResult, error) {
	if e.d.Releaser == nil {
		return probeResult{}, errors.New("no GitHub releaser")
	}
	if c.art == nil {
		return probeResult{}, errors.New("no built artifact")
	}
	rel, found, err := e.d.Releaser.GetReleaseByTag(ctx, c.m.Repo, c.m.Tag)
	if err != nil {
		return probeResult{}, err
	}
	if !found {
		return probeResult{}, fmt.Errorf("release %s not found", c.m.Tag)
	}
	a, ok := rel.Asset(st.Target)
	if !ok {
		return probeResult{}, nil
	}
	if a.State != "uploaded" || a.Size != c.art.Size || (a.Digest != "" && !strings.EqualFold(a.Digest, "sha256:"+c.art.SHA256)) {
		return probeResult{}, fmt.Errorf("asset %s is present but not this archive (state %s, size %d, digest %s)", a.Name, a.State, a.Size, a.Digest)
	}
	return probeResult{present: true, ref: a.BrowserDownloadURL}, nil
}

// checkArtifact re-hashes the archive right before it is sent.
func (e *Engine) checkArtifact(_ context.Context, c *rc, _ store.Step) (string, error) {
	if c.art == nil {
		return "failed:build", errors.New("no built artifact")
	}
	sum, err := sha256File(c.art.Path)
	if err != nil {
		return HeldArtifact, err
	}
	if sum != c.art.SHA256 {
		return HeldArtifact, fmt.Errorf("%s changed since the build (sha256 %s, built %s)", c.art.Name, sum, c.art.SHA256)
	}
	return "", nil
}

func (e *Engine) sendAsset(ctx context.Context, c *rc, st store.Step) (string, error) {
	rel, found, err := e.d.Releaser.GetReleaseByTag(ctx, c.m.Repo, c.m.Tag)
	if err != nil {
		return "", err
	}
	if !found {
		return "", fmt.Errorf("release %s not found", c.m.Tag)
	}
	a, err := e.d.Releaser.UploadReleaseAsset(ctx, rel, st.Target, c.art.Path, "application/zip")
	if err != nil {
		return "", err
	}
	return a.BrowserDownloadURL, nil
}

// --- publish / available -----------------------------------------------------

func (c *rc) target(key string) (Target, bool) {
	for _, t := range c.m.Targets {
		if t.Key == key {
			return t, true
		}
	}
	return Target{}, false
}

// targetFiles narrows the platform's files to the target's file (fileId).
func targetFiles(pt provider.PublishTargets, t Target) []provider.PublishFile {
	if t.Profile.FileID == "" {
		return pt.Files
	}
	for _, f := range pt.Files {
		if f.ID == t.Profile.FileID {
			return []provider.PublishFile{f}
		}
	}
	return nil
}

// findVersion looks for version among the target's files.
func findVersion(pt provider.PublishTargets, t Target, version string) (provider.PublishFile, provider.PublishVersion, bool) {
	for _, f := range targetFiles(pt, t) {
		for _, v := range f.Versions {
			if eqVersion(v.Version, version) {
				return f, v, true
			}
		}
	}
	return provider.PublishFile{}, provider.PublishVersion{}, false
}

func eqVersion(a, b string) bool {
	c, err := source.Compare(a, b)
	if err != nil {
		return strings.TrimPrefix(a, "v") == strings.TrimPrefix(b, "v")
	}
	return c == 0
}

// latestVersion is the highest semver among the target's files ("" = none).
func latestVersion(pt provider.PublishTargets, t Target) string {
	best := ""
	for _, f := range targetFiles(pt, t) {
		for _, v := range f.Versions {
			if _, err := source.Parse(v.Version); err != nil {
				continue
			}
			if best == "" {
				best = v.Version
			} else if c, err := source.Compare(v.Version, best); err == nil && c > 0 {
				best = v.Version
			}
		}
	}
	return best
}

// publishProbe: version X.Y.Z is listed on the target (Factorio: with the archive's sha1).
func (e *Engine) publishProbe(ctx context.Context, c *rc, key string) (probeResult, error) {
	t, ok := c.target(key)
	if !ok {
		return probeResult{}, fmt.Errorf("target %s is not in the manifest", key)
	}
	pub := e.publisher(t.Platform)
	if pub == nil {
		return probeResult{}, fmt.Errorf("publishing is not available for %s", t.Platform)
	}
	pt, err := pub.PublishTargets(ctx, t.project())
	if err != nil {
		return probeResult{}, err
	}
	if fs := targetFiles(pt, t); len(fs) > 0 {
		c.files[key] = fs[0].Name
	}
	_, v, found := findVersion(pt, t, c.m.Version)
	if !found {
		return probeResult{}, nil
	}
	if v.SHA1 != "" && c.art != nil && !strings.EqualFold(v.SHA1, c.art.SHA1) {
		return probeResult{hold: HeldVersion}, nil // the same version with another archive
	}
	return probeResult{present: true, ref: cmpOrStr(v.ID, v.Version)}, nil
}

func (e *Engine) publisher(platform string) provider.Publisher {
	if e.d.Publishers == nil {
		return nil
	}
	return e.d.Publishers(platform)
}

func (e *Engine) probePublish(ctx context.Context, c *rc, st store.Step) (probeResult, error) {
	return e.publishProbe(ctx, c, st.Target)
}

// publishState is a publish step's request_json: the reusable upload of a failed publish.
type publishState struct {
	UploadID string `json:"uploadId,omitempty"`
}

var nameUnsafe = regexp.MustCompile(`[^A-Za-z0-9 _'().-]+`)

func (e *Engine) publishRequest(c *rc, st store.Step, t Target) provider.PublishRequest {
	m := c.m
	name := strings.TrimSpace(nameUnsafe.ReplaceAllString(cmpOrStr(c.files[t.Key], cmpOrStr(m.ModName, t.Name)), " "))
	if len(name) > 100 {
		name = name[:100]
	}
	req := provider.PublishRequest{Path: c.art.Path, Version: m.Version, Name: cmpOrStr(name, "release")}
	switch t.Platform {
	case "factorio":
	default:
		req.FileID, req.Category, req.ArchivePrevious = t.Profile.FileID, t.Profile.Category, t.Profile.ArchivePrevious
		if r := []rune(m.Changelog); len(r) > 2000 {
			req.Changelog = string(r[:2000])
		} else {
			req.Changelog = m.Changelog
		}
	}
	var ps publishState
	if json.Unmarshal(st.Request, &ps) == nil && ps.UploadID != "" && t.Platform != "factorio" {
		req.Path, req.UploadID = "", ps.UploadID // the archive is uploaded already
	}
	return req
}

func (e *Engine) checkPublish(ctx context.Context, c *rc, st store.Step) (string, error) {
	if hold, err := e.checkArtifact(ctx, c, st); hold != "" {
		return hold, err
	}
	t, ok := c.target(st.Target)
	if !ok {
		return "failed:" + stepName(st), errors.New("target not in the manifest")
	}
	pub := e.publisher(t.Platform)
	if pub == nil {
		return "auth:" + t.Platform, fmt.Errorf("publishing is not available for %s", t.Platform)
	}
	if err := pub.CheckPublish(t.project(), e.publishRequest(c, st, t)); err != nil {
		return "failed:" + stepName(st), err
	}
	return "", nil
}

func (e *Engine) sendPublish(ctx context.Context, c *rc, st store.Step) (string, error) {
	t, _ := c.target(st.Target)
	pub := e.publisher(t.Platform)
	res, err := pub.Publish(ctx, t.project(), e.publishRequest(c, st, t), nil)
	if err == nil {
		return cmpOrStr(res.VersionID, cmpOrStr(res.UploadID, c.m.Version)), nil
	}
	var pe *provider.PublishError
	if errors.As(err, &pe) && pe.UploadID != "" {
		b, _ := json.Marshal(publishState{UploadID: pe.UploadID})
		_, _ = e.transition(context.WithoutCancel(ctx), st, store.StepSending, store.StepSending, store.StepUpdate{Request: b})
	}
	if code := authState(err); code == "no_api_key" || code == "bad_api_key" {
		return "", holdError{reason: "auth:" + t.Platform, err: err, definite: true}
	}
	return "", err
}

// authState classifies a platform error for the plan (ok | no_api_key | bad_api_key | error).
func authState(err error) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, nexus.ErrNoAPIKey), errors.Is(err, factorio.ErrNoAPIKey):
		return "no_api_key"
	case errors.Is(err, nexus.ErrBadAPIKey), errors.Is(err, factorio.ErrBadAPIKey):
		return "bad_api_key"
	}
	return "error"
}

// available polls until the published version is listed (downloadable), up
// to AvailableFor after the step started.
func (e *Engine) available(ctx context.Context, c *rc, st store.Step) localResult {
	start, err := time.Parse(time.RFC3339Nano, st.StartedAt)
	if err != nil {
		start = e.d.Now()
	}
	deadline := start.Add(e.d.AvailableFor)
	for {
		pr, err := e.publishProbe(ctx, c, st.Target)
		switch {
		case err != nil:
			return localResult{hold: "check:" + stepName(st), err: err}
		case pr.hold != "":
			return localResult{hold: pr.hold}
		case pr.present:
			return localResult{ref: pr.ref}
		}
		if e.d.Now().After(deadline) {
			return localResult{hold: HeldAvailability, err: fmt.Errorf("%s not listed after %s", c.m.Version, e.d.AvailableFor)}
		}
		select {
		case <-ctx.Done():
			return localResult{hold: "check:" + stepName(st), err: ctx.Err()}
		case <-time.After(e.d.PollEvery):
		}
		if e.stopping(c.run.ID) {
			return localResult{hold: "check:" + stepName(st), err: errors.New("stopped")}
		}
	}
}
