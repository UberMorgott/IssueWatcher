# Agent rules

This repo does not use qgate and has no pre-commit hook. Verification is Aegis only:
`aegis.verify` for Go (build, vet, tests, lint), plus `npm run build` and eslint in `frontend/`
when the UI changes. Include the verify result in your final report.

Full project rules: [CLAUDE.md](CLAUDE.md).
