param(
    [int]$Seconds = 20,
    [string]$Levels = '25,100,300',
    [int]$WriteRequests = 2000,
    [switch]$KeepDependencies,
    [switch]$ReadOnly,
    [switch]$FaultChecks,
    [string]$Cases = 'feed_anon_hot,feed_auth_hot,feed_anon_cold,detail_hot,detail_distributed,detail_expiring',
    [ValidatePattern('^feedsystem-p0(?:-[a-z0-9-]+)?$')]
    [string]$ProjectName = 'feedsystem-p0'
)
$ErrorActionPreference = 'Stop'
if ($FaultChecks -and ($ReadOnly -or -not $ProjectName.StartsWith('feedsystem-p0-reliable'))) {
    throw 'Fault checks write only to a dedicated feedsystem-p0-reliable project; do not combine with ReadOnly.'
}
$repoRoot = (Resolve-Path (Join-Path $PSScriptRoot '../..')).Path
$composePath = Join-Path $PSScriptRoot 'compose.p0.yml'
$resourceJob = $null
Push-Location (Join-Path $repoRoot 'backend')
try {
    & docker compose -p $ProjectName -f $composePath up -d --wait --wait-timeout 240
    if ($LASTEXITCODE -ne 0) { throw 'Dedicated P0 dependencies did not become healthy.' }
    & go build -o .run/p0/api.exe ./cmd
    if ($LASTEXITCODE -ne 0) { throw 'API build failed.' }
    & go build -o .run/p0/worker.exe ./cmd/worker
    if ($LASTEXITCODE -ne 0) { throw 'Worker build failed.' }
    & go build -o .run/p0/p0bench.exe ./cmd/p0bench
    if ($LASTEXITCODE -ne 0) { throw 'Load generator build failed.' }
    $resultDir = Join-Path $PSScriptRoot ('results/' + (Get-Date -Format 'yyyyMMdd-HHmmss'))
    New-Item -ItemType Directory -Path $resultDir -Force | Out-Null
    & docker info --format '{{json .}}' | Set-Content -Encoding utf8 (Join-Path $resultDir 'docker-info.json')
    & git rev-parse HEAD | Set-Content (Join-Path $resultDir 'revision.txt')
    & git status --short | Set-Content (Join-Path $resultDir 'working-tree.txt')
    & git diff --binary | Set-Content -Encoding utf8 (Join-Path $resultDir 'working-tree.patch')
    Get-FileHash .run/p0/api.exe,.run/p0/worker.exe,.run/p0/p0bench.exe,cmd/p0bench/main.go -Algorithm SHA256 |
        Select-Object Path,Hash | ConvertTo-Json | Set-Content -Encoding utf8 (Join-Path $resultDir 'sha256.json')
    Get-CimInstance Win32_ComputerSystem | Select-Object NumberOfLogicalProcessors,TotalPhysicalMemory |
        ConvertTo-Json | Set-Content -Encoding utf8 (Join-Path $resultDir 'host.json')
    $resourceJob = Start-Job -ArgumentList $resultDir,(Join-Path $repoRoot 'backend/.run/p0') -ScriptBlock {
        param($resourceDir,$binaryRoot)
        while ($true) {
            $at = [DateTimeOffset]::Now.ToString('o')
            Get-Process -Name api,worker,p0bench -ErrorAction SilentlyContinue |
                Where-Object { $_.Path -and $_.Path.StartsWith($binaryRoot,[StringComparison]::OrdinalIgnoreCase) } |
                Select-Object @{Name='At';Expression={$at}},Name,Id,CPU,WorkingSet64,PrivateMemorySize64,Handles,@{Name='Threads';Expression={$_.Threads.Count}} |
                Export-Csv -Path (Join-Path $resourceDir 'native-resources.csv') -NoTypeInformation -Append
            Start-Sleep -Milliseconds 1000
        }
    }
    $benchArguments = @('-seconds', $Seconds, '-levels', $Levels, '-write-requests', $WriteRequests, '-output', $resultDir, '-cases', $Cases, '-compose-project', $ProjectName)
    if ($ReadOnly) { $benchArguments += '-read-only' }
    if ($FaultChecks) { $benchArguments += '-fault-checks' }
    & ./.run/p0/p0bench.exe @benchArguments
    if ($LASTEXITCODE -ne 0) { throw "P0 runner failed; inspect $resultDir" }
} finally {
    if ($resourceJob) { Stop-Job $resourceJob; Remove-Job $resourceJob }
    Pop-Location
    if (-not $KeepDependencies) {
        & docker compose -p $ProjectName -f $composePath stop
    }
}
