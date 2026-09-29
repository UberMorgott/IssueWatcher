# Local release pipeline (no CI): verify, build, UPX-pack, smoke-test, sign,
# tag and publish a GitHub release of UberMorgott/IssueWatcher (Windows amd64).
#
#   pwsh -File release.ps1 -Version v0.1.0 [-MinVersion v0.0.0] [-DryRun]
#
# -DryRun stops after the signed assets are in build\release\<version> (no tag,
# no push, no GitHub release). A version with a prerelease part (v0.2.0-rc.1)
# is published as a GitHub prerelease: the updater's preview channel.
# The signing key stays outside the repo ($env:IW_RELEASE_KEY or
# ~\.config\issuewatcher\release-ed25519.key; create it with
# `go run ./cmd/releasekey init`).
param(
    [Parameter(Mandatory)][string]$Version,
    [string]$MinVersion = '',
    [switch]$DryRun
)
$ErrorActionPreference = 'Stop'
Set-Location $PSScriptRoot

function Fail($msg) { Write-Host "release: $msg" -ForegroundColor Red; exit 1 }
function Step($msg) { Write-Host "==> $msg" -ForegroundColor Cyan }
function Run([scriptblock]$cmd, $what) { & $cmd; if ($LASTEXITCODE) { Fail "$what failed (exit $LASTEXITCODE)" } }

if ($Version -notmatch '^v\d+\.\d+\.\d+(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$') { Fail "version must look like v1.2.3 or v1.2.3-rc.1, got '$Version'" }
$prerelease = $Version.Contains('-')
$asset = 'issuewatcher-windows-amd64.exe'
$keyPath = if ($env:IW_RELEASE_KEY) { $env:IW_RELEASE_KEY } else { Join-Path $HOME '.config\issuewatcher\release-ed25519.key' }

# --- preconditions --------------------------------------------------------------
Step 'checking the tree'
$branch = git rev-parse --abbrev-ref HEAD
if ($branch -ne 'main') { Fail "not on main (on '$branch')" }
$dirty = git status --porcelain
if ($dirty) { Fail "working tree is dirty:`n$($dirty -join "`n")" }
if (git tag --list $Version) { Fail "tag $Version already exists" }
if (-not (Get-Command upx -ErrorAction SilentlyContinue)) {
    Fail "UPX not found on PATH. Install it with 'winget install --id UPX.UPX' or 'scoop install upx', then open a new shell."
}
if (-not (Test-Path $keyPath)) { Fail "signing key $keyPath missing: run 'go run ./cmd/releasekey init' once" }
if (-not $DryRun) {
    if (-not (Get-Command gh -ErrorAction SilentlyContinue)) { Fail 'GitHub CLI (gh) not found' }
    Run { git fetch --quiet origin main --tags } 'git fetch'
    if ((git rev-parse HEAD) -ne (git rev-parse origin/main)) { Fail 'HEAD is not origin/main: push main first' }
    if (git ls-remote --tags origin "refs/tags/$Version") { Fail "tag $Version already exists on origin" }
}
$lastTag = git describe --tags --abbrev=0 2>$null

# --- verification ---------------------------------------------------------------
Step 'frontend: npm ci, build, eslint'
Push-Location frontend
try {
    Run { npm ci --no-audit --no-fund } 'npm ci'
    Run { npm run build } 'frontend build'
    Run { npx eslint src } 'eslint'
} finally { Pop-Location }

if (Get-Command aegis -ErrorAction SilentlyContinue) {
    Step 'aegis verify -profile release'
    Run { aegis verify -profile release } 'aegis verify'
} else {
    Step 'go vet + go test -race'
    Run { go vet ./... } 'go vet'
    Run { go test -race ./... } 'go test -race'
}

# --- build + pack -------------------------------------------------------------------
$out = Join-Path $PSScriptRoot "build\release\$Version"
if (Test-Path $out) { Remove-Item -Recurse -Force $out }
New-Item -ItemType Directory -Force $out | Out-Null
$raw = Join-Path $out 'issuewatcher-raw.exe'
$exe = Join-Path $out $asset

Step "go build $Version"
Run { go build -trimpath -ldflags "-s -w -H windowsgui -X main.Version=$Version" -o $raw ./cmd/issuewatcher } 'go build'
$rawSize = (Get-Item $raw).Length

Step 'upx --best --lzma'
Run { upx --best --lzma -q -o $exe $raw | Out-Null } 'upx'
Run { upx -t -q $exe | Out-Null } 'upx -t'
$size = (Get-Item $exe).Length
Remove-Item $raw

# --- smoke test: the packed exe serves /api/health with the right version ---------
Step 'smoke test (IW_HEADLESS=1, scratch data dir)'
$smokeData = Join-Path ([IO.Path]::GetTempPath()) ("iw-smoke-" + [guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Force $smokeData | Out-Null
$saved = @{ IW_HEADLESS = $env:IW_HEADLESS; IW_DATA_DIR = $env:IW_DATA_DIR; IW_UPDATE_BASE = $env:IW_UPDATE_BASE }
try {
    $env:IW_HEADLESS = '1'; $env:IW_DATA_DIR = $smokeData
    $env:IW_UPDATE_BASE = 'http://127.0.0.1:9' # never ask GitHub from a smoke run
    $proc = Start-Process -FilePath $exe -PassThru -WindowStyle Hidden
} finally {
    foreach ($k in $saved.Keys) {
        if ($null -eq $saved[$k]) { Remove-Item "env:$k" -ErrorAction SilentlyContinue } else { Set-Item "env:$k" -Value $saved[$k] }
    }
}
try {
    $rt = $null
    for ($i = 0; $i -lt 60 -and -not $rt; $i++) {
        Start-Sleep -Milliseconds 250
        if ($proc.HasExited) { Fail "smoke: exe exited with code $($proc.ExitCode)" }
        try { $rt = Get-Content (Join-Path $smokeData 'runtime.json') -Raw | ConvertFrom-Json } catch { $rt = $null }
    }
    if (-not $rt) { Fail 'smoke: runtime.json never appeared' }
    $h = Invoke-WebRequest -Uri "$($rt.url)/api/health" -Headers @{ Authorization = "Bearer $($rt.token)" } -UseBasicParsing
    $health = $h.Content | ConvertFrom-Json
    if ($h.StatusCode -ne 200 -or $health.version -ne $Version) { Fail "smoke: /api/health $($h.StatusCode) version '$($health.version)'" }
    Write-Host "smoke: /api/health 200, version $($health.version), pid $($proc.Id)"
    # The app writes its console copy for shells next to itself (not an asset).
    $cli = Join-Path $out 'issuewatcher-cli.exe'
    for ($i = 0; $i -lt 40 -and -not (Test-Path $cli); $i++) { Start-Sleep -Milliseconds 250 }
    if (-not (Test-Path $cli)) { Fail 'smoke: issuewatcher-cli.exe never appeared' }
    Start-Sleep -Milliseconds 500 # written as .new, then renamed: settled once it exists
    $env:IW_DATA_DIR = $smokeData
    try { $cliOut = & $cli status; $cliCode = $LASTEXITCODE } finally { Remove-Item env:IW_DATA_DIR -ErrorAction SilentlyContinue; if ($null -ne $saved.IW_DATA_DIR) { $env:IW_DATA_DIR = $saved.IW_DATA_DIR } }
    if ($cliCode -ne 0 -or (($cliOut -join "`n") | ConvertFrom-Json).health.version -ne $Version) { Fail "smoke: issuewatcher-cli status exit $cliCode" }
    Write-Host "smoke: issuewatcher-cli.exe status exit 0"
} finally {
    if (-not $proc.HasExited) { Stop-Process -Id $proc.Id -Force; $proc.WaitForExit(10000) | Out-Null }
    Remove-Item -Recurse -Force $smokeData -ErrorAction SilentlyContinue
    Get-ChildItem $out -Force -Filter '*issuewatcher-cli.exe*' | Remove-Item -Force -ErrorAction SilentlyContinue
}

# --- manifest + signature -----------------------------------------------------------
Step 'signing manifest'
$releasekeyArgs = @('manifest', '-key', $keyPath, '-exe', $exe, '-version', $Version, '-out', $out)
if ($MinVersion) { $releasekeyArgs += @('-min', $MinVersion) }
Run { go run ./cmd/releasekey @releasekeyArgs } 'releasekey manifest'

# --- notes: commits since the last tag ----------------------------------------------
$range = if ($lastTag) { "$lastTag..HEAD" } else { 'HEAD' }
$log = git log $range --no-merges --pretty=format:'- %s'
$notes = Join-Path $out 'notes.md'
$header = if ($lastTag) { "Changes since $lastTag`:" } else { 'First public release.' }
Set-Content -Path $notes -Value (@($header, '', ($log -join "`n"), '', "SHA-256 ($asset): $((Get-FileHash $exe -Algorithm SHA256).Hash.ToLower())") -join "`n")

$assets = @($exe, "$exe.sha256", (Join-Path $out 'manifest.json'), (Join-Path $out 'manifest.json.sig'))
Write-Host ("size: {0:N0} bytes raw -> {1:N0} bytes packed ({2:P0})" -f $rawSize, $size, ($size / $rawSize))
if ($DryRun) {
    Step "dry run: assets in $out (no tag, no release)"
    exit 0
}

# --- tag + publish ------------------------------------------------------------------------
Step "tag $Version + GitHub release"
Run { git -c user.name=UberMorgott -c user.email=UberMorgott@users.noreply.github.com tag -a $Version -m "IssueWatcher $Version" } 'git tag'
Run { git push origin $Version } 'git push tag'
$ghArgs = @('release', 'create', $Version) + $assets + @('--repo', 'UberMorgott/IssueWatcher', '--title', "IssueWatcher $Version", '--notes-file', $notes, '--verify-tag')
if ($prerelease) { $ghArgs += '--prerelease' } else { $ghArgs += '--latest' }
Run { gh @ghArgs } 'gh release create'
Write-Host "released $Version"
