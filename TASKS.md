# TASKS

See `docs/ARCHITECTURE.md` for decisions and phases.

## Phase 0 — scaffold + shell spike
- [x] git init, .gitignore (`data/`, `build/bin`, `frontend/node_modules`, `frontend/dist`), LICENSE (CC BY-NC 4.0)
- [x] Go module + Vue 3 + TS + Vite + Pinia + PrimeVue frontend, embedded via `//go:embed`
- [x] Portable data dir resolver (`data\` next to exe; `IW_DATA_DIR` override)
- [x] SQLite store + embedded migrations (sources, projects, items, comments, jobs)
- [x] Loopback HTTP server: `127.0.0.1` random free port (optional `IW_PORT` if free), SPA fallback, `data\runtime.json` (port + token), token/cookie auth, Host check
- [x] Tray (own Win32): dynamic badge icon, menu Open dashboard / Test notification / Quit, left-click opens browser
- [x] Balloon notification; click → browser at `/item/:id` (verified via simulated `NIN_BALLOONUSERCLICK`)
- [x] Single instance: named mutex per data dir; second launch → `POST /api/open` to running instance
- [x] Dashboard placeholder (PrimeVue DataTable, mock rows, checkbox selection) + `/item/:id` route — replaced by the Phase 1 SPA
- [ ] Manual visual check: tray icon/badge look, menu, real balloon click, DataTable in browser
- [ ] App icon + manifest resource for the exe (go-winres)

## Phase 1 — GitHub
- [x] Auth backend: GitHub App manifest flow + web flow/PKCE/loopback + refresh; device-flow fallback (`internal/provider/github`, `internal/api/github_auth.go`)
- [x] Sync backend: repos via installations, issues/comments via GraphQL, cursors, poller, rate limit (`internal/syncer`)
- [x] HTTP API: auth (providers list, start opens browser), projects, items (filters/pages), detail, read, reply, stats, sync (ARCHITECTURE.md → HTTP API; legacy /api/repos, /api/issues aliases kept)
- [x] Notifications: new issue / new comment / closed → tray balloon; badge = unread items
- [x] Sign-in callback focuses an open dashboard tab via SSE `navigate` (uses the SPA agent's hub in `internal/api/events.go`)
- [ ] **Manual live test (user)** — see below
- [x] Dashboard SPA (design spec from Codex): collapsible sidebar (Overview / Issues / Projects / Agents / Connections / Settings), top bar (sync status popover, sync now, theme, account menu), dark default + light toggle, history routing
- [x] Overview: stat cards, ECharts weekly opened/closed + per-project bars, needs attention, recent/live activity; "Connect GitHub" onboarding when nothing is connected or synced
- [x] Issues: lazy DataTable on `/api/items`, URL-backed filters (source/project/label/state/unread/text), multi-select bulk bar ("Send to agent" disabled → Phase 2, mark read), J/K/Enter/X keys; `/item/:id` detail with thread + reply composer (Ctrl+Enter)
- [x] Projects (counts, progress, `localPath` column, per-project chart), Connections (GitHub wired incl. device code + copy; CurseForge / Nexus Mods / Steam planned), Agents + Settings pages
- [x] SSE hub `GET /api/events` (`internal/api/events.go`) + client with backoff, toasts; tray / notification / second launch reuse an open tab via `navigate{path}` (new tab only when no client is connected)
- [ ] Drop legacy `/api/repos`, `/api/issues`, `/api/auth/{start,logout,device}` aliases (SPA no longer uses them)
- [ ] Manual check with a real GitHub account: onboarding → connect → live toasts; tray click focusing the existing tab in a real (non-headless) browser
- [x] Russian UI (vue-i18n, ru default + en switch), Russian tray/balloons/auth pages
- [x] Live reactivity: `sync.status` progress + `data.changed` → quiet refetch in every view
- [x] Settings → start with Windows (HKCU Run `"<exe>" --minimized`, rewritten when the folder moves) + start minimized; `--minimized` flag; `GET/PUT /api/settings`
- [x] Tray / notification click brings the dashboard's browser window to the front (EnumWindows by the `IssueWatcher · ` title marker); not found → new tab + old tabs «Дашборд открыт в другой вкладке»; double-click debounced; `IW_DEBUG=1` tray callback log
- [ ] **Manual (user, real desktop)**: Settings → «Запускать вместе с Windows» on → `HKCU\Software\Microsoft\Windows\CurrentVersion\Run\IssueWatcher` = `"<exe>" --minimized`; sign out/in → tray only, no tab; off → value gone. Move the folder, start once → value points to the new exe.
- [ ] **Manual (user)**: dashboard tab open but window minimised / behind other windows → tray left-click, menu «Открыть» and a balloon click each restore it and bring it to the front. Dashboard tab open but not the active tab in its window → a new tab opens, the old one shows «Дашборд открыт в другой вкладке» (log: `dashboard window not found by title; opening a new tab`). Double-click on the icon → one open. Real balloon click with `IW_DEBUG=1`: log must show `NIN_BALLOONUSERCLICK`; if it shows `NIN_BALLOONSHOW` + `NIN_BALLOONTIMEOUT` at once, Windows suppressed the banner (Do Not Disturb / Focus assist) — clicks in the notification center are not reported (seen on the dev machine 2026-09-28). Installed as an app window (browser menu → Install IssueWatcher) → always found.
- [x] Installed app window after an app restart stays signed in: persistent session secret `data\secrets\session.json` (block 5)
- [x] Infinite scroll: keyset API (`cursor`/`after`/`ids`/`limit`, comments + projects chunks), virtual Issues list, chunked comments/projects; page/per removed
- [x] Repo ↔ local folder mapping + auto-discovery (Settings → Проекты и папки; block 5)
- [ ] Optional: revoke token on logout (`DELETE /applications/{client_id}/token`)

### Manual live test (needs a real GitHub account; cannot run in CI)

1. `pwsh -File build.ps1`; start `build\bin\issuewatcher.exe` with a fresh `data\` (or `IW_DATA_DIR=<empty dir>`).
2. Dashboard → **Войти** → GitHub "Create GitHub App for (your account)" page → **Create GitHub App**.
   Expect: redirect to `github.com/apps/issuewatcher-xxxxxxxx/installations/new`; `data\secrets\github-app.json` exists; log has `github: app registered`.
3. Choose **All repositories** → **Install** → GitHub "Authorize IssueWatcher-…" → **Authorize**.
   Expect: back on the dashboard, header shows your login; log `github: signed in`; `data\secrets\github-token.json` exists.
4. Within ~1 min: `GET /api/projects` (browser devtools or `Invoke-RestMethod` with the bearer from `data\runtime.json`) lists your repos with counts; `GET /api/items` returns issues.
5. From another account (or web UI as someone else): open an issue / comment on one / close one → after the next poll (or `POST /api/sync`) a tray balloon appears and the badge increments; clicking opens `/item/<id>`.
6. `POST /api/items/<id>/comments {"body":"test"}` → comment visible on github.com; no balloon for your own comment.
7. **Port check (open risk)**: quit, start with `IW_PORT=<some other free port>` → **Войти** → **Authorize**.
   - Works → GitHub Apps honour the any-port loopback rule; record it in ARCHITECTURE.md.
   - "redirect_uri is not associated with this application" → rule not applied: normal runs still work via the remembered port; for busy-port cases enable **Device Flow** in the app settings (`github.com/settings/apps/<slug>` → Optional features/General) and use `POST /api/auth/github/device`; record the result.
8. Token refresh: after >8 h (or edit `expiry` in `github-token.json` to the past) sync still works; log shows no sign-out.
9. **Выйти** → header shows **Войти**; app registration kept (next **Войти** goes straight to Authorize).

## Block 5 — settings, palettes, tiered sync, session
- [x] No PrimeUI license key in the exe: `frontend/scripts/primevue-local.mjs` (npm `postinstall`, also before `dev`/`build`) strips PrimeVue 5.0.1's license check; exact-hash guarded, idempotent, fails on any other PrimeVue version (`npm test`)
- [x] One page title: the top bar is the heading (breadcrumb on detail pages via `lib/crumbs.ts`: Issues › owner/repo#N); body H1s and filler subtitles removed; repeated counts/labels dropped (Issues total when equal to the state counter, item author/opened line, Agents phase badge, project name twice)
- [x] Settings page with its own menu and deep links `/settings/:section`: Общие, Внешний вид, Уведомления, Синхронизация, Подключения, Проекты и папки, Агенты (placeholder), Обновления (version + «скоро»), Расширенные (paths, export, diagnostics, reset per section). `config.json` v2 (`schemaVersion`, migration of v1 `pollIntervalMinutes`/flat keys, unknown keys kept), one `GET/PATCH /api/settings` (merge patch, revision → 409, validation codes → localised messages, atomic write + `.bak`), live apply + SSE `settings.changed`; language and theme mode moved server-side (localStorage only for first paint)
- [x] Notifications settings: per-kind switches, per-project mute, quiet hours, group repeats, display time, full-screen DND, test button → `notify.Prefs` via `Tray.SetPrefs` (live); in-app toasts honour kinds + mutes
- [x] Projects & folders: roots, depth, excludes; discovery by git remote (`internal/folders`, worktrees too) → suggestions the user accepts; per-project path + status (ok / missing / not git / other remote)
- [x] Palettes: mode dark/light/system, 6 presets (indigo, ocean, forest, amber, rose, graphite; each ≥ 7:1 text contrast), custom accent/background/surface/text for the current mode with live preview + Reset, font family, text size (0.85–1.30, every px font size scales via `--iw-fs`), density (Issues row height, table cell padding). Server validates hex, ranges, enums and WCAG AA ≥ 4.5:1 text on surface and background. `lib/appearance.ts` derives the `--iw-*` tokens, PrimeVue `updatePreset` (`palette(accent)`, surface scale, datatable padding) and re-themes ECharts; popups get the same colours (`popupTheme`); first paint from `localStorage iw.paint`
- [x] Tiered sync: conditional REST change checks (ETag per exact URL, 304 = free) on `issues?state=all&since=` + `issues/comments?since=`, targeted GraphQL fetch of changed items, periodic full GraphQL reconcile; scheduler with active/idle intervals, jitter, bounded concurrency, hourly budget, `X-Poll-Interval`, `Retry-After` / secondary-limit backoff; generic `provider.Poller` + `Scheduling` for future platforms; Settings → Синхронизация shows quota, budget, 304s, next reconcile. Tests on the fake GitHub: 304 path, change path, rate-limit pause, budget, X-Poll-Interval
- [ ] **Manual (user, real GitHub)**: comment on an issue → toast within the active interval (default 5 min, fast 2); Settings → Синхронизация «бесплатных» grows while nothing changes
- [x] Session persistence: browser session secret kept in `data\secrets\session.json` (cookie `Max-Age` 400 d); tabs and the app window stay signed in across restarts (verified: cookie from run 1 → 200 on run 2 with a new port and bearer token); bearer token still per run. Needed by the self-update block (same-port restart)
- [x] `IW_HEADLESS=1` for automated runs: server + sync only (no tray icon, popups, browser, window focusing; in-memory autostart)

## Block 6 — self-update, public repo, local releases
- [x] Version from `git describe --tags` (tags `vX.Y.Z`); sidebar footer version opens Settings › Обновления (dot when an update is available)
- [x] `release.ps1` (no CI): clean `main` only, frontend `npm ci`/build/eslint, `aegis verify -profile release` (else vet + `test -race`), `-trimpath -s -w -H windowsgui`, UPX `--best --lzma` (missing → winget/scoop hint), smoke test of the packed exe, `issuewatcher-windows-amd64.exe` + `.sha256` + Ed25519-signed `manifest.json`, tag + `gh release create` with notes since the last tag. Key: `~\.config\issuewatcher\release-ed25519.key` (owner-only, `cmd/releasekey`), public key in `internal/selfupdate/pubkey.go`
- [x] Self-update (`internal/selfupdate`): stable/preview channels, auto-check 24 h ± jitter (no auto-install), signed manifest + size + SHA-256 + GitHub digest, no downgrade, `.old`/`.new` swap, `--after-update=<pid>` hand-over on the same port (busy → error + rollback via `--rolled-back`), graceful shutdown of the old process, leftovers removed; UI: Settings › Обновления (channel, auto-check, interval, check, install, progress, notes, last result), SSE reconnect → reload on the same route; tray «Проверить обновления»
- [x] Start grace: a normal start waits ≤ 3 s for the previous run's tab to reconnect before opening a new tab (SSE retry capped at 2.5 s); after-update starts never open one
- [x] E2E (`go test -tags e2e -run TestUpdateE2E ./cmd/issuewatcher`): v0.1.0 → v0.1.1 on the same port, old PID gone, no `.old`/`.new`, SSE reconnects with the persistent cookie; bad SHA / bad signature refused (old keeps running); pinned port taken → rollback to v0.1.0 with the reason
- [ ] **Manual (user, real desktop)**: open dashboard tab → Settings › Обновления → Install on a real release: the tab shows «Перезапуск…», reconnects and reloads on the same page; no second tab; tray icon comes back
## Own popup notifications (replace Windows tray balloons)

- [x] Own popup cards (`internal/notify/popup_windows.go`): layered, topmost, no-activate tool windows in the bottom-right corner of the work area, per-monitor DPI; card rendered in Go (Inter, SIL OFL) with shadow and slide/fade; stack of 3 + «+N ещё»; hover pauses auto-hide; × closes; click → `/item/:id` / `/issues?unread=1`. Balloon code removed; tray hover callbacks no longer logged; `IW_POPUP_SNAPSHOT=<dir>` renders sample PNG files
- [x] Wire settings → popups: `notifications` → `Tray.SetPrefs(notify.Prefs)` and theme mode → `Tray.SetTheme` at start and on every settings change (palette colours: block 5 task 3)
- [ ] **Manual (user, real desktop)**: tray «Тестовое уведомление» → card bottom-right, focus stays in the current window, hover pauses, × closes, click opens the item; taskbar on top/left; DPI change
- [ ] Focus assist / DND has no public API: `respectWindowsDnd` covers only full-screen / presentation mode (`SHQueryUserNotificationState`)

## Phase 2+ — see ARCHITECTURE.md
