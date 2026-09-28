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
