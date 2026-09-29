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

Register the MCP server (stdio) in Claude Code:

```powershell
claude mcp add issuewatcher -- "C:\path\to\issuewatcher.exe" mcp
```

Tools: `list_projects, list_items, get_item, list_jobs, get_job, get_job_log,
sync_now, reply_item, start_jobs, cancel_job, retry_job, send_job_reply,
apply_job_labels, push_job, create_pr` (local ids, pages ≤ 50; logs in
`data\logs\mcp.log`). Publishing tools (comment, push, PR) have no extra gate:
approve them in your MCP client.

The release exe is a windowed app: Windows shells do not wait for it. Pipe its
output so they do — `issuewatcher status | Out-String` in PowerShell (also sets
`$LASTEXITCODE`). Agents and MCP
clients read through pipes and need nothing extra.

## Build

Needs Go 1.27+ and Node.js (npm).

```powershell
pwsh -File build.ps1          # frontend + go build → build\bin\issuewatcher.exe
go test -race ./...           # Go tests
cd frontend; npm run build    # frontend type check + bundle
```

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
