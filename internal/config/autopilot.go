package config

import (
	"encoding/json"
	"fmt"
	"maps"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
)

// Autopilot: unattended fix → verify → release → publish → reply
// (docs/AUTOPILOT.md → Config schema). Everything is off by default.

// AgentsAutopilot is the global autopilot block (agents.autopilot).
type AgentsAutopilot struct {
	// Paused is the global kill switch: no autopilot run starts while it is set.
	Paused bool `json:"paused"`
	// MaxReleasesPerDay caps release runs across all projects in 24 h.
	MaxReleasesPerDay int `json:"maxReleasesPerDay"`
	// MaxPublishesPerDay caps publish steps across all targets in 24 h.
	MaxPublishesPerDay int `json:"maxPublishesPerDay"`
}

func defaultAgentsAutopilot() AgentsAutopilot {
	return AgentsAutopilot{MaxReleasesPerDay: 5, MaxPublishesPerDay: 20}
}

// ProjectAutopilot is a code project's autopilot block (agents.projects.<key>.autopilot).
type ProjectAutopilot struct {
	Enabled     bool `json:"enabled"`     // master switch for the project
	AutoTriage  bool `json:"autoTriage"`  // classify incoming items before acting
	AutoFix     bool `json:"autoFix"`     // implies automation allowAutoFix for this project
	AutoPush    bool `json:"autoPush"`    // push verified fixes to the default branch
	AutoRelease bool `json:"autoRelease"` // start release runs by the coalescing timer
	// GithubRelease makes a GitHub release for the tag (off = tag only).
	GithubRelease bool `json:"githubRelease"`
	// Publish enables publishing per linked mod page (platform:external_id → on).
	Publish   map[string]bool `json:"publish,omitempty"`
	AutoReply bool            `json:"autoReply"` // post reporter replies without a click
	AutoClose bool            `json:"autoClose"` // close GitHub issues after the reply
	// CoalesceMinutes: wait this long after the last pushed fix …
	CoalesceMinutes int `json:"coalesceMinutes"`
	// MaxBatchAgeHours: … but release at most this long after the oldest unreleased fix.
	MaxBatchAgeHours    int  `json:"maxBatchAgeHours"`
	MaxReleasesPerDay   int  `json:"maxReleasesPerDay"`
	MaxDiffLines        int  `json:"maxDiffLines"` // a bigger fix is held for review
	PublishWithoutSmoke bool `json:"publishWithoutSmoke"`
	RegressionWindowHrs int  `json:"regressionWindowHours"`
}

// DefaultProjectAutopilot is the block of a project that never set one.
func DefaultProjectAutopilot() ProjectAutopilot {
	return ProjectAutopilot{AutoTriage: true, GithubRelease: true, AutoClose: true, CoalesceMinutes: 60,
		MaxBatchAgeHours: 24, MaxReleasesPerDay: 2, MaxDiffLines: 400, RegressionWindowHrs: 48}
}

// IsZero reports a block equal to the defaults; it is not written (omitzero),
// so a newer build's defaults reach projects that never changed it.
func (p ProjectAutopilot) IsZero() bool {
	d := DefaultProjectAutopilot()
	pub := p.Publish
	p.Publish, d.Publish = nil, nil
	return reflect.DeepEqual(p, d) && len(pub) == 0
}

// Version source kinds (VersionProfile.Kind).
const (
	VersionFactorioInfo = "factorio-info"
	VersionJSON         = "json"
	VersionRegex        = "regex"
	VersionGitTag       = "git-tag"
)

// Changelog source kinds (ChangelogProfile.Kind).
const (
	ChangelogFactorio       = "factorio"
	ChangelogKeepAChangelog = "keepachangelog"
	ChangelogCommits        = "commits"
)

// Smoke test kinds (SmokeProfile.Kind).
const (
	SmokeFactorio = "factorio"
	SmokeCommand  = "command"
	SmokeNone     = "none"
)

// PublishProfile says how a code project is built, versioned and published
// (agents.projects.<key>.publishProfile). It lives in the app config, not in
// the repo, and holds no secrets (credentials stay in data\secrets).
type PublishProfile struct {
	Build BuildProfile `json:"build,omitzero"`
	// SteamContent is the folder handed to steamcmd (Steam targets only).
	SteamContent string           `json:"steamContent,omitempty"`
	Version      VersionProfile   `json:"version,omitzero"`
	Changelog    ChangelogProfile `json:"changelog,omitzero"`
	Smoke        SmokeProfile     `json:"smoke,omitzero"`
	// Targets configures each publish target, keyed platform:external_id.
	Targets map[string]TargetProfile `json:"targets,omitempty"`
}

// BuildProfile: Command (+ Output, may use {name} {version}) builds the
// archive; or Path names a ready archive (manual releases only).
type BuildProfile struct {
	Command string `json:"command,omitempty"`
	Output  string `json:"output,omitempty"`
	Path    string `json:"path,omitempty"`
}

// VersionProfile is where the project's version lives. Its fields match
// release/source.VersionSource (convertible).
type VersionProfile struct {
	Kind    string `json:"kind,omitempty"`    // factorio-info | json | regex | git-tag
	Path    string `json:"path,omitempty"`    // file in the repo (json, regex; factorio-info: default info.json)
	Key     string `json:"key,omitempty"`     // json: dot path to the value
	Pattern string `json:"pattern,omitempty"` // regex: exactly one capture group
}

// ChangelogProfile is where release notes come from. Its fields match
// release/source.ChangelogSource (convertible).
type ChangelogProfile struct {
	Kind string `json:"kind,omitempty"` // factorio | keepachangelog | commits
	Path string `json:"path,omitempty"` // default changelog.txt / CHANGELOG.md
}

// SmokeProfile is the post-build smoke test.
type SmokeProfile struct {
	Kind  string `json:"kind,omitempty"` // factorio | command | none
	Save  string `json:"save,omitempty"` // factorio: save to load ("" = a fresh map)
	Ticks int    `json:"ticks,omitempty"`
	// Install is the Factorio install root or factorio.exe ("" = auto-detect: Steam libraries, Program Files).
	Install string `json:"install,omitempty"`
	// Command runs in a temp dir with {archive} (quoted path), {name}, {version} replaced.
	Command string `json:"command,omitempty"`
}

// TargetProfile configures one publish target; each platform reads its own fields.
type TargetProfile struct {
	FileID          string   `json:"fileId,omitempty"`   // nexus: file group to update
	Category        string   `json:"category,omitempty"` // nexus
	ArchivePrevious bool     `json:"archivePrevious,omitempty"`
	AppID           int      `json:"appId,omitempty"`        // steam
	GameVersions    []string `json:"gameVersions,omitempty"` // curseforge
	ReleaseType     string   `json:"releaseType,omitempty"`  // curseforge: release | beta | alpha
}

// UnmarshalJSON fills the autopilot defaults before the stored keys, so a
// project without the block (or with part of it) gets the documented defaults.
func (p *ProjectAgent) UnmarshalJSON(b []byte) error {
	type plain ProjectAgent
	v := plain(*p)
	if reflect.DeepEqual(v.Autopilot, ProjectAutopilot{}) {
		v.Autopilot = DefaultProjectAutopilot()
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	*p = ProjectAgent(v)
	return nil
}

// AutopilotFor is project's (platform:external_id) autopilot block, defaults when unset.
func (a Agents) AutopilotFor(project string) ProjectAutopilot {
	p, ok := a.Projects[project]
	if !ok {
		return DefaultProjectAutopilot()
	}
	return p.Autopilot
}

const (
	maxAutopilotReleases  = 50
	maxAutopilotPublishes = 200
	maxProjectReleases    = 20
	maxCoalesceMinutes    = 1440
	maxBatchAgeHours      = 168
	maxDiffLinesCap       = 100000
	maxRegressionHours    = 720
	maxPublishTargets     = 32
	maxProfileText        = 4096
	maxSmokeTicks         = 1000000
)

func (g AgentsAutopilot) validate() error {
	const base = "agents.autopilot."
	switch {
	case g.MaxReleasesPerDay < 1 || g.MaxReleasesPerDay > maxAutopilotReleases:
		return outOfRange(base+"maxReleasesPerDay", 1, maxAutopilotReleases)
	case g.MaxPublishesPerDay < 1 || g.MaxPublishesPerDay > maxAutopilotPublishes:
		return outOfRange(base+"maxPublishesPerDay", 1, maxAutopilotPublishes)
	}
	return nil
}

func (p ProjectAutopilot) validate(project string) error {
	base := "agents.projects." + project + ".autopilot."
	switch {
	case p.CoalesceMinutes < 1 || p.CoalesceMinutes > maxCoalesceMinutes:
		return outOfRange(base+"coalesceMinutes", 1, maxCoalesceMinutes)
	case p.MaxBatchAgeHours < 1 || p.MaxBatchAgeHours > maxBatchAgeHours:
		return outOfRange(base+"maxBatchAgeHours", 1, maxBatchAgeHours)
	case p.MaxReleasesPerDay < 1 || p.MaxReleasesPerDay > maxProjectReleases:
		return outOfRange(base+"maxReleasesPerDay", 1, maxProjectReleases)
	case p.MaxDiffLines < 1 || p.MaxDiffLines > maxDiffLinesCap:
		return outOfRange(base+"maxDiffLines", 1, maxDiffLinesCap)
	case p.RegressionWindowHrs < 1 || p.RegressionWindowHrs > maxRegressionHours:
		return outOfRange(base+"regressionWindowHours", 1, maxRegressionHours)
	case len(p.Publish) > maxPublishTargets:
		return invalid(base+"publish", "tooMany", map[string]any{"max": maxPublishTargets}, "at most %d targets", maxPublishTargets)
	}
	for _, k := range slices.Sorted(maps.Keys(p.Publish)) {
		if !projectKey.MatchString(k) {
			return invalid(base+"publish."+k, "projectKey", nil, "must be platform:id (e.g. nexus:game/123)")
		}
	}
	return nil
}

// repoPath reports a relative path that stays inside the repository.
func repoPath(p string) bool {
	if p == "" || filepath.IsAbs(p) || filepath.VolumeName(p) != "" || strings.HasPrefix(p, "/") || strings.HasPrefix(p, `\`) {
		return false
	}
	c := filepath.ToSlash(filepath.Clean(p))
	return c != ".." && !strings.HasPrefix(c, "../")
}

var releaseTypes = []string{"release", "beta", "alpha"}

func (p PublishProfile) validate(project string) error {
	base := "agents.projects." + project + ".publishProfile."
	b := p.Build
	switch {
	case b.Command != "" && b.Path != "":
		return invalid(base+"build", "exclusive", nil, "set command (+ output) or path, not both")
	case b.Command != "" && b.Output == "":
		return invalid(base+"build.output", "required", nil, "required with build.command")
	case b.Output != "" && !repoPath(b.Output):
		return invalid(base+"build.output", "repoPath", nil, "must be a path inside the repository")
	case len(b.Command) > maxProfileText || len(b.Path) > maxProfileText || len(p.SteamContent) > maxProfileText:
		return invalid(base+"build", "tooLong", map[string]any{"max": maxProfileText}, "too long")
	}
	if err := p.Version.validate(base + "version."); err != nil {
		return err
	}
	c := p.Changelog
	switch {
	case c.Kind != "" && !slices.Contains([]string{ChangelogFactorio, ChangelogKeepAChangelog, ChangelogCommits}, c.Kind):
		return notOneOf(base+"changelog.kind", ChangelogFactorio, ChangelogKeepAChangelog, ChangelogCommits)
	case c.Path != "" && !repoPath(c.Path):
		return invalid(base+"changelog.path", "repoPath", nil, "must be a path inside the repository")
	}
	s := p.Smoke
	switch {
	case s.Kind != "" && !slices.Contains([]string{SmokeFactorio, SmokeCommand, SmokeNone}, s.Kind):
		return notOneOf(base+"smoke.kind", SmokeFactorio, SmokeCommand, SmokeNone)
	case s.Kind == SmokeCommand && s.Command == "":
		return invalid(base+"smoke.command", "required", nil, "required for a command smoke test")
	case s.Ticks < 0 || s.Ticks > maxSmokeTicks:
		return outOfRange(base+"smoke.ticks", 0, maxSmokeTicks)
	case len(s.Command) > maxProfileText || len(s.Save) > maxProfileText || len(s.Install) > maxProfileText:
		return invalid(base+"smoke", "tooLong", map[string]any{"max": maxProfileText}, "too long")
	case len(p.Targets) > maxPublishTargets:
		return invalid(base+"targets", "tooMany", map[string]any{"max": maxPublishTargets}, "at most %d targets", maxPublishTargets)
	}
	for _, k := range slices.Sorted(maps.Keys(p.Targets)) {
		t := p.Targets[k]
		f := base + "targets." + k
		switch {
		case !projectKey.MatchString(k):
			return invalid(f, "projectKey", nil, "must be platform:id (e.g. nexus:game/123)")
		case t.ReleaseType != "" && !slices.Contains(releaseTypes, t.ReleaseType):
			return notOneOf(f+".releaseType", releaseTypes...)
		case t.AppID < 0:
			return outOfRange(f+".appId", 0, 1<<31-1)
		case len(t.FileID) > 64 || len(t.Category) > 64 || len(t.GameVersions) > 50:
			return invalid(f, "tooLong", nil, "too long")
		}
	}
	return nil
}

func (v VersionProfile) validate(base string) error {
	kinds := []string{VersionFactorioInfo, VersionJSON, VersionRegex, VersionGitTag}
	switch {
	case v.Kind != "" && !slices.Contains(kinds, v.Kind):
		return notOneOf(base+"kind", kinds...)
	case v.Path != "" && !repoPath(v.Path):
		return invalid(base+"path", "repoPath", nil, "must be a path inside the repository")
	case (v.Kind == VersionJSON || v.Kind == VersionRegex) && v.Path == "":
		return invalid(base+"path", "required", nil, "required for kind %s", v.Kind)
	case v.Kind == VersionJSON && v.Key == "":
		return invalid(base+"key", "required", nil, "required for kind json")
	case v.Kind == VersionRegex && v.Pattern == "":
		return invalid(base+"pattern", "required", nil, "required for kind regex")
	}
	if v.Pattern != "" {
		re, err := regexp.Compile(v.Pattern)
		if err != nil {
			return invalid(base+"pattern", "regex", nil, "%v", err)
		}
		if re.NumSubexp() != 1 {
			return invalid(base+"pattern", "regexGroup", nil, "must have exactly one capture group")
		}
	}
	return nil
}

// secretWords mark keys that look like credentials; the autopilot blocks must
// not hold any (they would sit in plain config.json, unknown keys included).
var secretWords = []string{"token", "secret", "password", "passwd", "apikey", "api_key", "api-key", "credential", "cookie", "privatekey", "private_key"}

// checkNoSecrets refuses secret-looking keys anywhere inside the projects'
// autopilot and publishProfile blocks of raw (known and unknown keys).
func checkNoSecrets(raw map[string]any) error {
	ag, _ := raw["agents"].(map[string]any)
	projects, _ := ag["projects"].(map[string]any)
	for _, name := range slices.Sorted(maps.Keys(projects)) {
		pm, _ := projects[name].(map[string]any)
		for _, block := range []string{"autopilot", "publishProfile"} {
			if err := secretKey(fmt.Sprintf("agents.projects.%s.%s", name, block), pm[block]); err != nil {
				return err
			}
		}
	}
	return nil
}

func secretKey(path string, v any) error {
	switch t := v.(type) {
	case map[string]any:
		for _, k := range slices.Sorted(maps.Keys(t)) {
			lk := strings.ToLower(k)
			if slices.ContainsFunc(secretWords, func(w string) bool { return strings.Contains(lk, w) }) {
				return invalid(path+"."+k, "secret", nil, "secrets do not belong in the config (data\\secrets holds credentials)")
			}
			if err := secretKey(path+"."+k, t[k]); err != nil {
				return err
			}
		}
	case []any:
		for i, e := range t {
			if err := secretKey(fmt.Sprintf("%s.%d", path, i), e); err != nil {
				return err
			}
		}
	}
	return nil
}
