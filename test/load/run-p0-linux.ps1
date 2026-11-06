param([int]$Seconds = 20, [string]$Levels = '300,300', [switch]$ReadOnly,
    [ValidatePattern('^feedsystem-p0-linux(?:-[a-z0-9-]+)?$')][string]$ProjectName='feedsystem-p0-linux')
$ErrorActionPreference = 'Stop'
$repoRoot = (Resolve-Path (Join-Path $PSScriptRoot '../..')).Path
$composeArgs = @('compose','-p',$ProjectName,'-f',(Join-Path $PSScriptRoot 'compose.p0.yml'),'-f',(Join-Path $PSScriptRoot 'compose.p0.linux.yml'))
$resultDir = Join-Path $PSScriptRoot ('results/' + (Get-Date -Format 'yyyyMMdd-HHmmss'))
$savedTargetOS = $env:GOOS
$savedCGO = $env:CGO_ENABLED
$savedZoneInfo = $env:P0_ZONEINFO
$env:P0_ZONEINFO = Join-Path (& go env GOROOT) 'lib/time/zoneinfo.zip'
$started = $false
Push-Location (Join-Path $repoRoot 'backend')
try {
    try {
        $env:GOOS='linux'; $env:CGO_ENABLED='0'
        & go build -o .run/p0/linux/api ./cmd
        if ($LASTEXITCODE -ne 0) { throw 'Linux API build failed' }
        & go build -o .run/p0/linux/worker ./cmd/worker
        if ($LASTEXITCODE -ne 0) { throw 'Linux Worker build failed' }
    } finally { $env:GOOS=$savedTargetOS; $env:CGO_ENABLED=$savedCGO }
    & go build -o .run/p0/p0bench.exe ./cmd/p0bench
    if ($LASTEXITCODE -ne 0) { throw 'Load generator build failed' }
    New-Item -ItemType Directory -Path $resultDir -Force | Out-Null
    & git diff --binary | Set-Content -Encoding utf8 (Join-Path $resultDir 'working-tree.patch')
    & git rev-parse HEAD | Set-Content (Join-Path $resultDir 'revision.txt')
    Get-FileHash .run/p0/linux/api,.run/p0/linux/worker,.run/p0/p0bench.exe -Algorithm SHA256 | ConvertTo-Json | Set-Content (Join-Path $resultDir 'sha256.json')
    Get-CimInstance Win32_ComputerSystem | Select-Object NumberOfLogicalProcessors,TotalPhysicalMemory | ConvertTo-Json | Set-Content (Join-Path $resultDir 'host.json')
    $started = $true
    & docker @composeArgs up -d --wait --wait-timeout 120
    if ($LASTEXITCODE -ne 0) { throw 'Linux test dependencies did not become healthy' }
    & docker @composeArgs config | Set-Content (Join-Path $resultDir 'compose.resolved.yml')
    & docker info --format '{{json .}}' | Set-Content (Join-Path $resultDir 'docker-info.json')
    $benchArgs = @('-external-processes','-output',$resultDir,'-seconds',$Seconds,'-levels',$Levels,'-cases','feed_auth_hot,feed_anon_cold','-compose-project',$ProjectName)
    if ($ReadOnly) { $benchArgs += '-read-only' }
    & ./.run/p0/p0bench.exe @benchArgs
    if ($LASTEXITCODE -ne 0) { throw 'Linux test failed; inspect its result directory' }
} finally {
    try {
        if ($started) {
            & docker @composeArgs logs --no-color api | Set-Content -Encoding utf8 (Join-Path $resultDir 'api.log')
            & docker @composeArgs logs --no-color worker | Set-Content -Encoding utf8 (Join-Path $resultDir 'worker.log')
            & docker @composeArgs stop
        }
    } finally { $env:P0_ZONEINFO=$savedZoneInfo; Pop-Location }
}
