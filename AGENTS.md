# Agent rules

This repo uses neither qgate nor Aegis and has no pre-commit hook. Verification:
`go vet ./...` + `go test ./...`, plus `npm run build` and eslint in `frontend/`
when the UI changes. Include the command output in your final report.

Full project rules: [CLAUDE.md](CLAUDE.md).
