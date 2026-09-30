# Restart cost against the GitHub fake: which upstream calls does a start make?
#
#   ./tools/perf/restart.ps1 -Bin <dir>\iw.exe -FakeBin <dir>\fakegithub.exe -DataDir <dir>\data -Setup  # fake + first sync
#   ./tools/perf/restart.ps1 -Bin <dir>\iw.exe -DataDir <dir>\data                                     # measure a restart
#   ./tools/perf/restart.ps1 -Stop -DataDir <dir>\data                                                 # stop the fake
#
# -Setup starts the fake (-Repos repos x -Issues issues, signed in through
# -seed) and runs a first start until the GitHub reconcile is done. A measured
# restart changes nothing upstream, watches the instance for -WaitSec seconds
# (reading /api/items and /api/projects meanwhile, listening to /api/events)
# and prints the fake's calls grouped by endpoint, the sync.status "started"
# events and the read latencies, then checks the warm-start target
# (docs/ARCHITECTURE.md → Storage & sync model): no /user/installations, no
# /graphql, no started cycle; only conditional REST checks (304 when idle).
# A check is due only after its interval since the last one (balanced: 5 min
# active, 30 min idle), so restart more than 5 min after -Setup to see 304s.
# Build the binaries with:
#   go build -o <dir>\iw.exe ./cmd/issuewatcher; go build -o <dir>\fakegithub.exe ./tools/fakegithub
# Never point -DataDir at a real data dir: -Setup writes fake GitHub secrets.
param(
  [string]$Bin = '',
  [Parameter(Mandatory)][string]$DataDir,
  [string]$FakeBin = '',
  [int]$Port = 18392,
  [int]$Repos = 61,
  [int]$Issues = 30,
  [switch]$Setup,
  [switch]$Stop,
  [string]$Tag = 'restart',
  [int]$WaitSec = 75
)
$ErrorActionPreference = 'Stop'
$fake = "http://127.0.0.1:$Port"
$h = [System.Net.Http.HttpClient]::new()
$h.Timeout = [TimeSpan]::FromSeconds(30)
$DataDir = [IO.Path]::GetFullPath($DataDir)
$work = Split-Path $DataDir -Parent
New-Item -ItemType Directory -Force $DataDir | Out-Null

if ($Stop) {
  $pidFile = Join-Path $work 'fake.pid'
  if (Test-Path $pidFile) { Stop-Process -Id (Get-Content $pidFile) -Force -ErrorAction SilentlyContinue; Remove-Item $pidFile }
  return
}
if (-not $Bin) { throw '-Bin is required' }

function Start-Iw([string]$log) {
  Remove-Item (Join-Path $DataDir 'runtime.json') -ErrorAction SilentlyContinue
  $env:IW_DATA_DIR = $DataDir; $env:IW_HEADLESS = '1'; $env:IW_NO_BROWSER = '1'; $env:IW_PORT = ''; $env:IW_GITHUB_API = $fake
  $p = Start-Process $Bin -PassThru -WindowStyle Hidden -RedirectStandardOutput "$log.out" -RedirectStandardError "$log.err"
  $sw = [Diagnostics.Stopwatch]::StartNew()
  while (-not (Test-Path (Join-Path $DataDir 'runtime.json'))) {
    if ($sw.ElapsedMilliseconds -gt 30000) { Stop-Process -Id $p.Id -Force; throw 'no runtime.json after 30 s' }
    Start-Sleep -Milliseconds 20
  }
  Start-Sleep -Milliseconds 50 # written atomically, but give the reader a moment
  $rt = Get-Content (Join-Path $DataDir 'runtime.json') | ConvertFrom-Json
  $h.DefaultRequestHeaders.Authorization = [System.Net.Http.Headers.AuthenticationHeaderValue]::new('Bearer', $rt.Token)
  [pscustomobject]@{ Proc = $p; URL = $rt.URL; Token = $rt.Token; Ms = $sw.ElapsedMilliseconds }
}

function Get-Calls { @(($h.GetStringAsync("$fake/_fake/state").GetAwaiter().GetResult() | ConvertFrom-Json).calls) }
function Get-Github($url) { ($h.GetStringAsync("$url/api/sync").GetAwaiter().GetResult() | ConvertFrom-Json).sources | Where-Object platform -EQ 'github' }
function Format-Call($c) {
  $s = if ($c -is [string]) { $c } else { ($c.PSObject.Properties.Value | Select-Object -First 2) -join ' ' }
  ($s -replace '\?.*$', '') -replace '/r\d+', '/rN'
}

if ($Setup) {
  if (-not $FakeBin) { throw '-Setup needs -FakeBin' }
  Get-NetTCPConnection -LocalPort $Port -State Listen -ErrorAction SilentlyContinue | ForEach-Object { Stop-Process -Id $_.OwningProcess -Force }
  $list = (1..$Repos | ForEach-Object { "octo/r$_" }) -join ','
  $f = Start-Process $FakeBin -ArgumentList '-addr', "127.0.0.1:$Port", '-repo', $list, '-seed', $DataDir -PassThru -WindowStyle Hidden `
    -RedirectStandardOutput (Join-Path $work 'fake.out') -RedirectStandardError (Join-Path $work 'fake.err')
  $f.Id | Set-Content (Join-Path $work 'fake.pid')
  Start-Sleep -Milliseconds 500
  foreach ($r in 1..$Repos) {
    foreach ($i in 1..$Issues) {
      $body = [System.Net.Http.StringContent]::new((@{ repo = "octo/r$r"; title = "issue $i of r$r" } | ConvertTo-Json), [Text.Encoding]::UTF8, 'application/json')
      $null = $h.PostAsync("$fake/_fake/issues", $body).GetAwaiter().GetResult()
    }
  }
  $iw = Start-Iw (Join-Path $work 'iw-first')
  try {
    $sw = [Diagnostics.Stopwatch]::StartNew()
    do { Start-Sleep -Milliseconds 200; $gh = Get-Github $iw.URL } until (($gh.lastSync -and -not $gh.running) -or $sw.ElapsedMilliseconds -gt 120000)
    Start-Sleep -Seconds 2 # poll state and cursors written
  } finally { Stop-Process -Id $iw.Proc.Id -Force }
  "[setup] fake on $fake (pid $($f.Id)) with $Repos x $Issues issues; first sync done in $($sw.ElapsedMilliseconds) ms; calls so far: $((Get-Calls).Count)"
  return
}

$calls0 = Get-Calls
$iw = Start-Iw (Join-Path $work "iw-$Tag")
$lat = @(); $reconciled = $false; $started = 0; $statusEvents = 0
try {
  # SSE: count sync.status events; a warm restart must not start a cycle.
  $es = [System.Net.Http.HttpClient]::new()
  $es.Timeout = [Threading.Timeout]::InfiniteTimeSpan
  $es.DefaultRequestHeaders.Authorization = [System.Net.Http.Headers.AuthenticationHeaderValue]::new('Bearer', $iw.Token)
  $reader = [IO.StreamReader]::new($es.GetStreamAsync("$($iw.URL)/api/events").GetAwaiter().GetResult())
  $line = $reader.ReadLineAsync(); $event = ''
  $sw = [Diagnostics.Stopwatch]::StartNew()
  while ($sw.ElapsedMilliseconds -lt $WaitSec * 1000) {
    foreach ($ep in '/api/items?limit=50', '/api/projects?limit=50') {
      $t = [Diagnostics.Stopwatch]::StartNew()
      $null = $h.GetStringAsync($iw.URL + $ep).GetAwaiter().GetResult()
      $lat += $t.Elapsed.TotalMilliseconds
    }
    $gh = Get-Github $iw.URL
    if ($gh.running) { $reconciled = $true }
    while ($line.IsCompleted) {
      $l = $line.Result
      if ($null -eq $l) { break }
      if ($l.StartsWith('event:')) { $event = $l.Substring(6).Trim() }
      elseif ($l.StartsWith('data:') -and $event -eq 'sync.status') {
        $statusEvents++
        if ($l -match '"state":"started"') { $started++ }
      }
      $line = $reader.ReadLineAsync()
    }
    Start-Sleep -Milliseconds 250
  }
  $calls1 = Get-Calls
} finally {
  Stop-Process -Id $iw.Proc.Id -Force
  if ($es) { $es.Dispose() }
}
$new = @($calls1 | Select-Object -Skip $calls0.Count | ForEach-Object { Format-Call $_ })
$s = $lat | Sort-Object
"[$Tag] runtime.json $($iw.Ms) ms; watched $WaitSec s; upstream calls: $($new.Count); reconcile seen: $reconciled; sync.status events: $statusEvents (started: $started)"
$new | Group-Object | Sort-Object Count -Descending | ForEach-Object { "  $($_.Count) x $($_.Name)" }
"  reads meanwhile: n=$($s.Count) p50=$([math]::Round($s[[int]($s.Count/2)],1)) p95=$([math]::Round($s[[int][math]::Floor($s.Count*0.95)],1)) ms"
"  github: lastSync=$($gh.lastSync) nextReconcile=$($gh.nextReconcile) projects=$($gh.projects) checks=$($gh.checks) notModified=$($gh.notModified)"
$bad = @($new | Where-Object { $_ -match 'installations|graphql' })
$ok = $bad.Count -eq 0 -and $started -eq 0 -and -not $reconciled
"  warm-start target (no installations/graphql calls, no started cycle): $(if ($ok) { 'PASS' } else { "FAIL ($($bad.Count) listing/GraphQL calls, $started started)" })"
