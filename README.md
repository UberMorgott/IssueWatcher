# IssueWatcher

A small Windows tray app that watches the issues of your GitHub repositories and
shows them in a local web dashboard. One portable executable: a Go loopback
server with an embedded Vue UI, its own tray icon and popup notifications, and
SQLite storage.

## Features

- Sign in with GitHub (a GitHub App created for your account in one click); all
  repositories of the installation are synced.
- Near-realtime sync: conditional REST change checks (a `304` costs nothing),
  targeted GraphQL fetches, a periodic full reconcile and a request budget.
- Dashboard at `http://127.0.0.1:<port>`: overview charts, an issue list with
  filters and keyboard navigation, threads with replies, projects mapped to
  local clones.
- Mod platforms (Nexus Mods, CurseForge, Factorio Mod Portal, Steam Workshop)
  with one «Подключить» button each: Steam signs in by a QR code scanned in the
  Steam app, the others in a sign-in window of your installed browser (or its
  session); accounts are detected, expired sessions raise a «войдите снова»
  card, mod pages link themselves to the GitHub repo of the same name. All
  built in: no Node.js or external MCP servers at runtime.
- Tray icon with an unread badge and own popup cards for new issues, comments
  and closed issues (quiet hours, per-project mute, grouping).
- Settings for language (Russian / English), palettes and fonts, sync intervals,
  notifications, start with Windows.
- Self-update from GitHub releases: stable or preview channel, Ed25519-signed
  manifests, restart on the same port so the open tab reconnects.

## Portable

Everything lives next to the executable in `data\` (database, settings, logs,
tokens readable only by your Windows account). Move or delete the folder and
the app goes with it. The only write outside it is the optional «start with
Windows» entry in `HKCU\...\Run`.

## Run

Download `issuewatcher-windows-amd64.exe` from
[Releases](https://github.com/UberMorgott/IssueWatcher/releases), put it in a
folder of its own and start it. Windows 10/11, amd64.

## CLI and MCP (control an agent can use)

The same exe, called with a subcommand, talks to the **running** app of its
`data\` folder over the loopback API (it never starts the app, and needs no
sign-in of its own). JSON on stdout, errors on stderr; exit code 0 ok, 1 API
error, 2 usage, 3 app not running. `issuewatcher help` lists the flags.

| Command | Does |
|---|---|
| `status` · `projects` | app version + sync state · projects with counts |
| `items [--project --state --label --q --unread --limit --cursor]` | one page of issues (`nextCursor` for the next) |
| `item <id>` · `jobs [--state --flow --origin --project --item]` · `job <id>` · `job log <id> [--attempt]` | issue + comments · agent jobs · one job · its log |
| `sync` | sync now |
| `reply <itemId> (--body-file f \| -)` | post a comment on GitHub (`-` = stdin) |
| `jobs create --flow fix\|reply\|label <itemId>...` | queue agent jobs |
| `job cancel\|retry\|dismiss\|push\|pr <id>` · `job reply <id> (--body-file f \| -)` · `job labels <id> <name>...` | the job page buttons |
| `publish <projectId> --path ARCHIVE --version V [--file-id ID \| --new-file] [--name N] [--changelog-file f] [--dry-run] [--wait]` · `publish targets <projectId>` · `publish status <taskId>` | upload an archive as a new mod version (Nexus, Factorio) with the app's stored credentials · the files/versions to publish to · a publish task |

MCP server (stdio): **don't register it globally or per project** (`claude mcp add`,
`~/.codex/config.toml`) — every other session in that folder would carry its
tools. Agent jobs get it on their own (below). For manual use, attach it to one
session only:

```powershell
'{"mcpServers":{"issuewatcher":{"command":"C:\\path\\to\\issuewatcher.exe","args":["mcp"]}}}' | Set-Content iw-mcp.json
claude --mcp-config iw-mcp.json
```

Tools: `list_projects, list_items, get_item, list_item_comments, list_jobs, get_job, get_job_log,
sync_now, reply_item, start_jobs, cancel_job, retry_job, send_job_reply,
apply_job_labels, push_job, create_pr, list_publish_targets, publish_version,
get_publish_task` (local ids, pages ≤ 50; logs in `data\logs\mcp.log`).
Publishing tools (comment, push, PR, mod version) have no extra gate: approve
them in your MCP client. `publish_version` takes a mod project id, an absolute
archive path and the version; run it with `dry_run` first. Nexus uses the API
key from Settings › Платформы; Factorio creates its upload API key once from
the signed-in factorio.com session (`data\secrets\factorio-api.json`) and checks
the archive's `info.json` name and version.

Agent jobs: each agent run of a job gets `issuewatcher.exe mcp --item <id>`, a
read-only server for that job's issue only (`get_item`, `list_item_comments`),
passed on the command line — claude `--mcp-config <temp file>` (deleted after
the run; your own MCP servers still load), codex `-c mcp_servers.issuewatcher=…`.
Nothing is written to the CLIs' config. Settings › Агенты › «MCP IssueWatcher в
задачах» (`agents.jobMcp`, default on), per project in «По проектам»
(`agents.projects["owner/repo"].jobMcp`, unset = inherit).

Two ways to start agents: per issue (issue page, or select issues → «Отправить
агенту»), or per project — «Разобрать проект» on the Projects page
(`POST /api/projects/{id}/triage`): a read-only triage agent (the responder)
ranks the project's open issues by criticality (it gets their titles, labels,
age, comment counts and body starts, plus `mcp --project <id>`: read-only
`list_items` / `get_item` / `list_item_comments` of that project only), and the
app queues a fix job for each of the first N picks that have no unfinished fix
job (`agents.triageTopN`, default 3, per project override; triage prompt and
per-project criteria in Settings › Агенты). The triage job lists the ranking
and the fix jobs it queued.

In a shell use `issuewatcher-cli.exe`: the app writes it next to itself on
every start (a console build of the same exe, refreshed by updates). Shells
wait for it and `$LASTEXITCODE` / `%ERRORLEVEL%` are set:

```powershell
& "C:\path\to\issuewatcher-cli.exe" status; $LASTEXITCODE
```

`issuewatcher.exe` is windowed, so an interactive shell does not wait for it
(output after the prompt, exit code lost); agents and MCP clients read it
through pipes, where it works as is.

## Build

Needs Go 1.27+ and Node.js (npm) to build the frontend; the built app needs neither.

```powershell
pwsh -File build.ps1          # frontend + go build → build\bin\issuewatcher.exe
go test -race ./...           # Go tests
cd frontend; npm run build    # frontend type check + bundle
```

Perf guard (every read from SQLite, < 50 ms; see `docs/ARCHITECTURE.md` → Storage & sync model):
`go test ./internal/api -run '^$' -bench BenchmarkReadPaths -benchtime 200x` (seeded store, 5k items × 100 projects);
`tools/perf/restart.ps1` (upstream calls of a restart against `tools/fakegithub`: `-Setup` once, then a restart; prints PASS/FAIL for the warm-start target) and
`tools/perf/bench.ps1 -DataDir <dir>` (endpoint p50/p95 of a running instance, e.g. a copy of a data dir without `secrets`). Usage in each script's header.

## Release

Releases are built and published locally (no CI) by the owner:

```powershell
pwsh -File release.ps1 -Version v0.2.0        # -DryRun to stop before tagging
```

It refuses a dirty tree or another branch, verifies (frontend build + eslint,
Go checks), builds a stripped `-H windowsgui` exe, packs it with UPX, smoke-tests
the packed exe, writes the SHA-256 and a signed `manifest.json`, then tags and
runs `gh release create`. The signing key stays outside the repository
(`go run ./cmd/releasekey init`).

## License

[CC BY-NC 4.0](LICENSE) — Copyright (c) 2026 Morgott.
