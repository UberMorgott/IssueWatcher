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
- [x] Dashboard placeholder (PrimeVue DataTable, mock rows, checkbox selection) + `/item/:id` route
- [ ] Manual visual check: tray icon/badge look, menu, real balloon click, DataTable in browser
- [ ] App icon + manifest resource for the exe (go-winres)

## Phase 1 — GitHub
- [ ] Auth: GitHub App manifest flow + web flow/PKCE/loopback; device-flow fallback
- [ ] Sync repos/issues/comments (GraphQL, polling, cursors)
- [ ] Dashboard table: filters, multi-select, per-item actions
- [ ] Reply to comments from UI
- [ ] Repo ↔ local folder mapping + auto-discovery
- [ ] Stats: global + per repo
- [ ] Notifications: new issue, new comment, closed/fixed

## Phase 2+ — see ARCHITECTURE.md
