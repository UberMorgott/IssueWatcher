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
| GitHub auth | GitHub App created by **manifest flow** (browser: confirm create → install on all repos) → user token via web flow + PKCE + loopback `127.0.0.1:<port>`. Client secret stays in local `data\secrets\` (generated for this user, never shipped). Fallback: device flow. Details + sources: [GitHub auth](#github-auth) | "click Login → Accept, Accept → works", no shipped secret, no broker |
| Agents | local `claude -p` / `codex exec` child processes, Windows Job Object for cancel, streamed logs | user uses local subscriptions only |
| Publishing | dispatcher publishes (comments, labels, push, draft PR) via provider; agent only proposes | least privilege; merge always manual |
| AI control | same exe in MCP stdio mode (`modelcontextprotocol/go-sdk`) talking to running core over loopback+token | "tell Claude: reply to X" |
| Frontend | Vue 3 + Vite + TypeScript + Pinia + PrimeVue (DataTable) + ECharts; `//go:embed` bundle, history routing with server-side SPA fallback | table with filters/checkbox, charts |
| Language | UI Russian by default, English switch in Settings (`localStorage iw.lang`): `vue-i18n` (Composition API, Slavic plural rule), PrimeVue texts from `primelocale`, dates/relative time via `Intl`. Go-side text (tray menu, balloons, auth pages) is Russian only | user request; no server round trip for a UI preference |

## Layout

```text
cmd/issuewatcher        entry: desktop (default), `mcp` (stdio), CLI subcommands
internal/domain         (later) Rule, Job, AgentProfile, Prompt; phase 1 records live in provider + store
internal/provider       Provider interface, capability flags, platform-neutral Project/Item/Comment
internal/provider/github  auth (manifest, web flow+PKCE, device, refresh), REST/GraphQL client, sync queries
internal/provider/github/githubtest  in-memory fake GitHub for tests (no network)
internal/syncer         poller: cursors, diff → events, unread badge, replies (named syncer: avoids clash with std sync)
internal/secret         owner-only JSON files (protected DACL = current user) for app keys + tokens
internal/config         data\config.json (pollIntervalMinutes)
internal/runner         job queue, worktrees, claude/codex adapters, result check
internal/store          SQLite + migrations
internal/api            loopback HTTP server: SPA, JSON API, auth (browser + MCP bridge)
internal/notify         Win32 tray icon, badge rendering, balloon notifications
internal/instance       single-instance mutex + data\runtime.json
internal/paths          portable data dir resolver (IW_DATA_DIR override)
frontend/               Vue app
data/                   runtime (gitignored): issuewatcher.db, runtime.json, config.json, logs\, secrets\
```

## Build & dev switches

- `pwsh -File build.ps1` → `build\bin\issuewatcher.exe` (npm build + `go build -H windowsgui`).
- Env: `IW_DATA_DIR` (data dir override), `IW_PORT` (preferred port, used only if free), `IW_NO_BROWSER=1` (log launch URLs instead of opening), `IW_DEMO=1` (scripted badge + notification + simulated click, for log-based verification).
- Fatal panics go to `data\logs\issuewatcher.log` (`debug.SetCrashOutput`); startup errors also show a MessageBox.

## GitHub auth

Verified against GitHub docs 2026-09-28. Flow as built (`internal/api/github_auth.go`, `internal/provider/github/auth.go`):

1. Dashboard "Войти" → `POST /api/auth/github/start` (server opens the default browser itself). No app yet → local page `/auth/github/manifest?state=…` auto-POSTs form field `manifest` to `https://github.com/settings/apps/new?state=…`.
   Manifest: name `IssueWatcher-<8 hex>`, `public:false`, permissions issues/pull_requests/contents `write` + metadata `read`, `default_events: []`, webhook `active:false` (polling), `redirect_url` = `http://127.0.0.1:<port>/auth/github/app-created`, `setup_url` = `…/auth/github/setup`, `callback_urls` = [`http://127.0.0.1:<port>/auth/github/callback`, `http://127.0.0.1/auth/github/callback`].
2. User clicks **Create GitHub App** → GitHub redirects to `redirect_url?code&state` → `POST https://api.github.com/app-manifests/{code}/conversions` (no auth, 201; `id`, `slug`, `client_id`, `client_secret`, `webhook_secret`, `pem`; code valid 1 h) → stored in `data\secrets\github-app.json` with the registered port → redirect to `https://github.com/apps/<slug>/installations/new`.
3. User clicks **Install** (all repositories) → GitHub → `setup_url` → redirect to `https://github.com/login/oauth/authorize` with `client_id`, `redirect_uri` (current port), `state`, `code_challenge` (S256).
4. User clicks **Authorize** → `/auth/github/callback?code&state` → `POST https://github.com/login/oauth/access_token` (`client_id`, `client_secret`, `code`, `redirect_uri`, `code_verifier`) → `ghu_` token (8 h) + `ghr_` refresh token (6 months) → `data\secrets\github-token.json` → sync triggered, `auth.changed` published; if a dashboard tab is on `/api/events` it gets `navigate` (focus) and the GitHub tab shows "you can close this tab", else a fresh launch URL opens the dashboard.
   Later runs: "Войти" skips 1–3 and goes straight to 4 (one click, or none if GitHub remembers the grant).

- Refresh: `grant_type=refresh_token` 5 min before expiry, serialized (a used refresh token and its access token die). OAuth error on refresh, dead refresh token or API 401 → token deleted → signed out (app kept). Non-expiring tokens (user opted out) used as is.
- The three GitHub redirect paths skip the session-cookie check (cross-site navigations never carry the `SameSite=Strict` cookie); app-created/callback act only on our single-use in-memory `state` (TTL 1 h / 10 min); setup only starts a new authorize round trip. Host check still applies.
- Secrets: `data\secrets\` dir + files get a protected DACL granting only the current user; contents never logged. No secret in the binary, no broker.
- **Loopback port (open risk)**: the OAuth-apps doc says a loopback `redirect_uri` "does not need to match the port specified in the callback URL"; the GitHub-Apps callback doc is silent (only "up to 10 callback URLs", `redirect_uri` selects one). Not verifiable without a live app. Mitigations as built: (a) both exact-port and port-less `127.0.0.1` callbacks registered; (b) the registered port is remembered and preferred on later runs (like `IW_PORT`: only if free); (c) device flow fallback `POST /api/auth/github/device` (opens `verification_uri`, UI shows/copies `user_code`), which needs **Enable Device Flow** ticked once in the app settings (manifest has no field for it).
- Logout (`POST /api/auth/github/logout`) only forgets the local token; revoke at <https://github.com/settings/apps/authorizations> if needed.

Sources:

- <https://docs.github.com/en/apps/sharing-github-apps/registering-a-github-app-from-a-manifest>
- <https://docs.github.com/en/rest/apps/apps#create-a-github-app-from-a-manifest>
- <https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/generating-a-user-access-token-for-a-github-app>
- <https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/refreshing-user-access-tokens>
- <https://docs.github.com/en/apps/creating-github-apps/registering-a-github-app/about-the-user-authorization-callback-url>
- <https://docs.github.com/en/apps/oauth-apps/building-oauth-apps/authorizing-oauth-apps> (loopback rule)
- <https://docs.github.com/en/rest/apps/installations> (`GET /user/installations`, `/user/installations/{id}/repositories`, per_page ≤ 100)

## GitHub sync

- Repos: REST `GET /user/installations` → `GET /user/installations/{id}/repositories` (user token ∩ app installation). Repos no longer listed → `projects.active = 0` (hidden, data kept).
- Issues: GraphQL `repository.issues(first: 50, orderBy: UPDATED_AT ASC, filterBy: {since})` (issues connection has no PRs) with labels + comments (100/page, extra pages via `node(id)`). Cursor = max `updatedAt` per project (`projects.sync_cursor`); `since` is inclusive, upserts are idempotent.
- Poller (`internal/syncer`): immediately at start, then every `pollIntervalMinutes` from `data\config.json` (default 5, min 1), plus `POST /api/sync` and after sign-in. One cycle at a time.
- Rate limit: `x-ratelimit-remaining/reset` headers (REST + GraphQL); sync stops below 50 remaining until reset (replies may use the reserve); 403/429 with `Retry-After` or remaining 0 → rate-limited status.
- Diff → events (`store.ApplyItems`): new issue, new comment, issue open→closed. First sync of a project = silent baseline; own issues/comments never notify. Events mark the item `unread`.
- Tray: 1 event → balloon (click → `/item/<id>`); more → one summary balloon (click → dashboard) because the tray keeps one click target. **Badge = number of unread items**; `POST /api/items/{id}/read` clears. Sync events are mirrored to open tabs as SSE (`cmd/issuewatcher/live.go`).

## HTTP API

JSON over the loopback server; session cookie or bearer required (except the GitHub redirect paths above). Legacy aliases stay until the SPA drops them: `/api/repos` = `/api/projects`, `/api/issues…` = `/api/items…`, `repo=` = `project=`, `per_page=` = `per=`, `POST /api/auth/{start,logout,device}` (start there only returns the URL).

- `GET /api/auth/status` → `{providers:[{id, name, connected, login, avatarUrl, setupNeeded, state, error?}], …legacy GitHub fields (app, appSlug, installUrl, signedIn, login, device{pending,userCode,verificationUri,error})}`.
  `state`: `not_configured` (no GitHub App yet; first start creates it) · `disconnected` · `connecting` (browser round trip or device poll pending) · `connected` · `error` · `unavailable` (curseforge, nexus, steam: listed, not implemented).
- `POST /api/auth/{provider}/start` → `{step: create_app|authorize, url, opened: true}`; the server opens the default browser. Non-github: 501 (known) / 404.
- `POST /api/auth/{provider}/logout` → status; `POST /api/auth/github/device` → `{userCode, verificationUri, expiresIn, interval}` (server opens `verificationUri`; UI shows/copies `userCode`; completion → `auth.changed`).
- `GET /api/projects` → `[{id, name, url, platform, open, closed, unread, localPath, lastSync}]`.
- `GET /api/items?source=&project=&state=open|closed|all&label=&q=&unread=1&page=&per=` → `{total, page, perPage, items:[{id, repoId, repo, number, title, url, author, state, rawStatus, labels, comments, unread, createdAt, updatedAt, closedAt}]}`; `source` = platform id, `q` = title/body substring or `#N`, 50/page, max 200, newest update first.
- `GET /api/items/{id}` → item + `body` + `commentsList:[{id, author, body, url, createdAt, updatedAt}]`; `POST /api/items/{id}/read` → 204 (badge refresh).
- `POST /api/items/{id}/comments {body}` → 201 comment; 400 empty, 404, 409 not signed in, 429 rate limited, 502 GitHub error.
- `GET /api/stats?project=&weeks=` → `{open, closed, weekly:[{start, opened, closed}], projects?}` (Monday-start UTC weeks, default 26; `projects` = per-project totals when `project` omitted).
- `GET /api/sync` → `{running, signedIn, lastSync, lastError, rateLimitedUntil, interval}`; `POST /api/sync` → 202.
- `GET /api/events` (SSE, owned by the SPA/tray side, `internal/api/events.go`): `item.new`, `comment.new`, `item.closed` `{id, repo, number, title, actor?, body?}`, `sync.status {state: started|progress|done|error, repo?, done, total, changed, unread, error?}` (every cycle, first/silent sync included), `data.changed {reason: sync|read|reply, itemId?, repo?}` (any write that changes items, comments, projects or read state), `auth.changed {provider, state, login}`, `navigate {path}`.
- SPA reactivity: one store (`frontend/src/stores/app.ts`) turns `data.changed` / `auth.changed` / item events into a debounced (300 ms) `dataVersion` bump; every view refetches quietly on it (no loading overlay; selection, filters, scroll kept). SSE reconnect = full refetch. The top bar shows `sync.status` progress (project N/M).
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
