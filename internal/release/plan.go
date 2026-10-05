package release

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/config"
	"github.com/UberMorgott/issuewatcher/internal/release/source"
	"github.com/UberMorgott/issuewatcher/internal/smoke"
	"github.com/UberMorgott/issuewatcher/internal/store"
)

// RefusedError is a start refused by the plan (the API answers 409 with the plan).
type RefusedError struct{ Plan Plan }

func (e *RefusedError) Error() string {
	if len(e.Plan.Refusals) == 0 {
		return "release: refused"
	}
	return "release: refused: " + e.Plan.Refusals[0].Code + ": " + e.Plan.Refusals[0].Message
}

// Plan resolves a release of code project projectID without changing
// anything (no file, no remote write; it fetches the remote branch into the
// mirror): version, changelog preview, targets with their latest versions and
// auth state, the planned steps, and every refusal.
func (e *Engine) Plan(ctx context.Context, projectID int64, req Request) (Plan, error) {
	p := Plan{ProjectID: projectID, Refusals: []Refusal{}, Targets: []PlanTarget{}, Steps: []PlanStep{},
		WritesVersion: []string{}, WritesLog: []string{}}
	refuse := func(code, format string, a ...any) {
		p.Refusals = append(p.Refusals, Refusal{Code: code, Message: fmt.Sprintf(format, a...)})
	}
	repo, err := e.d.Store.Repo(ctx, projectID)
	if err != nil {
		return p, err
	}
	p.Project = repo.Key
	cfg := e.d.Settings()
	pa := cfg.Agents.Projects[repo.Key]
	ap := cfg.Agents.AutopilotFor(repo.Key)
	prof := pa.PublishProfile
	p.Paused, p.Enabled, p.GithubRelease, p.SmokeKind = cfg.Agents.Autopilot.Paused, ap.Enabled, ap.GithubRelease, prof.Smoke.Kind
	// The owner's click (manual) is the owner's decision; an MCP / CLI release is
	// never owner approval, so the kill switch and the project switch hold it.
	owner := req.Origin == "" || req.Origin == store.RunOriginManual
	if p.Paused && !owner {
		refuse(CodePaused, "autopilot is paused (agents.autopilot.paused)")
	}
	if !ap.Enabled && !owner {
		refuse(CodeDisabled, "autopilot is off for %s", repo.Key)
	}
	if repo.Platform != "github" {
		refuse(CodeNoFolder, "%s is not a GitHub code project", repo.Key)
		return p, nil
	}
	p.Folder = repo.LocalPath
	if repo.LocalPath == "" {
		refuse(CodeNoFolder, "no local folder is mapped to %s", repo.Key)
		return p, nil
	}
	if !exists(filepath.Join(repo.LocalPath, ".git")) {
		refuse(CodeNoFolder, "%s is not a git clone", repo.LocalPath)
		return p, nil
	}
	b := prof.Build
	if (b.Command == "" || b.Output == "") && b.Path == "" {
		refuse(CodeNoProfile, "publishProfile.build: set command + output (or path)")
	}
	if prof.Version.Kind == "" {
		refuse(CodeNoProfile, "publishProfile.version.kind is not set")
	}
	if prof.Changelog.Kind == "" {
		refuse(CodeNoProfile, "publishProfile.changelog.kind is not set")
	}
	repoName := strings.TrimPrefix(repo.Key, "github:")

	// Targets: linked mod pages with a publisher.
	targets, err := e.planTargets(ctx, repo, ap, prof, req, refuse)
	if err != nil {
		return p, err
	}
	p.Targets = targets
	var selected []Target
	for _, t := range targets {
		if !t.Selected {
			continue
		}
		mp, err := e.d.Store.Repo(ctx, t.ProjectID)
		if err != nil {
			return p, err
		}
		selected = append(selected, Target{Key: t.Key, ProjectID: t.ProjectID, Platform: t.Platform,
			ExternalID: strings.TrimPrefix(t.Key, t.Platform+":"), Name: mp.Name, URL: mp.URL, Profile: prof.Targets[t.Key]})
	}
	if len(selected) == 0 {
		refuse(CodeNoTargets, "no enabled and configured publish target (autopilot.publish + publishProfile.targets of a linked mod page)")
	}

	// Git state of the folder and the remote.
	m := &Manifest{ProjectID: projectID, Project: repo.Key, Repo: repoName, RepoURL: repo.URL, Folder: repo.LocalPath,
		GithubRelease: ap.GithubRelease, Profile: prof, Targets: selected, Items: req.Items}
	if m.Items == nil {
		m.Items = []int64{}
	}
	branch := ""
	if e.d.DefaultBranch != nil {
		branch, _ = e.d.DefaultBranch(ctx, repoName)
	}
	if branch == "" {
		if h, err := e.folderGit(ctx, m, "symbolic-ref", "--short", "refs/remotes/origin/HEAD"); err == nil {
			branch = strings.TrimPrefix(h, "origin/")
		}
	}
	if branch == "" {
		branch = "main"
	}
	m.Branch, p.Branch = branch, branch
	cur, _ := e.folderGit(ctx, m, "rev-parse", "--abbrev-ref", "HEAD")
	if cur != branch {
		refuse(CodeNotDefaultBranch, "the folder is on %q, the default branch is %s", cur, branch)
	}
	head, err := e.folderGit(ctx, m, "rev-parse", "HEAD")
	if err != nil {
		refuse(CodeNoFolder, "%v", err)
		return p, nil
	}
	m.Head, p.Head = head, head
	if st, err := e.folderGit(ctx, m, "status", "--porcelain"); err != nil || st != "" {
		refuse(CodeDirtyFolder, "the folder has uncommitted changes: commit or remove them first")
	}
	mir := newMirror(e.d.Git, e.d.DataDir, repoName)
	remoteHead := ""
	if err := mir.ensure(ctx); err != nil {
		return p, err
	}
	url, env, secret, err := e.remote(ctx, m)
	if err == nil {
		remoteHead, err = mir.fetchRemote(ctx, url, env, branch)
		err = redact(err, secret)
	}
	if err != nil {
		refuse(CodeRemote, "cannot read %s of the remote: %v", branch, err)
	}
	p.RemoteHead = remoteHead
	if remoteHead != "" && head != remoteHead {
		refuse(CodeHeadNotRemote, "the folder's HEAD %s is not the remote %s head %s (push or pull first)", short(head), branch, short(remoteHead))
	}
	if req.Head != "" && !strings.HasPrefix(head, strings.ToLower(req.Head)) {
		refuse(CodeHeadNotRemote, "head %s is not the folder's HEAD %s", req.Head, short(head))
	}

	// Version.
	vs := source.VersionSource(prof.Version)
	current := ""
	if prof.Version.Kind != "" {
		//nolint:contextcheck // source reads git tags with its own timeout
		if current, err = source.CurrentVersion(repo.LocalPath, vs); err != nil && !errors.Is(err, source.ErrNoVersion) {
			refuse(CodeNoProfile, "version source: %v", err)
		}
	}
	p.CurrentVersion = current
	version := strings.TrimPrefix(strings.TrimSpace(req.Version), "v")
	switch {
	case version != "":
		if _, err := source.Parse(version); err != nil {
			refuse(CodeBadRequest, "version %q: %v", req.Version, err)
			version = ""
		}
	case current != "":
		version, _ = source.NextPatch(current)
	case prof.Version.Kind != "":
		refuse(CodeBadRequest, "the version source has no version yet: pass an explicit version")
	}
	m.Version, m.FromVersion, m.Tag = version, current, "v"+version
	p.Version, p.Tag = version, m.Tag
	if version != "" {
		tagLocal := e.gitOK(ctx, repo.LocalPath, "rev-parse", "-q", "--verify", "refs/tags/"+m.Tag)
		tagRemote := ""
		if url != "" && remoteHead != "" {
			tagRemote, _ = mir.remoteTag(ctx, url, env, m.Tag)
		}
		if tagLocal || tagRemote != "" {
			refuse(CodeTagExists, "tag %s already exists (tags are never moved or reused)", m.Tag)
		}
		if current != "" {
			c, _ := source.Compare(version, current)
			if c < 0 || (c == 0 && (req.Version == "" || tagLocal || tagRemote != "")) {
				refuse(CodeVersionConflict, "version %s is not above the source's %s", version, current)
			}
		}
		for _, t := range p.Targets {
			if t.Selected && t.LatestVersion != "" {
				if c, err := source.Compare(version, t.LatestVersion); err == nil && c <= 0 {
					refuse(CodeVersionConflict, "%s already has %s (the release is %s)", t.Key, t.LatestVersion, version)
				}
			}
		}
	}

	// Base tag, changelog preview, names.
	if bt, err := e.folderGit(ctx, m, "describe", "--tags", "--abbrev=0", "--match", "v*", "HEAD"); err == nil {
		m.BaseTag = bt
	}
	p.BaseTag = m.BaseTag
	if version != "" && prof.Changelog.Kind != "" && prof.Version.Kind != "" {
		text, vfiles, cfiles, err := e.changelogPreview(repo.LocalPath, prof, version, e.subjects(ctx, repo.LocalPath, m.BaseTag, head))
		switch {
		case errors.Is(err, source.ErrNoChanges):
			refuse(CodeBadRequest, "no changelog entry for %s and no commits since %s to write one from", version, cmpOrStr(m.BaseTag, "the start"))
		case err != nil:
			refuse(CodeNoProfile, "changelog / version source: %v", err)
		}
		p.Changelog, m.Changelog = text, ""
		p.WritesVersion, p.WritesLog = nonNil(vfiles), nonNil(cfiles)
	}
	m.ModName = modName(repo.LocalPath, prof, repoName)
	switch {
	case b.Path != "":
		m.Asset = filepath.Base(b.Path)
	case b.Output != "":
		m.Asset = path.Base(filepath.ToSlash(expandOutput(b.Output, m)))
	}
	p.Asset = m.Asset
	m.Identity = e.identity(ctx, m)

	// Items.
	for _, id := range req.Items {
		in, err := e.d.Store.JobInput(ctx, id)
		if err != nil || in.CodeKey != repo.Key {
			refuse(CodeBadRequest, "item %d is not an item of %s or its mod pages", id, repo.Key)
		}
	}

	// Serialization and caps.
	e.planCaps(ctx, &p, projectID, cfg, ap, len(selected), refuse)
	p.Steps = planSteps(m)
	p.OK = len(p.Refusals) == 0
	p.manifest = m // Release uses it only when OK; Check builds from it regardless
	return p, nil
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func (e *Engine) gitOK(ctx context.Context, dir string, args ...string) bool {
	_, err := e.d.Git.Run(ctx, dir, nil, args...)
	return err == nil
}

// identity is the folder's git identity (committer of the bump, tagger).
func (e *Engine) identity(ctx context.Context, m *Manifest) [2]string {
	name, _ := e.folderGit(ctx, m, "config", "user.name")
	email, _ := e.folderGit(ctx, m, "config", "user.email")
	return [2]string{cmpOrStr(name, "IssueWatcher"), cmpOrStr(email, "issuewatcher@users.noreply.github.com")}
}

// modName is {name} of build.output: info.json's name for factorio-info, else the repo name.
func modName(dir string, p config.PublishProfile, repo string) string {
	if p.Version.Kind == source.KindFactorioInfo {
		b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(cmpOrStr(p.Version.Path, source.DefaultInfoJSON)))) //nolint:gosec // G304: the profile's version file in the mapped folder
		if err == nil {
			var v struct {
				Name string `json:"name"`
			}
			if json.Unmarshal(b, &v) == nil && v.Name != "" {
				return v.Name
			}
		}
	}
	_, name, _ := strings.Cut(repo, "/")
	return name
}

// planTargets lists the code project's linked mod pages that can publish,
// with their latest version and auth state.
func (e *Engine) planTargets(ctx context.Context, repo store.Repo, ap config.ProjectAutopilot, prof config.PublishProfile,
	req Request, refuse func(code, format string, a ...any),
) ([]PlanTarget, error) {
	out := []PlanTarget{}
	known := map[string]bool{}
	for _, id := range repo.Links {
		mp, err := e.d.Store.Repo(ctx, id)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				continue
			}
			return nil, err
		}
		tp, configured := prof.Targets[mp.Key]
		if mp.Platform == "nexus" && tp.FileID == "" {
			configured = false // Nexus needs the file group to add the version to
		}
		pt := PlanTarget{Key: mp.Key, ProjectID: mp.ID, Platform: mp.Platform, Name: mp.Name, URL: mp.URL,
			Enabled: ap.Publish[mp.Key], Configured: configured, Auth: "unavailable"}
		known[mp.Key] = true
		pub := e.publisher(mp.Platform)
		pt.Publishable = pub != nil
		pt.Selected = pt.Enabled && pt.Configured && pt.Publishable && (len(req.Targets) == 0 || slices.Contains(req.Targets, mp.Key))
		if pub != nil {
			t := Target{Key: mp.Key, Platform: mp.Platform, ExternalID: strings.TrimPrefix(mp.Key, mp.Platform+":"), Name: mp.Name,
				URL: mp.URL, Profile: tp}
			tctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			list, err := pub.PublishTargets(tctx, t.project())
			cancel()
			pt.Auth = authState(err)
			if err != nil {
				pt.Error = err.Error()
				if pt.Selected {
					refuse(CodeRemote, "%s: %v", mp.Key, err)
				}
			} else {
				pt.LatestVersion = latestVersion(list, t)
				if tp.FileID != "" && len(targetFiles(list, t)) == 0 && pt.Selected {
					refuse(CodeNoProfile, "%s: file %s is not on the mod page", mp.Key, tp.FileID)
				}
			}
		}
		out = append(out, pt)
	}
	for _, k := range req.Targets {
		if !known[k] {
			refuse(CodeBadRequest, "target %s is not a linked mod page of %s", k, repo.Key)
		}
	}
	return out, nil
}

// planCaps fills the daily counters and refuses busy / over-cap starts.
func (e *Engine) planCaps(ctx context.Context, p *Plan, projectID int64, cfg config.Settings, ap config.ProjectAutopilot, publishes int,
	refuse func(code, format string, a ...any),
) {
	g := cfg.Agents.Autopilot
	p.Caps = Caps{ProjectMaxReleases: ap.MaxReleasesPerDay, MaxReleases: g.MaxReleasesPerDay, MaxPublishes: g.MaxPublishesPerDay}
	runs, err := e.d.Store.Runs(ctx, store.RunFilter{Kind: store.RunKindRelease, Limit: 500})
	if err != nil {
		return
	}
	day := e.d.Now().UTC().Truncate(24 * time.Hour)
	for _, r := range runs {
		if r.ProjectID == projectID && (r.State == store.RunPending || r.State == store.RunRunning || r.State == store.RunHeld) {
			refuse(CodeBusy, "release run %d of this project is not finished (%s)", r.ID, r.State)
		}
		at, err := time.Parse(time.RFC3339Nano, r.CreatedAt)
		if err != nil || at.Before(day) {
			continue
		}
		p.Caps.ReleasesToday++
		if r.ProjectID == projectID {
			p.Caps.ProjectReleasesToday++
		}
		if steps, err := e.d.Store.RunSteps(ctx, r.ID); err == nil {
			for _, st := range steps {
				if st.Step == StepPublish && st.State != store.StepSkipped {
					p.Caps.PublishesToday++
				}
			}
		}
	}
	switch {
	case ap.MaxReleasesPerDay > 0 && p.Caps.ProjectReleasesToday >= ap.MaxReleasesPerDay:
		refuse(CodeCapReached, "%d of %d releases of this project today (UTC)", p.Caps.ProjectReleasesToday, ap.MaxReleasesPerDay)
	case g.MaxReleasesPerDay > 0 && p.Caps.ReleasesToday >= g.MaxReleasesPerDay:
		refuse(CodeCapReached, "%d of %d releases today (UTC)", p.Caps.ReleasesToday, g.MaxReleasesPerDay)
	case g.MaxPublishesPerDay > 0 && p.Caps.PublishesToday+publishes > g.MaxPublishesPerDay:
		refuse(CodeCapReached, "%d of %d publishes today (UTC), this release adds %d", p.Caps.PublishesToday, g.MaxPublishesPerDay, publishes)
	}
}

// smokeStep is the smoke test as the plan shows it.
func smokeStep(s config.SmokeProfile) PlanStep {
	switch s.Kind {
	case config.SmokeFactorio:
		save := "a fresh map"
		if s.Save != "" {
			save = s.Save
		}
		ticks := s.Ticks
		if ticks <= 0 {
			ticks = smoke.DefaultTicks
		}
		install := "auto-detected install"
		if s.Install != "" {
			install = s.Install
		}
		return PlanStep{Step: StepSmoke, Request: fmt.Sprintf("factorio --create + --benchmark %s for %d ticks with the archive in a temp mod directory (%s)", save, ticks, install)}
	case config.SmokeCommand:
		return PlanStep{Step: StepSmoke, Request: "run in a temp dir: " + s.Command}
	}
	return PlanStep{Step: StepSmoke, Request: "no smoke test", Note: "held unless «Публиковать без смоук-теста» is on"}
}

// planSteps lists the run's steps in order (the NewStep list and the plan's view).
func planSteps(m *Manifest) []PlanStep {
	st := []PlanStep{
		{Step: StepBump, Request: "commit " + bumpPrefix + m.Tag + " in " + m.Folder + " (version + changelog; hooks off)"},
		{Step: StepBuild, Request: buildRequest(m)},
		{Step: StepArchiveCheck, Request: "open " + m.Asset + ": layout, version " + m.Version + ", size vs the previous release"},
		{Step: StepGate, Request: "verify gate (aegis verify / project verify command) in " + m.Folder + " at the bump commit"},
		smokeStep(m.Profile.Smoke),
		{Step: StepPush, IdemKey: m.Branch + ":<bump sha>", Request: "git push <repo>.git <bump sha>:refs/heads/" + m.Branch + " (from the app mirror, fast-forward only)"},
		{Step: StepTag, IdemKey: m.Tag + ":<bump sha>", Request: "git push <repo>.git refs/tags/" + m.Tag + " (annotated, on the bump sha)"},
	}
	if m.GithubRelease {
		st = append(st,
			PlanStep{Step: StepGHRelease, IdemKey: m.Tag + ":<bump sha>", Request: "POST /repos/" + m.Repo + "/releases {tag_name: " + m.Tag + ", target_commitish: <bump sha>, body: changelog}"},
			PlanStep{Step: StepGHAsset, Target: m.Asset, IdemKey: m.Tag + ":<bump sha>:" + m.Asset, Request: "POST uploads.github.com/repos/" + m.Repo + "/releases/<id>/assets?name=" + m.Asset})
	}
	for _, t := range m.Targets {
		st = append(st, PlanStep{Step: StepPublish, Target: t.Key, IdemKey: t.Key + ":" + m.Version + ":<sha256>",
			Request: fmt.Sprintf("publish %s %s to %s%s", m.Asset, m.Version, t.Key, fileNote(t))})
	}
	for _, t := range m.Targets {
		st = append(st, PlanStep{Step: StepAvailable, Target: t.Key, Request: "poll " + t.Key + " until " + m.Version + " is listed (max 72 h)"})
	}
	return st
}

func fileNote(t Target) string {
	if t.Profile.FileID != "" {
		return " (file " + t.Profile.FileID + ")"
	}
	return ""
}

func buildRequest(m *Manifest) string {
	b := m.Profile.Build
	if b.Path != "" {
		return "copy the ready archive " + b.Path
	}
	return "in data\\release\\<run>\\src (clean worktree of the bump sha): " + b.Command + " → " + expandOutput(b.Output, m)
}

// Release plans a release and, when nothing refuses it, creates the run with
// its frozen manifest (caps reserved in the same transaction) and starts it.
// req.DryRun only plans. A refusal → *RefusedError (with the plan).
func (e *Engine) Release(ctx context.Context, projectID int64, req Request) (store.Run, Plan, error) {
	p, err := e.Plan(ctx, projectID, req)
	if err != nil {
		return store.Run{}, p, err
	}
	if !p.OK {
		return store.Run{}, p, &RefusedError{Plan: p}
	}
	if req.DryRun {
		return store.Run{}, p, nil
	}
	m := p.manifest
	b, err := json.Marshal(m)
	if err != nil {
		return store.Run{}, p, err
	}
	var steps []store.NewStep
	for _, s := range p.Steps {
		steps = append(steps, store.NewStep{Step: s.Step, Target: s.Target})
	}
	items := make([]store.RunItem, 0, len(m.Items))
	for _, id := range m.Items {
		items = append(items, store.RunItem{ItemID: id, Role: store.RunItemPrimary})
	}
	cfg := e.d.Settings()
	origin := cmpOrStr(req.Origin, store.RunOriginManual)
	run, err := e.d.Store.CreateReleaseRun(ctx, store.NewReleaseRun{ProjectID: projectID, Origin: origin, Version: m.Version,
		Manifest: b, Items: items, Steps: steps, MaxPerProjectPerDay: cfg.Agents.AutopilotFor(m.Project).MaxReleasesPerDay,
		MaxGlobalPerDay: cfg.Agents.Autopilot.MaxReleasesPerDay, Now: e.d.Now()})
	switch {
	case errors.Is(err, store.ErrRunBusy):
		p.OK, p.Refusals = false, append(p.Refusals, Refusal{Code: CodeBusy, Message: "an unfinished release run of this project exists"})
		return store.Run{}, p, &RefusedError{Plan: p}
	case errors.Is(err, store.ErrCapReached):
		p.OK, p.Refusals = false, append(p.Refusals, Refusal{Code: CodeCapReached, Message: err.Error()})
		return store.Run{}, p, &RefusedError{Plan: p}
	case err != nil:
		return store.Run{}, p, err
	}
	e.d.OnChange(run.ID)
	e.launch(run.ID) //nolint:contextcheck // the run outlives the request: the engine's context
	return run, p, nil
}
