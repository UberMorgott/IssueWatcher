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
- [x] PrimeUI license: `VITE_PRIMEUI_LICENSE` in gitignored `frontend/.env` → `registerLicense` (as in `E:\DEV\1сEPD\web`); `frontend/.env.example` documents it
- [ ] Drop legacy `/api/repos`, `/api/issues`, `/api/auth/{start,logout,device}` aliases (SPA no longer uses them)
- [ ] Manual check with a real GitHub account: onboarding → connect → live toasts; tray click focusing the existing tab in a real (non-headless) browser
- [ ] Repo ↔ local folder mapping + auto-discovery
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

## Phase 2+ — see ARCHITECTURE.md
