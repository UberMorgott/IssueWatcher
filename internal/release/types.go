// Package release runs autopilot release runs (docs/AUTOPILOT.md → Release
// run): plan (dry run with refusals) → frozen manifest → bump → clean-worktree
// build → archive check → gate → push + tag from an app-owned mirror → GitHub
// release + asset → publish to each mod platform → availability. Every
// external step is compare-and-set to sending before the call and probed
// (read-only) before any send and after a crash.
package release

import (
	"github.com/UberMorgott/issuewatcher/internal/config"
	"github.com/UberMorgott/issuewatcher/internal/provider"
)

// Step names (store.Step.Step). Target-scoped steps carry the target in store.Step.Target.
const (
	StepBump         = "bump"
	StepBuild        = "build"
	StepArchiveCheck = "archive_check"
	StepGate         = "gate"
	StepSmoke        = "smoke"
	StepPush         = "push"
	StepTag          = "tag"
	StepGHRelease    = "gh_release"
	StepGHAsset      = "gh_asset"  // target = asset file name
	StepPublish      = "publish"   // target = platform:external_id
	StepAvailable    = "available" // target = platform:external_id
	StepReply        = "reply"     // target = item id: the reporter's reply with the version and links
	StepClose        = "close"     // target = item id: close the GitHub issue (after its reply)

	// Fix run steps (docs/AUTOPILOT.md → Fix run): fix → verify → push (StepPush).
	StepFix    = "fix"    // the autopilot direct fix job (external_ref = job id)
	StepVerify = "verify" // the static gate on the fix head + diff limits
	// StepTriage (auto-triage, first step of a fix run): the classify agent's
	// verdict (external_ref = Classification JSON). A fix run's reply step
	// (StepReply, target = item id) answers a question / asks for details /
	// gives a duplicate the original's release.
	StepTriage = "triage"
)

// Refusal codes of a plan / start (Refusal.Code) and held reasons.
const (
	CodeNoProfile        = "no_profile"         // publish profile incomplete (build / version / changelog)
	CodeNoFolder         = "no_folder"          // not a GitHub code project with a mapped git folder
	CodeDirtyFolder      = "dirty_folder"       // uncommitted changes in the mapped folder
	CodeNotDefaultBranch = "not_default_branch" // the folder is not on the default branch
	CodeHeadNotRemote    = "head_not_remote"    // folder HEAD (or the given head) != remote default branch head
	CodeVersionConflict  = "version_conflict"   // version not above the source / a target's latest
	CodeTagExists        = "tag_exists"         // vX.Y.Z exists locally or on the remote
	CodeNoTargets        = "no_targets"         // no enabled, configured publish target
	CodeBusy             = "busy"               // an unfinished release run of the project exists
	CodeCapReached       = "cap_reached"        // daily release / publish cap used up
	CodePaused           = "paused"             // agents.autopilot.paused (global kill switch)
	CodeDisabled         = "disabled"           // the project's autopilot.enabled is off
	CodeBadRequest       = "bad_request"        // bad version / items / targets in the request
	CodeRemote           = "remote_error"       // the remote / platform could not be read
	// CodeNoVerify: neither Aegis nor a verify command (no run, no daily slot used).
	CodeNoVerify = "no_verify"
	// CodeNoSmoke: no smoke test while «Публиковать без смоук-теста» is off.
	CodeNoSmoke = "smoke_missing"
)

// Held reasons (store.Run.HeldReason) besides check:<step> and failed:<step>.
const (
	HeldTagConflict    = "tag_conflict"
	HeldForeignCommits = "foreign_commits"
	HeldRemoteMoved    = "remote_moved"
	HeldFolderMoved    = "folder_moved"
	HeldDirtyBuild     = "dirty_build"
	HeldArchive        = "archive_check"
	HeldGate           = "gate_failed"
	HeldVersion        = "version_conflict"
	HeldArtifact       = "artifact_changed"
	HeldAvailability   = "availability_timeout"
	// HeldNoVerify: neither Aegis nor a verify command (owner rule: no publish without verification).
	HeldNoVerify = "no_verify"
	// HeldSmokeFailed: the smoke test failed on the built archive.
	HeldSmokeFailed = "smoke_failed"
	// HeldSmokeMissing: no smoke test and publishWithoutSmoke is off.
	HeldSmokeMissing = "smoke_missing"
	// HeldSmokeUnavailable: the smoke adapter cannot run here (no install, missing dependency).
	HeldSmokeUnavailable = "smoke_unavailable"
	// HeldReplyWaiting: a reply step found a target neither available nor skipped.
	HeldReplyWaiting = "reply_waiting"

	// Fix run held reasons.
	HeldDiffTooBig     = "diff_too_big"    // more changed lines than autopilot.maxDiffLines
	HeldDiffReview     = "diff_review"     // touches CI / build / release / profile / dependency files, or deletes files
	HeldClosingKeyword = "closing_keyword" // a commit closes the issue (Fixes #N) before the release
	HeldNeedsInfo      = "needs_info"
	HeldNotReproduced  = "not_reproduced"
	HeldNoCommit       = "no_commit"
	HeldNotDefault     = "not_default_branch" // the fix is not on the default branch
)

// Manifest is a release run's frozen input (store.Run.Manifest). Written once
// at start; BumpSHA and Changelog are filled once by the bump step.
type Manifest struct {
	ProjectID     int64                 `json:"projectId"`
	Project       string                `json:"project"` // github:owner/repo
	Repo          string                `json:"repo"`    // owner/repo
	RepoURL       string                `json:"repoUrl"`
	Folder        string                `json:"folder"`
	Branch        string                `json:"branch"`
	Head          string                `json:"head"`    // verified HEAD == remote default branch head at start
	BaseTag       string                `json:"baseTag"` // last v* tag ("" = none)
	FromVersion   string                `json:"fromVersion"`
	Version       string                `json:"version"`
	Tag           string                `json:"tag"`
	ModName       string                `json:"modName"` // {name} of build.output
	Asset         string                `json:"asset"`   // archive file name
	GithubRelease bool                  `json:"githubRelease"`
	Profile       config.PublishProfile `json:"profile"`
	Targets       []Target              `json:"targets"`
	Items         []int64               `json:"items"`
	Identity      [2]string             `json:"identity"` // committer / tagger name, email
	BumpSHA       string                `json:"bumpSha,omitempty"`
	Changelog     string                `json:"changelog,omitempty"`
	// Replies are the items answered by reply:<item> (and closed by close:<item>);
	// RepliesPending the items whose reply waits for the owner (autoReply off,
	// or the platform cannot reply).
	Replies        []ReplyItem `json:"replies,omitempty"`
	RepliesPending []int64     `json:"repliesPending,omitempty"`
	// FixRuns are the fix runs this release claimed (autopilot).
	FixRuns []int64 `json:"fixRuns,omitempty"`
}

// ReplyItem is an item a release answers.
type ReplyItem struct {
	ID       int64  `json:"id"`
	Platform string `json:"platform"`
	Number   int    `json:"number"`
	URL      string `json:"url"`
	Close    bool   `json:"close"` // a close:<item> step follows its reply (GitHub issue, autoClose)
}

// Target is one publish target (a linked mod page with a Publisher).
type Target struct {
	Key        string               `json:"key"` // platform:external_id
	ProjectID  int64                `json:"projectId"`
	Platform   string               `json:"platform"`
	ExternalID string               `json:"externalId"`
	Name       string               `json:"name"`
	URL        string               `json:"url"`
	Profile    config.TargetProfile `json:"profile"`
}

func (t Target) project() provider.Project {
	return provider.Project{ExternalID: t.ExternalID, Name: t.Name, URL: t.URL}
}

// Artifact is the built archive (build step's external ref).
type Artifact struct {
	Path   string `json:"path"`
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
	SHA1   string `json:"sha1"`
	MD5    string `json:"md5"`
}

// Refusal is why a plan cannot start.
type Refusal struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Request is POST /api/projects/{id}/release (and plan).
type Request struct {
	Version string   `json:"version,omitempty"` // explicit (minor/major allowed); "" = next patch
	Head    string   `json:"head,omitempty"`    // must equal the remote default branch head
	Items   []int64  `json:"items,omitempty"`   // items this release answers
	Targets []string `json:"targets,omitempty"` // subset of the enabled targets; empty = all
	DryRun  bool     `json:"dryRun,omitempty"`
	// Origin is set by the API from the caller: manual (the owner's browser session)
	// bypasses paused / disabled; mcp (bearer: MCP / CLI) is refused by both. "" = manual.
	Origin string `json:"-"`
	// Claim are pushed fix runs the release answers for (the coalescing timer):
	// claimed in the run's create transaction.
	Claim []int64 `json:"-"`
}

// PlanTarget is a target as the plan sees it.
type PlanTarget struct {
	Key           string `json:"key"`
	ProjectID     int64  `json:"projectId"`
	Platform      string `json:"platform"`
	Name          string `json:"name"`
	URL           string `json:"url"`
	Enabled       bool   `json:"enabled"`       // autopilot.publish[key]
	Configured    bool   `json:"configured"`    // publishProfile.targets[key]
	Publishable   bool   `json:"publishable"`   // the platform has a Publisher
	Selected      bool   `json:"selected"`      // part of this release
	LatestVersion string `json:"latestVersion"` // highest semver the platform lists ("" = none)
	Auth          string `json:"auth"`          // ok | no_api_key | bad_api_key | error | unavailable | unchecked
	Error         string `json:"error,omitempty"`
}

// PlanStep is one step the run would take.
type PlanStep struct {
	Step    string `json:"step"`
	Target  string `json:"target,omitempty"`
	IdemKey string `json:"idemKey,omitempty"`
	Request string `json:"request"` // what is sent / done (method + URL or command)
	Note    string `json:"note,omitempty"`
}

// Plan is the dry run of a release (and the resolved publish profile).
type Plan struct {
	OK             bool         `json:"ok"`
	Refusals       []Refusal    `json:"refusals"`
	ProjectID      int64        `json:"projectId"`
	Project        string       `json:"project"`
	Folder         string       `json:"folder"`
	Branch         string       `json:"branch"`
	Head           string       `json:"head"`
	RemoteHead     string       `json:"remoteHead"`
	BaseTag        string       `json:"baseTag"`
	CurrentVersion string       `json:"currentVersion"`
	Version        string       `json:"version"`
	Tag            string       `json:"tag"`
	WritesVersion  []string     `json:"writesVersion"`   // files the bump changes for the version
	WritesLog      []string     `json:"writesChangelog"` // files the bump changes for the changelog
	Changelog      string       `json:"changelog"`       // release body preview
	Asset          string       `json:"asset"`
	GithubRelease  bool         `json:"githubRelease"`
	SmokeKind      string       `json:"smokeKind"`
	Targets        []PlanTarget `json:"targets"`
	Steps          []PlanStep   `json:"steps"`
	Caps           Caps         `json:"caps"`
	Paused         bool         `json:"paused"`
	Enabled        bool         `json:"enabled"`

	manifest *Manifest // set when OK (start uses it)
}

// Caps are the daily release / publish counters (UTC day).
type Caps struct {
	ProjectReleasesToday int `json:"projectReleasesToday"`
	ProjectMaxReleases   int `json:"projectMaxReleases"`
	ReleasesToday        int `json:"releasesToday"`
	MaxReleases          int `json:"maxReleases"`
	PublishesToday       int `json:"publishesToday"`
	MaxPublishes         int `json:"maxPublishes"`
}
