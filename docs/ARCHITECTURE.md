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
| Portable | all state in `data\` next to exe (config, SQLite, logs, worktrees). No registry/AppData — **one exception**: Settings → «Запускать вместе с Windows» writes `HKCU\Software\Microsoft\Windows\CurrentVersion\Run\IssueWatcher` = `"<exe>" --minimized` (`internal/autostart`), only while on, deleted when off, rewritten at startup if the exe moved (`config.json general.startWithWindows` remembers the choice) | user requirement; autostart has no portable alternative |
| Start / focus | `--minimized` (or Settings → «Запускать свёрнутым», flag wins) = tray only, no browser tab. Tray click (double-click debounced 400 ms), «Открыть», notification click, second launch: `EnumWindows` finds the browser window whose title starts with the locale-independent marker `IssueWatcher · ` (the SPA's `document.title`), restores + `SetForegroundWindow`s it (foreground rights come from the click; second launch passes them via `AllowSetForegroundWindow`; `AttachThreadInput` fallback) and steers the tab with SSE `navigate`. Not found (background tab, other window) → a new tab opens and the old tabs get SSE `superseded` (+ `BroadcastChannel` from the new tab): dimmed overlay «Дашборд открыт в другой вкладке» with «Работать здесь», live updates and the title marker dropped. `IW_DEBUG=1` logs tray callbacks (hover/mouse-move callbacks never). Web app manifest lets the user install the dashboard as an app window, which makes the match reliable | browsers ignore `window.focus()` from a background tab |
| Notifications | the app's own popup cards (`internal/notify/popup_windows.go`), not Windows toasts/balloons: one borderless topmost `WS_EX_TOOLWINDOW` + `WS_EX_NOACTIVATE` layered window per card (no taskbar button, never takes focus), bottom-right of the primary monitor's work area (top-right with a top taskbar), per-monitor DPI v2. Card rendered in Go (`golang.org/x/image` + embedded Inter, SIL OFL, `internal/notify/fonts/OFL.txt`) into premultiplied RGBA → `UpdateLayeredWindow` (rounded corners, soft shadow, slide/fade). Kind icon (issue / comment / closed ✓), title, `repo#N`, 2-line snippet, time, ×. Stack ≤ 3: overflow folds into «+N ещё». Auto-hide (`Prefs.AutoHide`, default 8 s), hover pauses; click → `/item/:id` (group → `/issues?unread=1`) through the tray focus/new-tab logic. `Prefs` (types, muted repos, quiet hours, `RespectWindowsDnd` → `SHQueryUserNotificationState`) and `Theme` come from settings | Windows swallows balloons/toasts under Do Not Disturb / Focus assist and never reports clicks from the notification center; no AUMID/registry (portable); no webview |
| Tray | own Win32 tray: PNG icon rendered in Go with badge count (`99+`) (+ status dots later); menu Открыть / Тестовое уведомление / Выход; left-click opens browser; re-added on `TaskbarCreated` | quick status |
| DB | SQLite via `modernc.org/sqlite` (no cgo), embedded SQL migrations | reuse `E:\DEV\1сEPD\internal\store\sqlite.go` |
| GitHub API | GraphQL (explicit queries over `net/http`) for sync; REST (`go-github`) where simpler | rate limit 5000/h per token |
| GitHub auth | GitHub App created by **manifest flow** (browser: confirm create → install on all repos) → user token via web flow + PKCE + loopback `127.0.0.1:<port>`. Client secret stays in local `data\secrets\` (generated for this user, never shipped). Fallback: device flow. Details + sources: [GitHub auth](#github-auth) | "click Login → Accept, Accept → works", no shipped secret, no broker |
| Agents | local `claude -p` / `codex exec` child processes, Windows Job Object for cancel, streamed logs | user uses local subscriptions only |
| Publishing | dispatcher publishes (comments, labels, push, draft PR) via provider; agent only proposes | least privilege; merge always manual |
| AI control | same exe in MCP stdio mode (`modelcontextprotocol/go-sdk`) talking to running core over loopback+token | "tell Claude: reply to X" |
| Frontend | Vue 3 + Vite + TypeScript + Pinia + PrimeVue (DataTable) + ECharts; `//go:embed` bundle, history routing with server-side SPA fallback | table with filters/checkbox, charts |
| Language | UI Russian by default, English switch in Settings (`localStorage iw.lang`): `vue-i18n` (Composition API, Slavic plural rule), PrimeVue texts from `primelocale`, dates/relative time via `Intl`. Go-side text (tray menu, popup cards, auth pages) is Russian only | user request; no server round trip for a UI preference |

## Layout

```text
cmd/issuewatcher        entry: desktop (default), `mcp` (stdio), CLI subcommands
internal/domain         (later) Rule, Job, AgentProfile, Prompt; phase 1 records live in provider + store
internal/provider       Provider interface, capability flags, platform-neutral Project/Item/Comment
internal/provider/github  auth (manifest, web flow+PKCE, device, refresh), REST/GraphQL client, sync queries
internal/provider/github/githubtest  in-memory fake GitHub for tests (no network)
internal/syncer         poller: cursors, diff → events, unread badge, replies (named syncer: avoids clash with std sync)
internal/secret         owner-only JSON files (protected DACL = current user) for app keys + tokens
internal/config         data\config.json: typed sections, schemaVersion + migration, revision, validation, atomic write + .bak
internal/folders        repo ↔ local clone: git remote parsing, folder status, discovery under root folders
internal/runner         job queue, worktrees, claude/codex adapters, result check
internal/store          SQLite + migrations
internal/api            loopback HTTP server: SPA, JSON API, auth (browser + MCP bridge)
internal/notify         Win32 tray icon, badge rendering, own popup notification cards (layered windows)
internal/autostart      HKCU Run entry for «start with Windows» (the only registry write)
internal/instance       single-instance mutex + data\runtime.json
internal/paths          portable data dir resolver (IW_DATA_DIR override)
frontend/               Vue app
data/                   runtime (gitignored): issuewatcher.db, runtime.json, config.json, logs\, secrets\
```

## Build & dev switches

- `pwsh -File build.ps1` → `build\bin\issuewatcher.exe` (npm build + `go build -H windowsgui`).
- Env: `IW_DATA_DIR` (data dir override), `IW_PORT` (preferred port, used only if free), `IW_NO_BROWSER=1` (log launch URLs instead of opening), `IW_DEMO=1` (scripted badge + four popup cards + simulated click, for log-based verification), `IW_POPUP_SNAPSHOT=<dir>` (render sample popup PNG files — dark/light single, closed, stack — at 150 % and exit, no window).
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
- Poller (`internal/syncer`): immediately at start, then every GitHub `activeMinutes` of the sync plan (Settings → Синхронизация; balanced 5 min, fast 2, custom 1–120), changed live, plus `POST /api/sync` and after sign-in. One cycle at a time.
- Rate limit: `x-ratelimit-remaining/reset` headers (REST + GraphQL); sync stops below 50 remaining until reset (replies may use the reserve); 403/429 with `Retry-After` or remaining 0 → rate-limited status.
- Diff → events (`store.ApplyItems`): new issue, new comment, issue open→closed. First sync of a project = silent baseline; own issues/comments never notify. Events mark the item `unread`.
- Popups: one card per event (click → `/item/<id>`); beyond 3 cards the oldest fold into «+N ещё» (click → `/issues?unread=1`). **Badge = number of unread items**; `POST /api/items/{id}/read` clears. Sync events are mirrored to open tabs as SSE (`cmd/issuewatcher/live.go`).

## HTTP API

JSON over the loopback server; session cookie or bearer required (except the GitHub redirect paths above). Legacy aliases stay until the SPA drops them: `/api/repos` = `/api/projects`, `/api/issues…` = `/api/items…`, `repo=` = `project=`, `POST /api/auth/{start,logout,device}` (start there only returns the URL). Page/offset pagination is gone: lists load as keyset chunks (infinite scroll).

- `GET /api/auth/status` → `{providers:[{id, name, connected, login, avatarUrl, setupNeeded, state, error?}], …legacy GitHub fields (app, appSlug, installUrl, signedIn, login, device{pending,userCode,verificationUri,error})}`.
  `state`: `not_configured` (no GitHub App yet; first start creates it) · `disconnected` · `connecting` (browser round trip or device poll pending) · `connected` · `error` · `unavailable` (curseforge, nexus, steam: listed, not implemented).
- `POST /api/auth/{provider}/start` → `{step: create_app|authorize, url, opened: true}`; the server opens the default browser. Non-github: 501 (known) / 404.
- `POST /api/auth/{provider}/logout` → status; `POST /api/auth/github/device` → `{userCode, verificationUri, expiresIn, interval}` (server opens `verificationUri`; UI shows/copies `userCode`; completion → `auth.changed`).
- `GET /api/projects` → `[{id, name, url, platform, open, closed, unread, localPath, lastSync}]` (whole list: filters, badge); `?sort=name|open|closed|unread|lastSync&dir=asc|desc&q=&cursor=&limit=` → keyset chunk `{items, nextCursor, more, total}` (Projects page).
- `GET /api/items?source=&project=&state=open|closed|all&label=&q=&unread=1&cursor=|after=|ids=&limit=` → keyset chunk `{items:[{id, repoId, repo, number, title, url, author, state, rawStatus, labels, comments, unread, createdAt, updatedAt, closedAt}], headCursor, nextCursor, more, total?}`, order `updated_at DESC, id DESC` (index `items_updated_id`); `cursor` = next (older) chunk, `after` = rows newer than the head (live refresh; `more` = too many, start over), `ids` = re-check loaded rows against the filter (≤ 500); `total` (cheap count) on the first chunk and `after`; `limit` default 50, max 200; `source` = platform id, `q` = title/body substring or `#N`. Cursors are opaque (base64url JSON `[sortValue, id]`).
- `GET /api/items/{id}` → item + `body`; `GET /api/items/{id}/comments?cursor=&limit=` → `{items:[{id, author, body, url, createdAt, updatedAt}], nextCursor, more}` oldest first (`nextCursor` also finds comments added later); `POST /api/items/{id}/read` → 204 (badge refresh).
- `POST /api/items/{id}/comments {body}` → 201 comment; 400 empty, 404, 409 not signed in, 429 rate limited, 502 GitHub error.
- `GET /api/stats?project=&weeks=` → `{open, closed, weekly:[{start, opened, closed}], projects?}` (Monday-start UTC weeks, default 26; `projects` = per-project totals when `project` omitted).
- `GET /api/sync` → `{running, signedIn, lastSync, lastError, rateLimitedUntil, interval}`; `POST /api/sync` → 202.
- `GET /api/settings` → `{revision, settings, info}`: `settings` = the whole `config.json` document (sections `general` (language, startWithWindows, startMinimized), `appearance`, `notifications` (enabled, per-kind switches, mutedProjects, quiet hours, group, autoHideSeconds, respectDnd), `sync` (mode balanced/fast/custom, activeDays, per-provider plans), `projects` (roots, exclude, scanDepth)); `info` = data dir, config path, version, exe, sync presets. `general.startWithWindows` shows the Run value (the truth).
- `PATCH /api/settings {revision, patch}`: `patch` is a JSON merge patch (RFC 7396) of `settings`. 409 `{error, current}` when `revision` is stale (another tab saved first; the SPA re-applies once on top of `current`), 400 `{field, code, params, error}` when validation fails (codes: json, enum, range, time, differ, absPath, tooMany, section; the SPA localises them). On success: side effects first (Run entry), then atomic write (temp + fsync + rename, previous file → `config.json.bak`), live apply (popup prefs/theme, toast filter, sync plan) and SSE `settings.changed` (the new doc) to every tab. `POST /api/settings/reset {revision, section}` restores one section's defaults; `GET /api/settings/export` downloads the settings. Secrets and the runtime token never live in `config.json`.
- Appearance (`settings.appearance`): `mode` dark/light/system, `paletteId` (presets in `info.palettes`: four base colours accent/background/surface/text per mode), `custom.{dark,light}` overrides (empty = palette), `fontFamily`, `fontScale` 0.85–1.30, `density`. The server rejects bad hex and any combination whose text contrast on surface or background is below WCAG AA 4.5:1 (code `contrast`). The SPA (`lib/appearance.ts`) derives every `--iw-*` token from the four colours, updates PrimeVue via `updatePreset` (`palette(accent)`, surface scale, datatable padding), re-themes ECharts and caches the result in `localStorage iw.paint` for the first paint; popups use the same colours (`cmd/issuewatcher popupTheme`).
- `config.json` v2 (`schemaVersion: 2`): v1's flat `startWithWindows`/`startMinimized` move to `general`; v1 `pollIntervalMinutes` N ≠ 5 becomes sync mode `custom` with GitHub `activeMinutes = N` (5 = the balanced default). Unknown keys (nested too) survive every write.
- `GET /api/folders` → `[{projectId, name, url, platform, localPath, status}]`, status `none|ok|missing|notGit|mismatch` (`ok` = a git clone whose remote is the project; linked worktrees resolved via the `.git` file + `commondir`); `PUT /api/projects/{id}/path {path}` (absolute, empty unmaps) → row; `POST /api/folders/discover` scans `settings.projects.roots` (depth `scanDepth`, skipping `exclude` names, hidden folders and clone interiors, ≤ 20000 folders, 30 s) → `{suggestions:[{projectId, name, path, remote}], visited, roots}`; nothing is mapped until the user accepts.
- `POST /api/notifications/test` → 204, shows a sample popup.
- `GET /api/events` (SSE, owned by the SPA/tray side, `internal/api/events.go`): `item.new`, `comment.new`, `item.closed` `{id, repo, number, title, actor?, body?}`, `sync.status {state: started|progress|done|error, repo?, done, total, changed, unread, error?}` (every cycle, first/silent sync included), `data.changed {reason: sync|read|reply, itemId?, repo?}` (any write that changes items, comments, projects or read state), `auth.changed {provider, state, login}`, `settings.changed` (settings doc), `navigate {path}`.
- Infinite lists (no paginator): Issues = TanStack Virtual over a fixed 60 px row (only rows entering the view mount; 60 fps on 3k rows), next chunk of 50 when within 2 screens of the loaded end, skeleton rows at the tail; live `after` refresh prepends and keeps the viewport anchored (+ «N новых» pill), `ids` re-check patches/removes rows in place; selection survives. Item comments and Projects load chunks via an IntersectionObserver sentinel ~2 screens ahead.
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
