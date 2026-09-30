# Endpoint latency of a running instance: p50/p95/max per GET, first request dropped as warm-up.
#
#   ./tools/perf/bench.ps1 -DataDir <data dir of a running instance> [-N 15] [-Label idle] [-Only '/api/stats','/api/items?limit=50']
#
# Reads the URL and bearer token from <DataDir>\runtime.json. The item, project
# and job ids of the detail endpoints come from the first rows of the lists
# (override with -ItemId / -ProjectId / -JobId). Every endpoint is expected
# under -BudgetMs (50 ms, docs/ARCHITECTURE.md → Storage & sync model) and a few
# have tighter targets (-Targets); rows over their target are marked SLOW and
# the script exits 1. Benchmark a copy of a real data dir without its secrets
# folder (signed out), never the live one; start it with IW_DATA_DIR=<copy>,
# IW_HEADLESS=1, IW_NO_BROWSER=1.
param(
  [Parameter(Mandatory)][string]$DataDir,
  [int]$N = 15,
  [string]$Label = 'idle',
  [string[]]$Only = @(),
  [int64]$ItemId = 0,
  [int64]$ProjectId = 0,
  [int64]$JobId = 0,
  [double]$BudgetMs = 50,
  [hashtable]$Targets = @{ '/api/agents/detect' = 5; '/api/projects' = 2; '/api/platforms' = 2 }
)
$ErrorActionPreference = 'Stop'
$rt = Get-Content (Join-Path $DataDir 'runtime.json') | ConvertFrom-Json
$h = [System.Net.Http.HttpClient]::new()
$h.Timeout = [TimeSpan]::FromSeconds(60)
$h.DefaultRequestHeaders.Authorization = [System.Net.Http.Headers.AuthenticationHeaderValue]::new('Bearer', $rt.Token)
function Get-Json($path) { $h.GetStringAsync($rt.URL + $path).GetAwaiter().GetResult() | ConvertFrom-Json }
if (-not $ItemId) { $ItemId = @((Get-Json '/api/items?limit=1').items)[0].id }
if (-not $ProjectId) { $ProjectId = @((Get-Json '/api/projects?limit=1').items)[0].id }
if (-not $JobId) { $JobId = @((Get-Json '/api/jobs?limit=1').items)[0].id }
$eps = @(
  '/api/health', '/api/auth/status', '/api/sync', '/api/settings', '/api/update', '/api/platforms', '/api/providers/steam',
  '/api/projects', '/api/projects?limit=50', '/api/projects?limit=50&group=1',
  '/api/items?limit=50', '/api/items?limit=50&state=open', '/api/items?limit=50&unread=1', '/api/items?limit=50&q=crash',
  "/api/items/$ItemId", "/api/items/$ItemId/comments?limit=50", '/api/stats', "/api/stats?project=$ProjectId",
  '/api/jobs', '/api/automation/log', '/api/folders', "/api/projects/$ProjectId/links", "/api/projects/$ProjectId/labels", '/api/agents/detect'
)
if ($JobId) { $eps += "/api/jobs/$JobId", "/api/jobs/$JobId/log", "/api/jobs/$JobId/attempts" }
if ($Only.Count) { $eps = $Only }
$slow = 0
$rows = foreach ($e in $eps) {
  $t = @(); $code = 0; $len = 0
  for ($i = 0; $i -le $N; $i++) {
    $sw = [Diagnostics.Stopwatch]::StartNew()
    try { $r = $h.GetAsync($rt.URL + $e).GetAwaiter().GetResult(); $b = $r.Content.ReadAsByteArrayAsync().GetAwaiter().GetResult(); $code = [int]$r.StatusCode; $len = $b.Length } catch { $code = -1 }
    $sw.Stop()
    if ($i -gt 0) { $t += $sw.Elapsed.TotalMilliseconds } # first = warm-up
  }
  $s = $t | Sort-Object
  $p95 = $s[[int]([math]::Floor($s.Count * 0.95))]
  $target = if ($Targets.ContainsKey($e)) { $Targets[$e] } else { $BudgetMs }
  $flag = if ($p95 -gt $target) { $slow++; "SLOW (>$target)" } else { '' }
  [pscustomobject]@{ ep = $e; code = $code; kb = [math]::Round($len / 1KB, 1); p50 = [math]::Round($s[[int]($s.Count / 2)], 1); p95 = [math]::Round($p95, 1); max = [math]::Round($s[-1], 1); target = $flag }
}
"== $Label (N=$N, budget $BudgetMs ms) =="
$rows | Format-Table -AutoSize | Out-String -Width 200
if ($slow) { "$slow endpoint(s) over target"; exit 1 }
"all endpoints within target"
