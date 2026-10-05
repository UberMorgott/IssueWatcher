# IssueWatcher — project rules

## Verification: Aegis only (no qgate)
- Owner decision (2026-09-28): this repo does NOT use qgate. No lefthook/pre-commit hook, and don't reinstall one.
  This overrides the global "Quality gate (qgate)" rule for this project only.
- Aegis is enabled here (`aegis on`). Navigate with `aegis.query` first, make edits via `aegis.change` or
  normal edits, then `aegis.verify` for build/tests. Frontend: `npm run build` (vue-tsc) + eslint in `frontend/`.
- Done = shown evidence: include the `aegis.verify` result (and the frontend build result if the UI changed).
- Report Aegis problems and wins per the global "Aegis feedback" rule.

## Repo notes
- Commit identity: `git -c user.name=UberMorgott -c user.email=UberMorgott@users.noreply.github.com commit`.
- Parallel agents share one index: commit with `git commit --only -- <explicit paths>`, don't stage ahead of time.
- Design and phases: `docs/ARCHITECTURE.md`; checklist: `TASKS.md`.

## Portable, UI-only (owner, 2026-10-05)
- Every helper the app needs (steamcmd, steam_api64.dll, other tools, their caches/sessions) lives in the folder
  next to the portable binary (app data dir). Never install into or depend on system/other-program locations
  (no %APPDATA%/registry/global PATH writes; don't run tools from other apps' folders). Game installs are only read.
- Owner only ever acts through the app UI (one-time login forms). Everything else is automatic under the hood.

## Releases: standing OK (owner, 2026-09-30)
- After verified user-facing fixes/features land on `main`, cut a release without asking: push `main`, then
  `pwsh -File release.ps1 -Version vX.Y.Z` (patch for fixes), Russian release notes in the v0.9.0 style.
  Why: the owner's installed app self-updates only from releases, so unreleased fixes never reach them.
