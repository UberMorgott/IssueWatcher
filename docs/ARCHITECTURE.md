# IssueWatcher — Architecture

Portable Windows desktop hub: track issues/comments across all my projects and platforms
(GitHub first; later CurseForge, Nexus Mods, Steam Workshop), reply from one place, and
dispatch local AI agents (Claude Code, Codex CLI) manually, in batches, or by auto rules.

## Decisions

| Area | Decision | Why |
|---|---|---|
| Shell | single Go exe: loopback HTTP server (`127.0.0.1`, OS-chosen free port; optional `IW_PORT` only if free) serving the embedded Vue app, opened in the default browser; no embedded webview | no WebView2 profile, no Wails beta; user runs many local servers, so never a fixed port |
| Loopback auth | per-run bearer token + port in `data\runtime.json`; browser always opened via one-time `/auth?t=` link → `HttpOnly SameSite=Strict` session cookie; Host header must be `127.0.0.1`/`localhost` | other local processes/pages can't drive the API; DNS-rebinding guard |
| Single instance | named mutex `Local\IssueWatcher-<hash(data dir)>`; second launch reads `runtime.json`, `POST /api/open` → running instance opens the browser, exits | portable copies in different folders stay independent; mutex dies with the process (no stale lock) |
| Portable | all state in `data\` next to exe (config, SQLite, logs, worktrees). No registry/AppData | user requirement |
| Notifications | tray balloon (`Shell_NotifyIcon` `NIF_INFO`); click (`NIN_BALLOONUSERCLICK`) opens browser at `/item/:id`. Win10/11 render it as a toast | no AUMID/shortcut/registry needed (portable); `fyne.io/systray` exposes neither balloons nor that click → own small Win32 tray (`golang.org/x/sys/windows`) |
| Tray | own Win32 tray: PNG icon rendered in Go with badge count (`99+`) (+ status dots later); menu Open dashboard / Test notification / Quit; left-click opens browser; re-added on `TaskbarCreated` | quick status |
| DB | SQLite via `modernc.org/sqlite` (no cgo), embedded SQL migrations | reuse `E:\DEV\1сEPD\internal\store\sqlite.go` |
| GitHub API | GraphQL (explicit queries over `net/http`) for sync; REST (`go-github`) where simpler | rate limit 5000/h per token |
| GitHub auth | GitHub App created by **manifest flow** (browser: confirm create → install on all repos) → user token via web flow + PKCE + loopback `127.0.0.1:<port>`. Client secret stays in local `data\` (generated for this user, never shipped). Fallback: device flow | "click Login → Accept, Accept → works", no shipped secret, no broker |
| Agents | local `claude -p` / `codex exec` child processes, Windows Job Object for cancel, streamed logs | user uses local subscriptions only |
| Publishing | dispatcher publishes (comments, labels, push, draft PR) via provider; agent only proposes | least privilege; merge always manual |
| AI control | same exe in MCP stdio mode (`modelcontextprotocol/go-sdk`) talking to running core over loopback+token | "tell Claude: reply to X" |
| Frontend | Vue 3 + Vite + TypeScript + Pinia + PrimeVue (DataTable) + ECharts; `//go:embed` bundle, history routing with server-side SPA fallback | table with filters/checkbox, charts |

## Layout

```text
cmd/issuewatcher        entry: desktop (default), `mcp` (stdio), CLI subcommands
internal/domain         Source, Project, Item, Thread, Comment, Rule, Job, AgentProfile, Prompt
internal/provider       Provider interface + capability flags
internal/provider/github
internal/sync           polling, cursors, reconciliation, stats snapshots
internal/runner         job queue, worktrees, claude/codex adapters, result check
internal/store          SQLite + migrations
internal/api            loopback HTTP server: SPA, JSON API, auth (browser + MCP bridge)
internal/notify         Win32 tray icon, badge rendering, balloon notifications
internal/instance       single-instance mutex + data\runtime.json
internal/paths          portable data dir resolver (IW_DATA_DIR override)
frontend/               Vue app
data/                   runtime (gitignored): issuewatcher.db, runtime.json, logs\
```

## Build & dev switches

- `pwsh -File build.ps1` → `build\bin\issuewatcher.exe` (npm build + `go build -H windowsgui`).
- Env: `IW_DATA_DIR` (data dir override), `IW_PORT` (preferred port, used only if free), `IW_NO_BROWSER=1` (log launch URLs instead of opening), `IW_DEMO=1` (scripted badge + notification + simulated click, for log-based verification).
- Fatal panics go to `data\logs\issuewatcher.log` (`debug.SetCrashOutput`); startup errors also show a MessageBox.

## Domain

- `Source` — connected account on a platform (github:UberMorgott). Tokens stored in `data\`.
- `Project` — platform object (repo / mod page) + optional local folder mapping. Auto-discovery scans configured roots for git remotes.
- `Item` — issue or equivalent, key `(source, external_id)`; unified status + raw platform status.
- `Thread` / `Comment` — discussions with platform IDs; replies go through provider.
- `Rule` — match (project, labels, platform, kind) → flow (`fix`, `reply`, `verify`, `label`) + agent profile + limits; auto or suggest.
- `Job` — one per item per flow; states queued/running/needs_review/done/failed/cancelled; attempts, logs, result.
- `AgentProfile` — CLI (`claude`/`codex`), model, role (coder/responder/verifier), flags, limits.
- `Prompt` — global layer + per-project layer + per-flow template.

## Provider interface

Each provider declares capabilities: `ListProjects`, `SyncItems`, `ListComments`, `Reply`,
`SetLabels`, `SetStatus`, `CreatePR`, `Auth` (kind: oauth-loopback / device / sso-websocket / cookie-session / api-key).
UI disables actions a provider lacks. Compiled adapters, no dynamic plugins.

Known platform facts (2026-09 research, re-verify before building each):
- **CurseForge**: official APIs have no comments endpoint; comments only via session cookies (own `E:\DEV\curseforge` MCP server does it).
- **Nexus Mods**: SSO websocket `wss://sso.nexusmods.com` returns API key; no known public comments/bugs API → likely webview session.
- **Steam Workshop**: `ICommunityService/GetCommentThread`/`PostCommentToThread` (undocumented) or community endpoints with `steamLoginSecure`+`sessionid`; login via embedded webview to capture session.

## Runner

- Batch = N jobs (one per item). Concurrency: 1 per project, 1–2 global (configurable).
- Worktree per job created from the mapped local repo into `data\worktrees\<project>\<job>`; user's working copy untouched.
- Result check = actual diff + configured verify commands + git status, not the agent's claim.
- Auto mode only for explicit allow rules; limits on time, attempts, parallelism; issue text treated as untrusted (prompt injection).

## Phases

0. Scaffold + shell spike: portable data dir, loopback server + browser UI, tray badge, balloon click → dashboard item route, single instance.
1. GitHub: login, sync repos/issues/comments, table (filters, checkbox), reply, folder mapping, stats, tray/notifications.
2. Jobs: manual + batch `fix`, agent profiles, log stream, cancel, verify, draft PR.
3. Automation: `verify`/`reply`/`label` flows, prompts (global/project), rules, limits, history.
4. MCP/CLI control; providers CurseForge, Nexus Mods, Steam Workshop.
