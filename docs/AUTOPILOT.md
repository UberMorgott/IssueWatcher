# Autopilot — unattended fix → verify → release → publish → reply

Design 2026-10-05 (owner decisions of the same day; reviewed with Codex 2026-10-05, see **Review**). Checklist: `TASKS.md` → Phase 8. Builds on `ARCHITECTURE.md` → Runner, Automation, Control, Native mod platforms, and Phase 7 publishing.

**Goal**: the owner can walk away. A new issue / comment / bug report on a mod → triage → the coder fixes it in the mapped folder → the app verifies (build, lint, tests, in-game smoke test) → push → GitHub release → the fixed mod is uploaded to every enabled platform → the reporter gets a reply with the version and links → the issue is closed. Every step is a per-project checkbox; anything that fails stops the chain before the next public action and records an unread "needs attention" event in the app's autopilot log (in this document, "notification" always means such an event: there is no phone push).

## Owner decisions (2026-10-05)

1. **Verify gate** = the project's build/lint/tests **plus** an automated in-game smoke test where feasible (Factorio: headless load of the mod + a save; other games: whatever the adapter can do). Fail → nothing is published, an unread event is recorded in the autopilot log. *(Revised 2026-10-05: no phone push — see decision 5.)*
2. **Logins**: one-time sign-ins are fine, but **only through a form in the app** (Settings › Платформы). The owner types passwords / Steam Guard codes there; agents never see secrets. Steam Workshop uploads go through `steamcmd` driven by the app (the form pipes the credentials to steamcmd; its cached login is reused afterwards). No manual browser/cookie work, ever. An expired session is re-established silently (app browser profile, cached steamcmd login, stored API key); only when that fails → one notification with a link to the form.
3. **External agent sessions** (Claude/Codex) publish, reply and release **through the app** (MCP/CLI) with no explanation needed: self-describing tools + a per-project **publish profile** (archive build command or path, mod id per platform, version source, changelog source).

## What exists (2026-10-05)

| Piece | Where | Gap for autopilot |
|---|---|---|
| Rules → jobs (`new_issue`/`new_comment`/`new_item` → `fix`/`reply`/`label`), caps, decision log | `config/automation.go:12-60`, `store/automation.go` (`Automate`), migrations 006/007 | events are in memory (a crash between sync commit and callback loses triggers); nothing chains after the job |
| Direct fix: commit `Fixes #N`, never pushes; dirty folder refused | `runner/direct.go:68` (`runDirect`), `:299` (`Push`, manual only; `:398` runs the user's pre-push hooks with the token in env) | push is a click; mod items need `modPush`; `Fixes #N` closes the issue on push, before any release |
| Reply drafts, sent by click | `send_job_reply` (MCP), `syncer.Reply` with read-back | no automatic send |
| Triage (project job → picks → fix jobs) | Runner → Triage flow | per project, not per incoming item; not wired to rules |
| Publish: `provider.Publisher` (targets / check / publish once, `PublishError.UploadID`) | `provider/publish.go`, `api/publish.go:42` | tasks **in memory**; only Nexus; Factorio in progress (another agent) |
| GitHub App token with `contents: write` | `provider/github/auth.go:183-186` | no release API calls yet |
| Tray/popup notifications | `internal/notify` | transient cards only; no history of what autopilot did while the owner was away |
| MCP server / CLI | `control/mcp.go`, Control (Phase 4) | no release/run/profile tools; no caller check (an agent's shell can run `issuewatcher-cli`) |
| Coder runs | `runner/agent.go` | Codex direct fix = `danger-full-access`, same Windows user as the app (DPAPI secrets readable in principle) |

## Model: two pipelines

A fix and a release are different things: several reports can be fixed before one release, and one release answers several reporters. So autopilot has two persistent run kinds.

- **Fix run** (per incoming item, or a group of duplicates): `triage → fix → verify → push`. Ends `pushed` (on the default branch, unreleased), or `held` / `failed` with a reason.
- **Release run** (per code project): a **frozen manifest** of fix runs (or, manually, an explicit verified HEAD + optional items) → `bump → build → verify+smoke → push bump → tag → GitHub release → assets → publish ×N → await availability → reply ×N → close ×N`. Started by the coalescing timer (autopilot), a click, or MCP/CLI (`release`).

The code project (GitHub repo with a mapped folder) owns the autopilot settings; its linked mod pages (`project_links`) are the publish targets and item sources. A mod page without a code project cannot use autopilot.

### Fix run state machine

```
new ─▶ triage ─┬─▶ fix ─▶ verify ─▶ push ─▶ pushed ─(claimed by a release run)─▶ released
               │     └─▶ held (not_reproduced / needs_info → reply step if autoReply)
               ├─▶ duplicate (attached to the original's open run; or, if already released, replied to at once)
               ├─▶ reply_only (question → reply step)
               └─▶ ignored (spam / feature request / not actionable → notify, no action)
any step ─▶ failed (attempts left → retry later; none → held + notification)
```

- `triage` = a read-only agent (new flow `classify`, responder profile): `{kind: bug|question|feature|duplicate|spam|other, duplicateOf?: itemId, severity, actionable, regression, reason}`. Off → every item matching a `fix` rule goes to `fix` as today.
- `fix` = the existing direct fix job (rule origin, same caps), with the autopilot prompt variant: commits reference the issue **neutrally** (`Refs #N`, never a closing keyword — closing is the last step of the release run) and the facts check looks for that reference. `fixed_local` → `verify`; `not_reproduced` / `needs_info` → `held` (+ auto-reply asking for details when on); `no_commit` / `failed` → `failed`.
- `verify` = the static gate below on the fix's exact HEAD (smoke runs on the release archive).
- `push` = trusted push (see Safety rails → Git), fast-forward to the default branch.

### Release run state machine

```
claim ─▶ bump ─▶ build ─▶ gate ─▶ smoke ─▶ push ─▶ tag ─▶ gh_release ─▶ gh_asset:<name> ─▶ publish:<target> (each) ─▶ available:<target> (each)
      ─▶ reply:<item> (each) ─▶ close:<item> (each) ─▶ done
any step failed / unknown ─▶ held (owner notified; resume / skip target / cancel / rollback from UI, MCP or CLI)
```

- `claim`: in one transaction, every `pushed` fix run of the project gets `release_id` = this run (a fix run belongs to at most one release, ever). The **manifest** is written once and never changes: fix run ids + items, base sha (last release tag), profile + targets snapshot. Manual origin may instead give an explicit HEAD (must equal the remote default branch head and pass the gate) + optional item ids — Phase 1 works without fix runs.
- `bump`: next version (patch only for autopilot) + changelog entry; one commit `chore(release): vX.Y.Z` by the app; folder clean, branch = default branch; bump sha → manifest.
- `build`: in an **app-owned clean worktree** of the bump sha (`data\release\<run>`), never the mapped folder: profile build command → archive (or Steam content folder); worktree must be clean after the build except the declared output; record path, size, sha256 (Steam: content manifest = sorted relative paths + sha256), version read back from inside the archive (must equal the bump).
- `gate` + `smoke`: the verify gate on the bump sha and the smoke adapter on that exact artifact.
- `push`: the bump commit, trusted push, fast-forward.
- `tag`, `gh_release`, `gh_asset:<name>`: annotated tag `vX.Y.Z` on the bump sha; `POST /repos/{o}/{r}/releases {tag_name, target_commitish: sha, name, body: changelog}`; each asset its own step (`uploads.github.com`).
- `publish:<target>`: one step per enabled target, independent (one failing does not stop the others). The artifact's sha256 is re-checked right before each send.
- `available:<target>`: polls until the upload is downloadable (Nexus: version listed and not pending scan; Factorio: release in `/api/mods/<name>/full`; Steam: `time_updated` and visibility; CurseForge: file status approved — moderation may take hours/days, max wait 72 h → held).
- `reply:<item>`: per reporter (every item in the manifest, duplicates included), drafted read-only with the version and links of **available, non-skipped targets only**; waits until every non-skipped target is available (failed target → held; the owner can skip it, then replies go out without it).
- `close:<item>`: GitHub `PATCH issues/{n} {state: closed, state_reason: completed}`; mod platforms where a status exists later (Nexus bug status), else nothing.
- Terminal outcomes with steps off: `githubRelease=false` → tag only, no release/assets; `autoReply=false` → reply drafts become ordinary `reply` jobs in `needs_review` (the run ends `done_replies_pending`); a reply that fails never returns the fix to the pool (the fix stays `released`; the reply step is held/retried on its own).

### Persistence, idempotency, crash resume

Migration 016: `autopilot_runs (id, kind fix|release, project_id, state, origin auto|manual|mcp, release_id, manifest_json, version, artifact_sha256, created_at, updated_at, held_reason)` with a partial unique index **one unfinished release run per project**; `autopilot_run_items (run_id, item_id, role primary|duplicate)`; `autopilot_steps (run_id, step, target, state, attempt, idem_key, request_json, external_ref, error, started_at, finished_at)`, `UNIQUE(run_id, step, target)`; `autopilot_inbox (id, source_event UNIQUE, item_id, kind, at, consumed_run_id)`; `autopilot_events` (Activity log).

- Step states: `pending → sending → sent | failed | unknown | skipped`. `sending` is committed **before** the external call; the answer is committed after. Transitions are compare-and-set (`UPDATE … WHERE state = ?`), so two workers never send the same step.
- **At most once is a goal, not a promise**: `idem_key` = identity of the action — fix: `item:<id>:attempt:<n>`, push: `<branch>:<sha>`, tag/release/asset: `<tag>:<sha>[:<asset>]`, publish: `<target>:<version>:<sha256>`, reply: `<item>:<release>`, close: `<item>:<release>`. Before sending, a read-only **probe** checks whether the action already happened; a definite match → `sent` without sending. A probe error, timeout or ambiguous answer is **not** absence → `unknown` → `held` + notification "check X". Resend only when the probe proves absence *and* the platform call is safe to repeat.
- **Crash / restart**: before anything is re-run, the scheduler reconciles local facts (HEAD, branch, clean tree, commits since the manifest's base, bump commit present, worktree state). A step left `sending` → `unknown` → its probe. Agent steps left running → `failed` / `interrupted` (never auto-rerun mid-way); the fix run retries within `maxAttempts` only after the reconcile shows no commit of the interrupted attempt (else that commit is judged as a finished attempt).
- **Probes**:

| Action | Probe (read-only) |
|---|---|
| git push | `git fetch` then `merge-base --is-ancestor <sha> <remote branch>` |
| tag | `ls-remote refs/tags/vX.Y.Z^{}` (peeled) == bump sha; other sha → held `tag_conflict` |
| GitHub release / asset | `GET /releases/tags/{tag}`; asset by name with `state: uploaded`, size, and `digest` when the API returns it |
| Nexus | `PublishTargets` → version `X.Y.Z` on the target file + the upload's md5; a failed publish with an upload id resumes via `PublishRequest.UploadID` (no re-upload) |
| Factorio | `GET /api/mods/<name>/full` → `releases[]` version `X.Y.Z` with the archive's `sha1` |
| Steam | `GetPublishedFileDetails` → `time_updated` > step start and change note contains `vX.Y.Z` |
| CurseForge | the project's files list → file `X.Y.Z` with the archive's size (+ hash if exposed) |
| Reply | provider read-back (own author + body + time), as today |
| Close | issue state |

- **Event inbox**: sync events that may start fix runs are inserted into `autopilot_inbox` in the same transaction as `store.ApplyItems` (`source_event` = platform + item external id + comment external id, UNIQUE). Consuming an event and creating its fix run is one transaction. Own replies (own-author filter), the first-sync baseline, re-syncs and comment edits never create events; one open fix run per item.
- **Cancel**: before `push`, cancel drops the app's own bump commit only if HEAD == the recorded bump sha, the tree is clean and the probe proves it is not on the remote; after any public step, cancel stops the remaining steps (nothing is undone; rollback is a separate action).
- Publish tasks (`api/publish.go`) stay as the manual path; the release run calls the same `provider.Publisher` with its own persisted steps.

## Config schema (config v8)

Per code project, `agents.projects["github:owner/repo"]`:

```jsonc
"autopilot": {
  "enabled": false,             // master switch for the project (global kill switch: agents.autopilot.paused)
  "autoTriage": true,           // classify incoming items before acting
  "autoFix": false,             // implies automation allowAutoFix for this project
  "autoPush": false,            // push verified fixes to the default branch
  "autoRelease": false,         // start release runs by the coalescing timer
  "githubRelease": true,
  "publish": { "nexus:wartales/202": true, "factorio:my-mod": true, "steam:3739613434": false, "curseforge:1443010": false },
  "autoReply": false,           // post reporter replies (release links / needs-info questions) without a click
  "autoClose": true,            // close GitHub issues after the reply
  "coalesceMinutes": 60,        // wait this long after the last pushed fix …
  "maxBatchAgeHours": 24,       // … but release at most this long after the oldest unreleased fix
  "maxReleasesPerDay": 2,
  "maxDiffLines": 400,          // a bigger fix is held for review
  "publishWithoutSmoke": false,
  "regressionWindowHours": 48
},
"publishProfile": {
  "build": { "command": "pwsh -File build.ps1", "output": "dist/{name}_{version}.zip" },   // or { "path": "…" } (manual releases only)
  "steamContent": "dist/steam",                                                          // folder for steamcmd (Steam only)
  "version": { "kind": "factorio-info" },      // factorio-info | json (path, key) | regex (path, pattern) | git-tag
  "changelog": { "kind": "factorio" },         // factorio (changelog.txt) | keepachangelog (path) | commits
  "smoke": { "kind": "factorio", "save": "E:/Saves/test.zip", "ticks": 600 },  // factorio | command | none
  "targets": {
    "nexus:wartales/202": { "fileId": "…", "category": "main", "archivePrevious": true },
    "factorio:my-mod": {},
    "steam:3739613434": { "appId": 1234 },
    "curseforge:1443010": { "gameVersions": ["…"], "releaseType": "release" }
  }
}
```

Global: `agents.autopilot { paused: false, maxReleasesPerDay: 5, maxPublishesPerDay: 20 }`, events go to the `autopilot_events` table (see Activity log; no remote channel). The profile lives in the app config (not in the repo: the app must not dirty the folder, and the coder must not be able to change where or how it publishes); it holds no secrets — credentials stay in `data\secrets\*` (DPAPI). `POST /api/projects/{id}/publish-profile/check` resolves version source, changelog source, build output and targets.

## Platforms and auth

| Platform | Upload | Auth (form in Settings › Платформы) | Status |
|---|---|---|---|
| Nexus | v3 multipart upload + `POST /mod-files/{id}/versions` | API key (done, Phase 7) | done; live step 7 pending |
| Factorio | Mod Portal upload API (`init_upload` → upload URL → file POST) with a portal API key (scope *ModPortal: Upload Mods*) | API key field (`factorio/apikey.go`, in progress by another agent) | in progress |
| Steam Workshop | `steamcmd +login <user> +workshop_build_item <vdf> +quit` (VDF: `appid, publishedfileid, contentfolder, changenote`) | Login form: user + password + Steam Guard code (or mobile approval); steamcmd is downloaded from Valve's CDN on the owner's click into `data\tools\steamcmd`; password and code go to steamcmd's stdin (never argv/logs); later runs use its cached login; "Login Failure" / guard prompt → `relogin` + one notification | new |
| CurseForge | Upload API `POST /api/projects/{id}/upload-file` (multipart `file` + `metadata {changelog, changelogType, displayName, gameVersions, releaseType}`) with `X-Api-Token` | Upload API token field (authors.curseforge.com → API tokens) | new; re-check the host per game + game-version ids against current docs at build time |
| GitHub release | REST releases + asset upload | existing GitHub App token (`contents: write`) | new (no new scope) |

Every publisher implements `provider.Publisher`; Steam's "archive" is the content folder (checked against its content manifest). Credentials are validated by a read-only call when saved (as the Nexus key) and before each publish; `ErrRelogin` / bad key → step `held` with reason `auth:<platform>`, one notification, resume after the owner fixes it in the form.

## Verify gate and smoke tests

1. **Static gate** (fix runs and release runs): `aegis verify -root <dir>` when the folder has `.aegis`, else the project `verify` command (existing runner logic, 30 min). Must pass on the exact sha that is pushed / released; result stored on the step. A project with neither is **held** (`no_verify`, unread attention event), never skipped: no publish without verification (owner rule, 2026-10-05).
2. **Archive check** (release runs, all games, Phase 1): the archive opens, expected root layout (per adapter), version inside == bumped version, size within ±50 % of the previous release (outside → held).
3. **Smoke adapter** (release runs, on the built artifact), isolated temp dirs, job object, timeout 10 min:
   - `factorio`: the owner's Factorio install (auto-detected: Steam library / standalone; path overridable), temp `--mod-directory` with the archive + `mod-list.json` enabling it (dependencies copied from the owner's mods folder), then `factorio --mod-directory D --create T.zip` (data stage + map) and `factorio --mod-directory D --benchmark <copy of save or T.zip> --benchmark-ticks N` (control stage, migrations on the owner's save). Pass = exit 0 and no mod error lines in the log. Never touches the owner's real mods folder or saves. Flags re-checked against the installed version in the step.
   - `command`: any script from the profile (exit code + log tail), e.g. a batch-mode launch where a game supports it. Runs in a temp dir holding a copy of the archive; `{archive}` (quoted path), `{name}`, `{version}` are replaced.
   - `none`: allowed only with «Публиковать без смоук-теста» (default off) — otherwise the release is held.
4. **Fail** → no push of the bump, no tag, no publish; run `held`, notification with the failing step and log link.

## Safety rails

- **Kill switch**: `agents.autopilot.paused` (tray + UI; MCP can only *set* it, never clear it); per project `enabled`.
- **Serialization**: one unfinished release run per project (DB index); the folder lock is shared by fix jobs, release runs and manual pushes/publishes (one actor per checkout); daily caps are reserved in the same transaction that creates the run.
- **Caps**: existing rule caps for fix jobs; `maxReleasesPerDay` per project and global; `maxPublishesPerDay` global. Over a cap → deferred to the next window (fixes stay `pushed`).
- **Version rules**: autopilot bumps **patch only**; minor/major only on a manual release with an explicit version. The new version must be greater than the version source's value **and** every target's latest version (probed); else held `version_conflict`. Tags are never moved or reused.
- **Git (trusted push)**: the agent can edit `.git/hooks` and `.git/config` without that showing in the diff, so autopilot never pushes from the mapped folder with its hooks/config. Push, tag and build go through an **app-owned mirror** `data\mirrors\<repo>.git` (`git fetch <folder> <sha>` → verify the objects → `git push <url> <sha>:refs/heads/<branch>` from the mirror with `core.hooksPath` = an empty dir, `GIT_CONFIG_NOSYSTEM=1`, `GIT_CONFIG_GLOBAL` = an app file, credential helpers off, token via `GIT_CONFIG_*` as today). Fast-forward only, never `--force`; only the default branch; commits ahead of the remote must all be the manifest's fix commits or the bump — any other → held `foreign_commits` (never pushes the owner's own unpushed work). The manual «Push» keeps today's behaviour (the owner's hooks).
- **Diff limits**: a fix over `maxDiffLines`, touching CI/build/release scripts, the files the publish profile reads, dependency manifests, `.gitattributes`/`.gitmodules`, or deleting files → `held` for review (notification with the diff link). Issue text stays untrusted data in every prompt.
- **Control-surface capabilities**: `release`, `set_publish_profile`, clearing pause, `skip_step`, and the generic `publish` tool are refused when the caller is an agent run: the API resolves the loopback client's PID (`GetExtendedTcpTable`) and refuses processes inside any runner job object (job MCP servers and agents' shells included). An MCP/CLI release is never an owner approval: it goes through the same gate, caps and rails as autopilot.
- **Agent isolation** (owner decision 2026-10-05: risk accepted): coder runs keep the owner's Windows identity (Codex direct = `danger-full-access`), so a prompt-injected agent could in principle read DPAPI-protected secrets or call other local services. No separate Windows account. The app-owned mirror push and the agent-caller refusal on control tools close the publish path; diff limits and the gate stay. Enabling `autoFix` together with any unattended publish target shows this accepted risk once in the Autopilot panel.
- **Duplicates**: triage `duplicateOf` (same project or a linked page) → attached to the original's open fix run, no second fix; already released in the latest version → reply with that release at once.
- **Regression breaker**: trips only when triage marks `regression` for the latest released version **and** (≥ 2 distinct reporters, or one GitHub collaborator) within `regressionWindowHours`; a single anonymous report only notifies. Tripped → autopilot paused for the project, notification. **Rollback** = forward fix: `git revert` of the release's fix commits → manual release run with the next patch version (platforms never get a reused version); Nexus additionally archives the bad version. Never automatic.
- **Replies**: drafted read-only, ≤ 2000 chars, links only to the release / mod pages from the manifest; one auto-reply per item per release; platforms with `Capabilities.Reply` off → the draft waits for a click.
- **Quota**: every agent step counts against the existing caps (subscription quota).

## Activity log (replaces phone notifications)

Owner decision 2026-10-05: **no phone push** (no ntfy, no Telegram). `internal/notify` stays for transient local cards. Every autopilot action writes an event to `autopilot_events (id, at, run_id, project_id, item_id, kind, severity info|attention, title, detail_json, read_at)` (migration 016): fix pushed, release done (version + targets), reply posted, issue closed, and every `held` / `failed` step, `relogin` needed, regression pause, availability timeout as `attention`. UI: «Автопилот» entry in the top bar with an **unread counter badge** (attention events highlighted) that stays until the owner opens the log; the log lists events newest first with links to the run, item, release and platform pages; «Отметить прочитанным» / opening marks them read. SSE `autopilot.event` updates the badge live; MCP `list_autopilot_events {unreadOnly?}` for external sessions. Events are kept (newest 5000).

## UI

- **Projects › code project › «Автопилот»** panel: master switch; checkboxes «Авто-разбор», «Авто-исправление», «Авто-push», «Авто-релиз», «GitHub релиз», one per linked mod page «Загружать на Nexus / Factorio / Steam / CurseForge» (disabled with the reason: no key / no login / page not linked), «Авто-ответ», «Закрывать issue»; coalesce / batch age, caps; publish-profile editor (build command, output, version source, changelog, smoke) + «Проверить настройку» (full dry run: version, changelog preview, archive build in a temp worktree, smoke, planned requests per target; nothing public).
- **Runs** page: fix and release runs with a step timeline (state, probe result, external links), actions «Продолжить», «Отменить», «Пропустить платформу», «Откатить».
- **Settings › Платформы**: Factorio API key, CurseForge token, Steam «Вход для загрузки» (steamcmd form); ru + en.
- **«Автопилот» activity log**: top-bar entry with an unread counter badge (see Activity log).

## MCP / CLI surface

Self-describing tools (descriptions state preconditions, side effects and the refusal codes; every write has `dryRun`), through the same endpoints as the UI:

- `get_publish_profile {project}` → profile + resolved values (current version, next version, targets with their latest versions, auth state per target, smoke kind, autopilot switches) — the one call an external agent needs.
- `set_publish_profile {project, profile, dryRun}` (validated; no secrets accepted; refused for agent-run callers unless `dryRun`).
- `get_autopilot_settings {project}` / `set_autopilot_settings {project, autopilot, revision?, dryRun}` → the project's autopilot block (set refused for agent-run callers unless `dryRun`).
- `plan_release {project, version?, head?}` → the full dry-run plan.
- `release {project, version?, head?, items?, targets?, dryRun}` → starts a release run (origin `mcp`; same gate and rails; explicit `version` may be minor/major).
- `list_runs`, `get_run {id}`, `resume_run`, `cancel_run`, `skip_step {run, step}`, `pause_autopilot {project?}`.
- The generic per-platform `publish` tool (other agent, 2026-10-05) stays the low-level manual path; `release` is the orchestrated one.
- CLI: `issuewatcher release plan|run <project>`, `issuewatcher runs [--project]`, `issuewatcher run <id> [resume|cancel|skip <step>]`, `issuewatcher profile get|set <project>`.

## Phases (smallest useful slice first)

1. **Release run, manual**: migration 016, publish profile + check, version/changelog sources, release from an explicit verified HEAD (+ optional items), bump → clean-worktree build → archive check → gate → mirror push → tag → GitHub release + asset → Nexus/Factorio publish → availability, probes + reconcile + crash tests before/after every external action, serialization; UI «Выпустить релиз» + Runs page; MCP/CLI `get_publish_profile` / `plan_release` / `release` / runs with the caller check. Value: one click or one MCP call ships a mod everywhere.
2. **Smoke + activity log**: Factorio smoke adapter, `command` adapter; release held on failure; autopilot activity log with the unread badge.
3. **Fix runs + chain**: event inbox, autopilot fix prompt (`Refs #N`), fix run → trusted auto-push with git rails + diff limits, claim + coalescing timer (max batch age) → release run, replies and close steps, caps + kill switch, Autopilot panel.
4. **Auto-triage**: `classify` flow, duplicates, needs-info / question replies, regression breaker.
5. **Steam Workshop (steamcmd) + CurseForge upload**: login form + steamcmd driver, CurseForge token + uploader, probes + availability.
6. **Rollback + more smoke adapters**: «Откатить» (revert + forward release, Nexus archive), adapters for other games as feasible.

## Review (Codex, 2026-10-05)

Codex reviewed the first draft (agent-link chat `339783d9`). Agreed and folded in: coder can alter `.git/hooks`/config and pre-push hooks see the token → trusted push from an app-owned mirror; MCP/CLI must not let an agent run release/profile/unpause → caller check by PID + job object, MCP origin is not approval; serialization (one release run per project, shared folder lock, cap reservation); immutable manifest + clean-worktree build + hash re-check + Steam content manifest; honest idempotency (probe errors = unknown → held; per-action keys incl. item/commit identity); exact probes (ancestry, peeled tags, separate release/asset steps with `state: uploaded` + digest, platform hashes); local reconcile before any re-run, cancel only on exact HEAD; inbox UNIQUE + atomic consume, no loops from own replies/edits; uploaded ≠ available (availability step, replies only for available targets, skip excludes from the reply); `Fixes #N` closes before release → `Refs #N` + a close step after the reply; atomic fix→release claim and terminal outcomes for disabled steps; Phase 1 standalone from an explicit HEAD with archive checks and crash tests; batch max age; breaker needs ≥ 2 reporters or a collaborator.

Not adopted: **agent isolation** — Codex proposed a separate restricted execution identity for the coder. Same-user DPAPI cannot keep secrets from a same-user process; the owner accepted that risk (2026-10-05) and kept the mirror push + caller refusal, which close the publish path.

## Owner decisions on the open questions (2026-10-05)

4. **Agent isolation**: risk accepted — no separate Windows account; the mirror push and the agent-caller refusal on control tools stay.
5. **Notifications**: no phone push; an in-app autopilot activity log with an unread counter badge (see Activity log).
6. **Reply timing**: replies wait until every enabled (non-skipped) target shows the file as available, at most 72 h; then the step is held and an unread "needs attention" event is recorded.

## Phase 1 implementation notes (2026-10-05)

- Origins: `manual` (the owner's browser session) is not blocked by `paused` / project `enabled`; `mcp` (bearer: MCP/CLI) is refused by both.
- ~~`smoke` is recorded `skipped` until the Phase 2 adapters land; a project with neither Aegis nor a verify command records `gate` as skipped~~ — superseded in Phase 2: smoke runs its adapter; no verify command → `held` (`no_verify`).
- Daily caps count every release run created on the same UTC day (a cancelled or failed run keeps its slot).
- Nexus probe: version listed on the target file (the API exposes no md5); Factorio probe: release version + sha1.
- The 72 h availability deadline is stored on the step at its first run and survives a restart.
- `set_publish_profile` (MCP) takes only the publish profile; the autopilot block (it can switch a project on) has its own tool `set_autopilot_settings` (Phase 2), refused for agent-run callers like the other control tools.
- Activity log: events are written (held / failed / done / cancelled) and readable via `GET /api/autopilot/events` and MCP `list_autopilot_events`; the top-bar badge and log page are Phase 2.

## Phase 2 implementation notes (2026-10-05)

- **Gate without verification**: neither Aegis nor a project verify command → the `gate` step fails and the run is `held` with `no_verify` + an unread attention event (was: skipped). Resume after adding a verify command re-runs the gate.
- **Smoke step** (`internal/smoke`): kind `factorio` / `command` runs on the built archive in `data\release\<run>\smoke` (removed after); the result JSON is the step's external ref. Failure → `smoke_failed`; adapter cannot run here (no install, a flag missing, a required dependency not installed, bad save path) → `smoke_unavailable`; kind `none` / unset → `smoke_missing` unless the project's `publishWithoutSmoke` (then `skipped`). All hold before any push. «Проверить настройку» runs the same smoke on the trial archive (`smoke` in the check result).
- **Factorio adapter**: install = profile `smoke.install` (root or `factorio.exe`) else auto-detect (Steam `SteamPath` → `libraryfolders.vdf` libraries → `steamapps\common\Factorio`, then Program Files). The owner's mods folder comes from the install's `config-path.cfg` → `config.ini` `path.write-data` (else `%APPDATA%\Factorio`); it is only read. Each run writes its own `config.ini` (read-data = the install's data, **write-data = the temp dir**: log, lock and player data never touch the owner's folders), a temp `--mod-directory` with the archive + required dependencies (recursively, newest installed version; `?` / `(?)` / `!` skipped, `~` counted; `space-age` / `quality` / `elevated-rails` enabled only when required) and a `mod-list.json` enabling exactly those. `--version` + `--help` are checked first (flags `--config --mod-directory --create --benchmark --benchmark-ticks`), then `--create <temp map>` and `--benchmark <copy of the save | the new map> --benchmark-ticks N` (default 600) with `--disable-audio`, each in a job object, 10 min in total. Pass = exit 0 and no error lines (`Error …`, `Failed to load mod`, `non-recoverable error`, `Error while running`, `__mod__/file.lua:N:` traces) in stdout or `factorio-current.log`. Verified on Factorio 2.0.77 (Steam) with Lazy Builder 1.1.6 (fresh map and an owner save): create + 600-tick benchmark ≈ 1.3 s; the Steam build runs without Steam.
- **Activity log UI**: «Автопилот» in the top bar with the unread counter (red while attention events are unread), log page `/autopilot` (newest first, links to the run / project, «Отметить прочитанным»); opening it marks the loaded unread events read by id (an event arriving meanwhile stays unread). SSE `autopilot.unread {unread, attention}` after every new event and every mark-read; `POST /api/autopilot/events/read` answers the counts too.
- **Dry runs (Phase 2)**: `PUT …/publish-profile` and the new `GET/PUT /api/projects/{id}/autopilot` take `dryRun` (validated by the config store, nothing written, allowed for agent callers); MCP `set_publish_profile {dry_run}`, `get_autopilot_settings`, `set_autopilot_settings {dry_run}`.

## Phase 3 implementation notes (2026-10-05)

- **Inbox** (migration 017 adds `autopilot_inbox.outcome`): sync writes `new_issue` / `new_comment` / `new_item` events into the inbox in its own transaction (`source_event` = platform:item[:comment], UNIQUE) only for code projects with `enabled` + `autoFix` and while not paused (`Store.SetInboxFilter`). Baseline, own items / comments, re-syncs and comment edits never produce events. The autopilot loop (engine, every 30 s + a kick after each sync) consumes them: an open fix run of the item (pending / running / held / pushed) gets the event attached; otherwise a fix run starts when an enabled **fix rule** of the project matches the item (Phase 3 has no triage: the rules decide, as with autoTriage off); a comment on an item autopilot already fixed, a closed item, no rule, autopilot off → dropped with the reason in `outcome`. Automation fix rules skip such projects (`autopilot` reason in the decision log).
- **Fix run** = steps `fix → verify → push`. `fix` queues the direct fix job with rule id `autopilot` (rule origin: the automation caps and `maxAttempts`; over a cap → waits for the next window); the prompt asks for `Refs #N`, never a closing keyword; facts record `refsRef`. Fix runs of one project go one at a time (the next job starts once the previous fix is pushed), so each verify / push sees only its own commits. Held: `closing_keyword` (a commit says Fixes #N), `needs_info`, `not_reproduced`, `no_commit`, `fix:<reason>`; a failed job is retried until `maxAttempts`.
- **Verify** = the gate on the fix head in the folder (default branch, HEAD = fix head, clean; `no_verify` / `gate_failed`), then diff limits: `diff_too_big` (> `maxDiffLines` added + deleted), `diff_review` (deleted files; CI dirs `.github/` etc.; build / release scripts `*.ps1 *.bat *.cmd *.sh`, `build.*`, `release.*`, `publish.*`, Makefile…; dependency manifests; `.gitattributes` / `.gitmodules`; the profile's version and changelog files).
- **Push**: `autoPush` on → from the app mirror (hooks off, isolated config), fast-forward only, every commit ahead of the remote must be the fix's own (`foreign_commits` / `remote_moved`); probe before and after (crash → probe → never twice). Off → the run waits for the owner's «Push» (attention event `fix.verified`). Pushed → the job is done (`local.pushed`), event `fix.pushed`. Paused / project off → the push waits.
- **Coalescing timer**: a project with `autoRelease` releases its pushed, unclaimed fix runs `coalesceMinutes` after the newest or `maxBatchAgeHours` after the oldest: release run origin `auto`, items = the fixes' items, fix runs claimed in the create transaction (all or none). Release done → fix runs `released`; cancelled before any public step → unclaimed.
- **Plan refusals up front** (owner decision 2026-10-05): no verify gate (`no_verify`) or no smoke test while «Публиковать без смоук-теста» is off (`smoke_missing`) refuse the plan: no run, no daily slot; a non-dry-run start writes one unread `release.refused` attention event (repeated only after it was read). The timer's other refusals (dirty folder, head not on the remote…) are recorded the same way; busy / cap refusals wait quietly.
- **Reply / close steps** (release run, after every `available`): `reply:<item>` per manifest item when `autoReply` is on and the platform replies; it is refused (`reply_waiting`, held) while a target is neither available nor skipped (availability waits ≤ 72 h, then held + attention). The reply is a template (no agent): `Fixed in vX.Y.Z.` + the GitHub release + every available target's page, ≤ 2000 chars, an invisible marker on GitHub. Probe: never sent → absent; else read-back (GitHub: the issue's comments live; other platforms: the synced comments, not found = unclear → held). `close:<item>` (GitHub issues, `autoClose`) closes as completed (probe: issue state). Items without an automatic reply → attention event `release.replies_pending`. `skip_step` takes reply / close steps too.
- **CLI parity**: `issuewatcher autopilot get|set <project> [--dry-run]`, `profile set … --dry-run`.
- Verified with fake platforms (`internal/release/fix_test.go`): issue → fix → verify → push → release → reply → close; crash after the fix push and after the reply → restart sends nothing twice; own reply / reporter's thanks start nothing; closing keyword, diff limit, kill switch.
