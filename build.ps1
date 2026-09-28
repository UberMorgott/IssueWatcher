# Builds the portable exe into build\bin (frontend embedded).
# Usage: pwsh -File build.ps1 [-SkipFrontend]
param([switch]$SkipFrontend)
$ErrorActionPreference = 'Stop'
Set-Location $PSScriptRoot

if (-not $SkipFrontend) {
    Push-Location frontend
    try {
        npm install
        if ($LASTEXITCODE) { throw 'npm install failed' }
        npm run build
        if ($LASTEXITCODE) { throw 'frontend build failed' }
    } finally { Pop-Location }
}

$version = git describe --tags --always --dirty 2>$null
if (-not $version) { $version = 'dev' }

go build -trimpath -ldflags "-s -w -H windowsgui -X main.Version=$version" -o build\bin\issuewatcher.exe .\cmd\issuewatcher
if ($LASTEXITCODE) { throw 'go build failed' }
Write-Host "built build\bin\issuewatcher.exe ($version)"
