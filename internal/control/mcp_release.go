package control

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Shared parts of the release tool descriptions.
const (
	descCallerRefusal = " Refused with code agent_caller when called from inside an IssueWatcher agent run (a job's shell or MCP server), " +
		"caller_unknown when the app cannot tell the calling program."
	descRefusals = " Refusal codes (409, the error text lists every refusal with its code): no_profile, no_folder, dirty_folder, " +
		"not_default_branch, head_not_remote, version_conflict, tag_exists, no_targets, busy (an unfinished run of the project), " +
		"cap_reached (daily release / publish caps), paused (global kill switch), disabled (the project's autopilot.enabled is off; " +
		"needed even for a manual release), bad_request, remote_error. A mod page id instead of its GitHub code project: code not_code_project."
)

// addReleaseTools registers the autopilot release tools (docs/AUTOPILOT.md →
// MCP / CLI surface). Owner server only: job-scoped servers never get them.
func addReleaseTools(s *mcp.Server, c *Client) {
	ro := &mcp.ToolAnnotations{ReadOnlyHint: true}
	type project struct {
		Project int64 `json:"project" jsonschema:"the GitHub code project id from list_projects"`
	}
	add(s, &mcp.Tool{Name: "get_publish_profile", Description: "Get a code project's release setup in one call: publishProfile (build, version " +
		"and changelog sources, smoke, per-target settings such as the Nexus fileId), autopilot switches and caps, the global pause and caps, " +
		"allowed source kinds, revision (pass it to set_publish_profile), and resolved: the dry-run plan (current and next version, " +
		"changelog preview, targets with latest versions and auth state, steps, refusals). Read-only.", Annotations: ro},
		func(ctx context.Context, in project) (json.RawMessage, error) {
			return c.PublishProfile(ctx, in.Project)
		})

	type setProfile struct {
		Project        int64          `json:"project" jsonschema:"the GitHub code project id from list_projects"`
		Revision       int            `json:"revision,omitempty" jsonschema:"settings revision from get_publish_profile; a stale one fails with conflict (re-read and retry)"`
		PublishProfile map[string]any `json:"publish_profile" jsonschema:"the full publishProfile object (as returned by get_publish_profile, edited): it REPLACES the stored one"`
		DryRun         bool           `json:"dry_run,omitempty" jsonschema:"validate only: returns the profile as it would be saved, nothing is written"`
	}
	add(s, &mcp.Tool{Name: "set_publish_profile", Description: "Replace a code project's publishProfile (build command and output, version source " +
		"{kind, path, key, pattern}, changelog source, smoke, targets {key: {fileId, category, ...}}). Send the whole object from " +
		"get_publish_profile with your edits; keys left out are removed. Validated by the app (400 with the field on error); no secrets " +
		"are stored here (platform credentials stay in the app). Autopilot switches are not changed by this tool (set_autopilot_settings). " +
		"smoke: {kind: factorio|command|none, save (factorio: a save .zip; empty = a fresh map), ticks (default 600), install (factorio " +
		"root or factorio.exe; empty = auto-detect), command (command: runs in a temp dir; {archive} {name} {version} are replaced)}; " +
		"kind none holds every release unless the project's autopilot.publishWithoutSmoke is on. " +
		"dry_run validates and returns {dryRun, ok, publishProfile, autopilot} without saving (allowed for agent runs). " +
		"Otherwise returns the new profile with its resolved plan." + descCallerRefusal},
		func(ctx context.Context, in setProfile) (json.RawMessage, error) {
			if in.PublishProfile == nil {
				return nil, errors.New("publish_profile: give the full publishProfile object")
			}
			body := map[string]any{"publishProfile": in.PublishProfile}
			if in.DryRun {
				body["dryRun"] = true
			}
			if in.Revision > 0 {
				body["revision"] = in.Revision
			}
			b, err := json.Marshal(body)
			if err != nil {
				return nil, err
			}
			return c.SetPublishProfile(ctx, in.Project, b)
		})

	add(s, &mcp.Tool{Name: "get_autopilot_settings", Description: "Get a code project's autopilot switches: {projectId, project, revision " +
		"(pass it to set_autopilot_settings), autopilot {enabled, autoTriage, autoFix, autoPush, autoRelease, githubRelease, publish " +
		"{target key: on}, autoReply, autoClose, coalesceMinutes, maxBatchAgeHours, maxReleasesPerDay, maxDiffLines, publishWithoutSmoke, " +
		"regressionWindowHours}, global {paused, maxReleasesPerDay, maxPublishesPerDay}}. Read-only.", Annotations: ro},
		func(ctx context.Context, in project) (json.RawMessage, error) {
			return c.AutopilotSettings(ctx, in.Project)
		})

	type setAutopilot struct {
		Project   int64          `json:"project" jsonschema:"the GitHub code project id from list_projects"`
		Revision  int            `json:"revision,omitempty" jsonschema:"settings revision from get_autopilot_settings; a stale one fails with conflict (re-read and retry)"`
		Autopilot map[string]any `json:"autopilot" jsonschema:"the full autopilot object (as returned by get_autopilot_settings, edited): it REPLACES the stored one"`
		DryRun    bool           `json:"dry_run,omitempty" jsonschema:"validate only: returns the block as it would be saved, nothing is written"`
	}
	add(s, &mcp.Tool{Name: "set_autopilot_settings", Description: "Replace a code project's autopilot block (the project's switches, publish " +
		"targets, caps, publishWithoutSmoke). Send the whole object from get_autopilot_settings with your edits; keys left out get their " +
		"defaults. Validated by the app (400 with the field on error). The global pause is not changed here (pause_autopilot). " +
		"dry_run validates and returns the block as it would be saved (allowed for agent runs). Returns {projectId, project, revision, " +
		"autopilot, global}." + descCallerRefusal},
		func(ctx context.Context, in setAutopilot) (json.RawMessage, error) {
			if in.Autopilot == nil {
				return nil, errors.New("autopilot: give the full autopilot object")
			}
			body := map[string]any{"autopilot": in.Autopilot}
			if in.DryRun {
				body["dryRun"] = true
			}
			if in.Revision > 0 {
				body["revision"] = in.Revision
			}
			b, err := json.Marshal(body)
			if err != nil {
				return nil, err
			}
			return c.SetAutopilotSettings(ctx, in.Project, b)
		})

	type plan struct {
		Project int64    `json:"project" jsonschema:"the GitHub code project id from list_projects"`
		Version string   `json:"version,omitempty" jsonschema:"explicit version (e.g. 1.3.0; minor or major allowed); default: the next patch"`
		Head    string   `json:"head,omitempty" jsonschema:"commit sha to release; must equal the folder HEAD and the remote head"`
		Items   []int64  `json:"items,omitempty" jsonschema:"item ids the release fixes (for the changelog and replies)"`
		Targets []string `json:"targets,omitempty" jsonschema:"target keys to publish to (e.g. nexus:game/123); default: the enabled ones"`
	}
	add(s, &mcp.Tool{Name: "plan_release", Description: "Dry-run a release of a code project: returns the plan {ok, refusals [{code, message}], " +
		"folder, branch, head, remoteHead, currentVersion, version, tag, files the bump writes, changelog, asset, targets with auth state, " +
		"steps, caps, paused, enabled}. Nothing is changed. ok=false lists why release would refuse (same codes as release)." + descRefusals, Annotations: ro},
		func(ctx context.Context, in plan) (json.RawMessage, error) {
			return c.ReleasePlan(ctx, in.Project, ReleaseRequest{Version: in.Version, Head: in.Head, Items: in.Items, Targets: in.Targets})
		})

	type rel struct {
		Project int64    `json:"project" jsonschema:"the GitHub code project id from list_projects"`
		Version string   `json:"version,omitempty" jsonschema:"explicit version (e.g. 1.3.0; minor or major allowed); default: the next patch"`
		Head    string   `json:"head,omitempty" jsonschema:"commit sha to release; must equal the folder HEAD and the remote head"`
		Items   []int64  `json:"items,omitempty" jsonschema:"item ids the release fixes (for the changelog and replies)"`
		Targets []string `json:"targets,omitempty" jsonschema:"target keys to publish to (e.g. nexus:game/123); default: the enabled ones"`
		DryRun  bool     `json:"dry_run,omitempty" jsonschema:"plan only (same as plan_release, but a refusal is an error): nothing is started"`
	}
	add(s, &mcp.Tool{Name: "release", Description: "Start a release run of a code project (origin mcp): bump version and changelog with a commit, " +
		"build in a clean worktree, archive check, gate, push and an annotated tag, GitHub release with the asset, publish to every selected " +
		"target, wait for availability. Public and irreversible once pushed: run with dry_run (or plan_release) first. This is NOT owner " +
		"approval: the same gate, caps and rails apply as for an automatic release. Runs in the background: returns {run, plan} at once; " +
		"poll get_run with run.id (state pending|running|held|done|cancelled|failed; held = needs resume_run, skip_step or cancel_run). " +
		"Never call it again for the same release after an error: read list_runs first." + descRefusals + descCallerRefusal},
		func(ctx context.Context, in rel) (json.RawMessage, error) {
			return c.Release(ctx, in.Project, ReleaseRequest{Version: in.Version, Head: in.Head, Items: in.Items, Targets: in.Targets, DryRun: in.DryRun})
		})

	type listRuns struct {
		Project int64  `json:"project,omitempty" jsonschema:"only this project's runs"`
		State   string `json:"state,omitempty" jsonschema:"pending, running, held, done, cancelled or failed"`
		Kind    string `json:"kind,omitempty" jsonschema:"run kind (release)"`
		Limit   int    `json:"limit,omitempty" jsonschema:"newest runs to return, default 20, max 50"`
	}
	add(s, &mcp.Tool{Name: "list_runs", Description: "List autopilot runs, newest first: {id, kind, projectId, state, origin, version, heldReason, " +
		"manifest, createdAt, updatedAt}. Read-only.", Annotations: ro},
		func(ctx context.Context, in listRuns) (json.RawMessage, error) {
			return c.Runs(ctx, RunQuery{Project: in.Project, State: in.State, Kind: in.Kind, Limit: pageLimit(in.Limit)})
		})

	type runID struct {
		ID int64 `json:"id" jsonschema:"run id from release or list_runs"`
	}
	add(s, &mcp.Tool{Name: "get_run", Description: "Get one run {run, steps, items}: run.state and heldReason (check:<step> = unclear outcome, " +
		"re-probed on resume, never resent; failed:<step>, auth:<platform>, foreign_commits, remote_moved, gate_failed, no_verify (no Aegis " +
		"and no verify command), smoke_failed, smoke_missing (no smoke test, publishWithoutSmoke off), smoke_unavailable, ...), and every step " +
		"(release: bump, build, archive_check, gate, smoke, push, tag, gh_release, gh_asset, publish:<target>, available:<target>, " +
		"reply:<item>, close:<item>; fix run (kind fix): fix, verify, push; fix heldReason diff_too_big, diff_review, closing_keyword, " +
		"needs_info, not_reproduced, no_commit, fix:<reason>) with its state " +
		"(pending|sending|sent|failed|unknown|skipped), externalRef and error. Read-only.", Annotations: ro},
		func(ctx context.Context, in runID) (json.RawMessage, error) { return c.Run(ctx, in.ID) })

	add(s, &mcp.Tool{Name: "resume_run", Description: "Resume a held run after its cause is fixed: steps of unknown outcome are probed first, " +
		"never blindly resent; then the run continues in the background (poll get_run). Only a held run (else code bad_state). No dry run."},
		func(ctx context.Context, in runID) (json.RawMessage, error) { return c.RunAction(ctx, in.ID, "resume") })

	add(s, &mcp.Tool{Name: "cancel_run", Description: "Cancel an unfinished run. Before any public step (push) it also drops the app's own bump " +
		"commit (only when HEAD is that commit, the folder is clean and it is provably not on the remote): bumpDropped tells. After a public " +
		"step it only stops: what is pushed or published stays. Returns {run, bumpDropped, note}. A finished run: code bad_state. No dry run."},
		func(ctx context.Context, in runID) (json.RawMessage, error) { return c.RunAction(ctx, in.ID, "cancel") })

	type skip struct {
		Run    int64  `json:"run" jsonschema:"run id"`
		Step   string `json:"step" jsonschema:"publish, available, reply or close, or publish:<target> / available:<target> / reply:<item> / close:<item>"`
		Target string `json:"target,omitempty" jsonschema:"target key (e.g. nexus:game/123) when step has none"`
	}
	add(s, &mcp.Tool{Name: "skip_step", Description: "Skip a held or pending run's publish or available step of one target (the target is then " +
		"left out of the replies; the rest goes on), or one item's reply / close step (target = item id). Only publish / available / " +
		"reply / close steps (else code bad_step); a publish already sent cannot be skipped (bad_state); " +
		"no such step: 404. No dry run." + descCallerRefusal},
		func(ctx context.Context, in skip) (json.RawMessage, error) {
			return c.SkipStep(ctx, in.Run, in.Step, in.Target)
		})

	type pause struct {
		Project int64 `json:"project,omitempty" jsonschema:"pause only this project (turns its autopilot.enabled off); omit for the global kill switch"`
	}
	add(s, &mcp.Tool{Name: "pause_autopilot", Description: "Pull the kill switch: with project, turn that project's autopilot off; without, pause " +
		"every autopilot action (releases of every origin refuse with paused / disabled). Running steps finish; nothing new starts. " +
		"This tool can only pause: un-pausing is the owner's, in the app. Returns {paused, project?, enabled?}."},
		func(ctx context.Context, in pause) (json.RawMessage, error) {
			return c.PauseAutopilot(ctx, in.Project, true)
		})

	type events struct {
		UnreadOnly bool `json:"unread_only,omitempty" jsonschema:"only events the owner has not read"`
		Limit      int  `json:"limit,omitempty" jsonschema:"newest events to return, default 20, max 50"`
	}
	add(s, &mcp.Tool{Name: "list_autopilot_events", Description: "List the autopilot activity log, newest first: {events [{id, at, runId, projectId, " +
		"itemId, kind (release.held|release.done|release.cancelled), severity (info|attention), title, detail, readAt}], unread, attention}. " +
		"Read-only (does not mark events read).", Annotations: ro},
		func(ctx context.Context, in events) (json.RawMessage, error) {
			return c.AutopilotEvents(ctx, in.UnreadOnly, pageLimit(in.Limit))
		})
}
