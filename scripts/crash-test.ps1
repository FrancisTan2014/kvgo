# crash-test.ps1 - Kill a node mid-benchmark and observe behavior
param(
    [int]$Requests = 50000,
    [int]$Concurrency = 50,
    [int]$KillAfterMs = 500,
    [switch]$KillPrimary  # New switch to kill primary instead of replica
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
go build -o kv-cli.exe ./cmd/kv-cli
Pop-Location

$serverExe = Join-Path $srcDir "kv-server.exe"
$benchExe = Join-Path $srcDir "kv-bench.exe"
$cliExe = Join-Path $srcDir "kv-cli.exe"

# Log files
$primaryLog = Join-Path $dataDir "primary.log"
$primaryErr = Join-Path $dataDir "primary.err"
$replicaLog = Join-Path $dataDir "replica.log"
$replicaErr = Join-Path $dataDir "replica.err"

Write-Host "--- Starting Primary (port 4000) ---" -ForegroundColor Cyan
$primary = Start-Process -FilePath $serverExe -ArgumentList "--port", "4000", "--data-dir", "$dataDir\primary", "--debug" `
    -RedirectStandardOutput $primaryLog -RedirectStandardError $primaryErr -PassThru -NoNewWindow
Write-Host "Primary PID: $($primary.Id)" -ForegroundColor Yellow

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
    Write-Host "Continuing anyway..." -ForegroundColor Yellow
}

Write-Host "--- Starting Benchmark ---" -ForegroundColor Cyan
$targetDesc = if ($KillPrimary) { "Primary" } else { "Replica" }
Write-Host "Will kill $targetDesc after $KillAfterMs ms" -ForegroundColor Yellow

# Start benchmark in background
$benchJob = Start-Job -ScriptBlock {
    param($exe, $requests, $concurrency)
    & $exe -addr "127.0.0.1:4000" -n $requests -c $concurrency 2>&1
} -ArgumentList $benchExe, $Requests, $Concurrency

# Wait, then kill target
Start-Sleep -Milliseconds $KillAfterMs

if ($KillPrimary) {
    Write-Host "--- Killing Primary (PID: $($primary.Id)) ---" -ForegroundColor Red
    Stop-Process -Id $primary.Id -Force
    $killTime = Get-Date -Format "HH:mm:ss.fff"
    
    # Wait for benchmark to fail/finish
    $benchOutput = Receive-Job -Job $benchJob -Wait
    Remove-Job -Job $benchJob
    
    Write-Host ""
    Write-Host "--- Benchmark Partial Results (Expected Failure) ---" -ForegroundColor DarkGray
    $benchOutput | Select-Object -First 5 | ForEach-Object { Write-Host $_ }
    
    Write-Host ""
    Write-Host "--- Attempting Recovery via OpPromote ---" -ForegroundColor Cyan
    
    # 1. Verify Replica is still Read-Only
    Write-Host "1. Verifying Replica (4001) rejects writes... " -NoNewline
    $out = "put probe_fail 1" | & $cliExe -addr "127.0.0.1:4001" 2>&1
    if ($out -match "server error") { 
        Write-Host "OK (Writes Rejected)" -ForegroundColor Green 
    } else { 
        Write-Host "UNEXPECTED ($out)" -ForegroundColor Red 
    }

    # 2. Promote Replica
    Write-Host "2. Sending Promote command... " -NoNewline
    $out = "promote" | & $cliExe -addr "127.0.0.1:4001" 2>&1
    if ($out -match "OK") { 
        Write-Host "OK (Promoted)" -ForegroundColor Green 
    } else { 
        Write-Host "FAIL ($out)" -ForegroundColor Red 
    }

    # 3. Verify Replica Accepts Writes
    Write-Host "3. Verifying Replica accepts writes... " -NoNewline
    $out = "put recovery_key works" | & $cliExe -addr "127.0.0.1:4001" 2>&1
    if ($out -match "OK") { 
        Write-Host "SUCCESS (Write Accepted)" -ForegroundColor Green 
    } else { 
        Write-Host "FAIL ($out)" -ForegroundColor Red 
    }
    
    # 4. Verify Data (Read back)
    Write-Host "4. Reading back data... " -NoNewline
    $out = "get recovery_key" | & $cliExe -addr "127.0.0.1:4001" 2>&1
    if ($out -match "works") { 
        Write-Host "SUCCESS (Data Verified)" -ForegroundColor Green 
    } else { 
        Write-Host "FAIL ($out)" -ForegroundColor Red 
    }

} else {
    # Original Replica Kill Logic
    Write-Host "--- Killing Replica (PID: $($replica.Id)) ---" -ForegroundColor Red
    if (-not $replica.HasExited) {
        Stop-Process -Id $replica.Id -Force
        $killTime = Get-Date -Format "HH:mm:ss.fff"
        Write-Host "Replica killed at $killTime" -ForegroundColor Red
    }
    
    $benchOutput = Receive-Job -Job $benchJob -Wait
    Remove-Job -Job $benchJob
    Write-Host "--- Benchmark Results ---" -ForegroundColor Green
    $benchOutput | ForEach-Object { Write-Host $_ }
}

# Cleanup
if (-not $primary.HasExited) { Stop-Process -Id $primary.Id -Force -ErrorAction SilentlyContinue }
if (-not $replica.HasExited) { Stop-Process -Id $replica.Id -Force -ErrorAction SilentlyContinue }

Write-Host ""
Write-Host "--- Summary ---" -ForegroundColor Cyan
Write-Host "Logs saved to: $dataDir"
