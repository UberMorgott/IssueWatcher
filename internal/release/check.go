package release

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/UberMorgott/issuewatcher/internal/config"
	"github.com/UberMorgott/issuewatcher/internal/provider"
	"github.com/UberMorgott/issuewatcher/internal/release/source"
	"github.com/UberMorgott/issuewatcher/internal/smoke"
	"github.com/UberMorgott/issuewatcher/internal/store"
)

// CheckResult is the full dry run («Проверить настройку»): the plan plus the
// archive built at the folder's HEAD in a temporary worktree, its archive
// check and each target's publisher dry-run plan on it. Nothing is committed
// in the mapped folder and nothing is sent.
type CheckResult struct {
	Plan
	Build        CheckBuild   `json:"build"`
	ArchiveCheck CheckArchive `json:"archiveCheck"`
	Smoke        CheckSmoke   `json:"smoke"`
	TargetPlans  []TargetPlan `json:"targetPlans"`
}

// CheckSmoke is the smoke test of the trial build.
type CheckSmoke struct {
	OK      bool          `json:"ok"`
	Skipped string        `json:"skipped,omitempty"`
	Code    string        `json:"code,omitempty"` // smoke_failed | smoke_missing | smoke_unavailable
	Error   string        `json:"error,omitempty"`
	Result  *smoke.Result `json:"result,omitempty"`
}

// CheckBuild is the trial build.
type CheckBuild struct {
	OK       bool       `json:"ok"`
	Skipped  string     `json:"skipped,omitempty"` // why no build was tried
	Head     string     `json:"head,omitempty"`    // the commit built
	Writes   []string   `json:"writes"`            // version / changelog files written in the temporary worktree
	Command  *CmdResult `json:"command,omitempty"` // build command (output tail)
	Artifact *Artifact  `json:"artifact,omitempty"`
	Error    string     `json:"error,omitempty"`
}

// CheckArchive is the archive check of the trial build.
type CheckArchive struct {
	OK      bool   `json:"ok"`
	Skipped string `json:"skipped,omitempty"`
	Note    string `json:"note,omitempty"`
	Error   string `json:"error,omitempty"`
}

// TargetPlan is a target's publisher dry run on the trial archive.
type TargetPlan struct {
	Key   string                 `json:"key"`
	OK    bool                   `json:"ok"`
	Plan  []provider.PublishStep `json:"plan"`
	Error string                 `json:"error,omitempty"`
	Code  string                 `json:"code,omitempty"` // bad_request | no_api_key | bad_api_key | platform_error | unavailable
}

// Check runs the full dry run of a release of projectID (see CheckResult).
func (e *Engine) Check(ctx context.Context, projectID int64, req Request) (CheckResult, error) {
	p, err := e.Plan(ctx, projectID, req)
	res := CheckResult{Plan: p, TargetPlans: []TargetPlan{}, Build: CheckBuild{Writes: []string{}}}
	if err != nil {
		return res, err
	}
	m := p.manifest
	b := CheckBuild{Writes: []string{}}
	switch {
	case m == nil || m.Head == "":
		b.Skipped = "no git folder to build from"
	case m.Version == "":
		b.Skipped = "no version to build"
	case m.Profile.Build.Path == "" && (m.Profile.Build.Command == "" || m.Profile.Build.Output == ""):
		b.Skipped = "no build command / archive path in the publish profile"
	case m.Profile.Build.Command != "" && e.d.Command == nil:
		b.Skipped = "builds are not available"
	}
	if b.Skipped != "" {
		res.Build, res.ArchiveCheck.Skipped, res.Smoke.Skipped = b, "no archive", "no archive"
		return res, nil
	}
	dir := filepath.Join(e.d.DataDir, "release", "check-"+randHex(6))
	defer func() { _ = os.RemoveAll(dir) }()
	art, b := e.trialBuild(ctx, m, dir)
	res.Build = b
	if art == nil {
		res.ArchiveCheck.Skipped, res.Smoke.Skipped = "no archive", "no archive"
		return res, nil
	}
	if note, err := e.checkArchive(ctx, *art, m, projectID); err != nil {
		res.ArchiveCheck.Error = err.Error()
	} else {
		res.ArchiveCheck.OK, res.ArchiveCheck.Note = true, note
	}
	c := &rc{m: m, art: art, files: map[string]string{}}
	res.Smoke = e.trialSmoke(ctx, c, filepath.Join(dir, "smoke"), filepath.Join(dir, "logs"))
	for _, t := range m.Targets {
		res.TargetPlans = append(res.TargetPlans, e.targetDryRun(ctx, c, t))
	}
	return res, nil
}

// trialBuild checks HEAD of the mapped folder out of the mirror into
// dir\src, writes the version and changelog there only, builds and copies
// the archive to dir\artifact.
func (e *Engine) trialBuild(ctx context.Context, m *Manifest, dir string) (*Artifact, CheckBuild) {
	b := CheckBuild{Head: m.Head, Writes: []string{}}
	fail := func(err error) (*Artifact, CheckBuild) {
		b.Error = err.Error()
		return nil, b
	}
	mir := newMirror(e.d.Git, e.d.DataDir, m.Repo)
	if err := mir.ensure(ctx); err != nil {
		return fail(err)
	}
	if _, err := mir.run(ctx, nil, "-c", "transfer.fsckObjects=true", "fetch", "-q", "--no-tags", m.Folder, "+HEAD:refs/iw/check"); err != nil {
		return fail(err)
	}
	wt := filepath.Join(dir, "src")
	if _, err := mir.run(ctx, nil, "worktree", "add", "-q", "--detach", "--force", wt, m.Head); err != nil {
		return fail(err)
	}
	defer e.removeWorktree(context.WithoutCancel(ctx), mir, wt)

	vs, cs := source.VersionSource(m.Profile.Version), source.ChangelogSource(m.Profile.Changelog)
	if vs.Kind != source.KindGitTag {
		ch, err := source.WriteVersion(wt, vs, m.Version)
		if err != nil {
			return fail(fmt.Errorf("version: %w", err))
		}
		b.Writes = append(b.Writes, ch...)
	}
	_, ch, err := source.Entry(wt, cs, m.Version, e.subjects(ctx, m.Folder, m.BaseTag, m.Head), e.d.Now())
	if err != nil && !errors.Is(err, source.ErrNoChanges) {
		return fail(fmt.Errorf("changelog: %w", err))
	}
	b.Writes = append(b.Writes, ch...)

	p := m.Profile.Build
	src := p.Path
	if p.Path != "" {
		if !filepath.IsAbs(src) {
			src = filepath.Join(m.Folder, filepath.FromSlash(src))
		}
	} else {
		cmd := e.d.Command(ctx, wt, p.Command, filepath.Join(dir, "logs"), buildTimeout)
		cmd.Output = tail(cmd.Output, 4000)
		b.Command = &cmd
		if !cmd.OK {
			return fail(fmt.Errorf("build exit %d", cmd.ExitCode))
		}
		out := filepath.ToSlash(filepath.Clean(expandOutput(p.Output, m)))
		src = filepath.Join(wt, filepath.FromSlash(out))
		status, err := mir.git.Run(ctx, wt, mir.env(), "status", "--porcelain", "--untracked-files=all")
		if err != nil {
			return fail(err)
		}
		for line := range strings.SplitSeq(status, "\n") {
			if len(line) < 4 {
				continue
			}
			path := strings.Trim(line[3:], `"`)
			if path != out && !slices.Contains(b.Writes, path) {
				return fail(fmt.Errorf("%s: the build changed %s besides %s", HeldDirtyBuild, path, out))
			}
		}
	}
	art, err := copyArtifact(src, filepath.Join(dir, "artifact", m.Asset))
	if err != nil {
		return fail(err)
	}
	b.OK, b.Artifact = true, &art
	return &art, b
}

// trialSmoke runs the smoke test on the trial archive (as the smoke step would).
func (e *Engine) trialSmoke(ctx context.Context, c *rc, work, logs string) CheckSmoke {
	prof := c.m.Profile.Smoke
	if prof.Kind == "" || prof.Kind == config.SmokeNone {
		if e.d.Settings != nil && e.d.Settings().Agents.AutopilotFor(c.m.Project).PublishWithoutSmoke {
			return CheckSmoke{OK: true, Skipped: "no smoke test (publishing without a smoke test is on)"}
		}
		return CheckSmoke{Code: HeldSmokeMissing, Error: "no smoke test in the publish profile and «Публиковать без смоук-теста» is off: a release would be held"}
	}
	if e.d.Smoke == nil {
		return CheckSmoke{Code: HeldSmokeUnavailable, Error: "smoke tests are not available"}
	}
	defer func() { _ = os.RemoveAll(work) }()
	res, err := e.d.Smoke(ctx, smoke.Request{Profile: prof, Archive: c.art.Path, Name: c.m.ModName, Version: c.m.Version,
		WorkDir: work, LogDir: logs})
	out := CheckSmoke{OK: err == nil && res.OK, Result: &res}
	switch {
	case errors.Is(err, smoke.ErrUnavailable):
		out.Code, out.Error = HeldSmokeUnavailable, err.Error()
	case err != nil:
		out.Code, out.Error = HeldSmokeFailed, err.Error()
	case !res.OK:
		out.Code, out.Error = HeldSmokeFailed, res.Summary()
	}
	return out
}

// targetDryRun asks the target's publisher for its plan on the trial archive.
func (e *Engine) targetDryRun(ctx context.Context, c *rc, t Target) TargetPlan {
	tp := TargetPlan{Key: t.Key, Plan: []provider.PublishStep{}}
	pub := e.publisher(t.Platform)
	if pub == nil {
		tp.Code, tp.Error = "unavailable", "publishing is not available for "+t.Platform
		return tp
	}
	_, _ = e.publishProbe(ctx, c, t.Key) // the target file's name for the request
	req := e.publishRequest(c, store.Step{}, t)
	req.DryRun = true
	if err := pub.CheckPublish(t.project(), req); err != nil {
		tp.Code, tp.Error = "bad_request", err.Error()
		return tp
	}
	res, err := pub.Publish(ctx, t.project(), req, nil)
	if err != nil {
		tp.Code, tp.Error = authState(err), err.Error()
		if tp.Code == "error" {
			tp.Code = "platform_error"
		}
		return tp
	}
	tp.OK = true
	if res.Plan != nil {
		tp.Plan = res.Plan
	}
	return tp
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
