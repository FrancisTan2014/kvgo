# crash-test.ps1 - Kill a replica mid-benchmark and observe behavior
param(
    [int]$Requests = 50000,
    [int]$Concurrency = 50,
    [int]$KillAfterMs = 500  # Kill replica after this many milliseconds
)

$ErrorActionPreference = "Stop"
$scriptDir = Split-Path -Parent $MyInvocation.MyCommand.Path
$srcDir = Join-Path $scriptDir "..\src"
$dataDir = Join-Path $scriptDir "..\.bench-data"

# Clean up
if (Test-Path $dataDir) { Remove-Item -Recurse -Force $dataDir }
New-Item -ItemType Directory -Path $dataDir | Out-Null
New-Item -ItemType Directory -Path "$dataDir\primary" | Out-Null
New-Item -ItemType Directory -Path "$dataDir\replica" | Out-Null

# Build
Write-Host "--- Building ---" -ForegroundColor Cyan
Push-Location $srcDir
go build -o kv-server.exe ./cmd/kv-server
go build -o kv-bench.exe ./cmd/kv-bench
Pop-Location

$serverExe = Join-Path $srcDir "kv-server.exe"
$benchExe = Join-Path $srcDir "kv-bench.exe"

# Log files
$primaryLog = Join-Path $dataDir "primary.log"
$primaryErr = Join-Path $dataDir "primary.err"
$replicaLog = Join-Path $dataDir "replica.log"
$replicaErr = Join-Path $dataDir "replica.err"

Write-Host "--- Starting Primary (port 4000) ---" -ForegroundColor Cyan
$primary = Start-Process -FilePath $serverExe -ArgumentList "--port", "4000", "--data-dir", "$dataDir\primary", "--debug" `
    -RedirectStandardOutput $primaryLog -RedirectStandardError $primaryErr -PassThru -NoNewWindow

Start-Sleep -Milliseconds 500

Write-Host "--- Starting Replica (port 4001) ---" -ForegroundColor Cyan
$replica = Start-Process -FilePath $serverExe -ArgumentList "--port", "4001", "--data-dir", "$dataDir\replica", "--replica-of", "127.0.0.1:4000", "--debug" `
    -RedirectStandardOutput $replicaLog -RedirectStandardError $replicaErr -PassThru -NoNewWindow

Write-Host "Replica PID: $($replica.Id)" -ForegroundColor Yellow
Start-Sleep -Milliseconds 1000

# Check if replica is still alive
if ($replica.HasExited) {
    Write-Host "WARNING: Replica exited immediately with code $($replica.ExitCode)" -ForegroundColor Red
    Write-Host "--- Replica Log ---" -ForegroundColor Red
    if (Test-Path $replicaLog) { Get-Content $replicaLog }
    
    # Continue anyway to see what primary does without replica
    Write-Host ""
    Write-Host "Continuing benchmark WITHOUT replica..." -ForegroundColor Yellow
}

Write-Host "--- Starting Benchmark ---" -ForegroundColor Cyan
Write-Host "Will kill replica after $KillAfterMs ms" -ForegroundColor Yellow

# Start benchmark in background
$benchJob = Start-Job -ScriptBlock {
    param($exe, $requests, $concurrency)
    & $exe -addr "127.0.0.1:4000" -n $requests -c $concurrency 2>&1
} -ArgumentList $benchExe, $Requests, $Concurrency

# Wait, then kill replica
Start-Sleep -Milliseconds $KillAfterMs
Write-Host "--- Killing Replica (PID: $($replica.Id)) ---" -ForegroundColor Red

if (-not $replica.HasExited) {
    Stop-Process -Id $replica.Id -Force
    $killTime = Get-Date -Format "HH:mm:ss.fff"
    Write-Host "Replica killed at $killTime" -ForegroundColor Red
} else {
    Write-Host "Replica already exited on its own!" -ForegroundColor Yellow
    $killTime = "N/A (already dead)"
}

# Wait for benchmark to finish
Write-Host "--- Waiting for benchmark to complete ---" -ForegroundColor Cyan
$benchOutput = Receive-Job -Job $benchJob -Wait
Remove-Job -Job $benchJob

Write-Host ""
Write-Host "--- Benchmark Results ---" -ForegroundColor Green
$benchOutput | ForEach-Object { Write-Host $_ }

# Stop primary
Write-Host ""
Write-Host "--- Stopping Primary ---" -ForegroundColor Cyan
if (-not $primary.HasExited) {
    Stop-Process -Id $primary.Id -Force -ErrorAction SilentlyContinue
}

Start-Sleep -Milliseconds 500

# Logs saved to files (not printed to terminal)
# Primary: $primaryLog, $primaryErr
# Replica: $replicaLog, $replicaErr

Write-Host ""
Write-Host "--- Summary ---" -ForegroundColor Cyan
Write-Host "Replica was killed at: $killTime"
Write-Host "Logs saved to: $dataDir"
