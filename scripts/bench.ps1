<#
.SYNOPSIS
    Automated benchmark for kv-server with optional replication.

.DESCRIPTION
    Starts primary server, optionally starts replica(s), runs kv-bench,
    and reports results. Cleans up processes and data on exit.

.PARAMETER Requests
    Total number of requests (default: 100000)

.PARAMETER Concurrency
    Number of concurrent connections (default: 50)

.PARAMETER ValueSize
    Value payload size in bytes (default: 128)

.PARAMETER SyncInterval
    WAL sync interval (default: 10ms)

.PARAMETER Replicas
    Number of replicas to start (default: 0)

.PARAMETER GetRatio
    Fraction of GET requests (default: 0.0)

.PARAMETER Debug
    Enable debug logging on servers

.EXAMPLE
    .\bench.ps1 -Requests 50000 -Concurrency 100
    .\bench.ps1 -Replicas 2 -SyncInterval 1ms
#>

param(
    [int]$Requests = 100000,
    [int]$Concurrency = 50,
    [int]$ValueSize = 128,
    [string]$SyncInterval = "10ms",
    [int]$Replicas = 0,
    [double]$GetRatio = 0.0,
    [switch]$Debug
)

$ErrorActionPreference = "Stop"

# Paths
$root = Split-Path -Parent $PSScriptRoot
$srcDir = Join-Path $root "src"
$dataRoot = Join-Path $root ".test\bench"
$primaryPort = 4000
$replicaBasePort = 4001

# Track processes for cleanup
$processes = @()

function Cleanup {
    Write-Host "`n--- Cleanup ---" -ForegroundColor Yellow
    foreach ($proc in $script:processes) {
        if (!$proc.HasExited) {
            Write-Host "Stopping process $($proc.Id)..."
            Stop-Process -Id $proc.Id -Force -ErrorAction SilentlyContinue
        }
    }
    if (Test-Path $dataRoot) {
        Write-Host "Removing $dataRoot..."
        Remove-Item -Recurse -Force $dataRoot -ErrorAction SilentlyContinue
    }
}

# Register cleanup on exit
trap { Cleanup; break }

try {
    # Build
    Write-Host "--- Building ---" -ForegroundColor Cyan
    Push-Location $srcDir
    go build -o (Join-Path $srcDir "kv-server.exe") ./cmd/kv-server
    go build -o (Join-Path $srcDir "kv-bench.exe") ./cmd/kv-bench
    Pop-Location

    # Clean data dir
    if (Test-Path $dataRoot) {
        Remove-Item -Recurse -Force $dataRoot
    }
    New-Item -ItemType Directory -Path $dataRoot | Out-Null

    # Start primary
    Write-Host "`n--- Starting Primary (port $primaryPort) ---" -ForegroundColor Cyan
    $primaryDataDir = Join-Path $dataRoot "primary"
    New-Item -ItemType Directory -Path $primaryDataDir | Out-Null

    $primaryArgs = @(
        "-port", $primaryPort,
        "-data-dir", $primaryDataDir,
        "-sync", $SyncInterval
    )
    if ($Debug) { $primaryArgs += "-debug" }

    $primary = Start-Process -FilePath (Join-Path $srcDir "kv-server.exe") `
        -ArgumentList $primaryArgs `
        -PassThru -NoNewWindow -RedirectStandardOutput (Join-Path $dataRoot "primary.log")
    $processes += $primary
    Start-Sleep -Milliseconds 500

    if ($primary.HasExited) {
        Write-Host "Primary failed to start!" -ForegroundColor Red
        Get-Content (Join-Path $dataRoot "primary.log")
        exit 1
    }
    Write-Host "Primary started (PID: $($primary.Id))"

    # Start replicas
    $replicaProcs = @()
    for ($i = 0; $i -lt $Replicas; $i++) {
        $replicaPort = $replicaBasePort + $i
        Write-Host "`n--- Starting Replica $($i+1) (port $replicaPort) ---" -ForegroundColor Cyan
        
        $replicaDataDir = Join-Path $dataRoot "replica$($i+1)"
        New-Item -ItemType Directory -Path $replicaDataDir | Out-Null

        $replicaArgs = @(
            "-port", $replicaPort,
            "-data-dir", $replicaDataDir,
            "-sync", $SyncInterval,
            "-replica-of", "127.0.0.1:$primaryPort"
        )
        if ($Debug) { $replicaArgs += "-debug" }

        $replica = Start-Process -FilePath (Join-Path $srcDir "kv-server.exe") `
            -ArgumentList $replicaArgs `
            -PassThru -NoNewWindow -RedirectStandardOutput (Join-Path $dataRoot "replica$($i+1).log")
        $processes += $replica
        $replicaProcs += $replica
        Start-Sleep -Milliseconds 500

        if ($replica.HasExited) {
            Write-Host "Replica $($i+1) failed to start!" -ForegroundColor Red
            Get-Content (Join-Path $dataRoot "replica$($i+1).log")
            exit 1
        }
        Write-Host "Replica $($i+1) started (PID: $($replica.Id))"
    }

    # Wait for replicas to connect
    if ($Replicas -gt 0) {
        Write-Host "`nWaiting for replicas to connect..."
        Start-Sleep -Seconds 1
    }

    # Run benchmark
    Write-Host "`n--- Running Benchmark ---" -ForegroundColor Cyan
    Write-Host "  Requests:    $Requests"
    Write-Host "  Concurrency: $Concurrency"
    Write-Host "  Value Size:  $ValueSize bytes"
    Write-Host "  Sync:        $SyncInterval"
    Write-Host "  Replicas:    $Replicas"
    Write-Host "  GET Ratio:   $($GetRatio * 100)%"
    Write-Host ""

    $benchArgs = @(
        "-addr", "127.0.0.1:$primaryPort",
        "-n", $Requests,
        "-c", $Concurrency,
        "-size", $ValueSize,
        "-get-ratio", $GetRatio
    )

    & (Join-Path $srcDir "kv-bench.exe") @benchArgs

    # Show logs if debug
    if ($Debug) {
        Write-Host "`n--- Primary Log (last 20 lines) ---" -ForegroundColor Yellow
        Get-Content (Join-Path $dataRoot "primary.log") -Tail 20

        for ($i = 0; $i -lt $Replicas; $i++) {
            Write-Host "`n--- Replica $($i+1) Log (last 20 lines) ---" -ForegroundColor Yellow
            Get-Content (Join-Path $dataRoot "replica$($i+1).log") -Tail 20
        }
    }

} finally {
    Cleanup
}
