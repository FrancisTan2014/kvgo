# epoch-test.ps1 - Verify epoch detection during split-brain reconnection
# DEPRECATED: Uses manual `promote` + `replicaof` to trigger epoch mismatch.
# With automatic elections, epoch detection is tested implicitly by:
#   primary-rejoin-test.ps1 (crashed primary does full resync on rejoin)
$ErrorActionPreference = "Stop"
$scriptDir = Split-Path -Parent $MyInvocation.MyCommand.Path
$srcDir = Join-Path $scriptDir "..\src"
$dataDir = Join-Path $scriptDir "..\.test\epoch"

# Clean up
if (Test-Path $dataDir) { Remove-Item -Recurse -Force $dataDir }
New-Item -ItemType Directory -Path "$dataDir\p" | Out-Null
New-Item -ItemType Directory -Path "$dataDir\r1" | Out-Null

# Build
Write-Host "--- Building ---" -ForegroundColor Cyan
Push-Location $srcDir
go build -o kv-server.exe ./cmd/kv-server
go build -o kv-cli.exe ./cmd/kv-cli
Pop-Location
$serverExe = Join-Path $srcDir "kv-server.exe"
$cliExe = Join-Path $srcDir "kv-cli.exe"

# 1. Start Cluster with logging to files
Write-Host "--- 1. Starting P (4000) and R1 (4001) ---" -ForegroundColor Cyan
$pLog = "$dataDir\primary.log"
$r1Log = "$dataDir\replica.log"

$p = Start-Process -FilePath $serverExe `
    -ArgumentList "--port", "4000", "--data-dir", "$dataDir\p", "--debug" `
    -RedirectStandardOutput $pLog `
    -RedirectStandardError "$dataDir\primary-err.log" `
    -PassThru -NoNewWindow

Start-Sleep -Milliseconds 300

$r1 = Start-Process -FilePath $serverExe `
    -ArgumentList "--port", "4001", "--data-dir", "$dataDir\r1", "--replica-of", "127.0.0.1:4000", "--debug" `
    -RedirectStandardOutput $r1Log `
    -RedirectStandardError "$dataDir\replica-err.log" `
    -PassThru -NoNewWindow

Write-Host "P: $($p.Id), R1: $($r1.Id)" -ForegroundColor Yellow
Start-Sleep -Seconds 1

# 2. Write baseline
Write-Host "--- 2. Writing baseline data ---" -ForegroundColor Cyan
"put baseline sync" | & $cliExe -addr "127.0.0.1:4000" | Out-Null
Start-Sleep -Milliseconds 300

# 3. Promote R1 (simulate partition)
Write-Host "--- 3. Promoting R1 (split brain) ---" -ForegroundColor Red
"promote" | & $cliExe -addr "127.0.0.1:4001" | Out-Null
Write-Host "R1 is now a Primary. TWO primaries exist." -ForegroundColor Yellow

# 4. Write to both sides
Write-Host "--- 4. Divergent writes ---" -ForegroundColor Cyan
"put key_p 100" | & $cliExe -addr "127.0.0.1:4000" | Out-Null
"put key_r 200" | & $cliExe -addr "127.0.0.1:4001" | Out-Null

# 5. Reconnect R1 to P (triggers epoch check)
Write-Host "--- 5. Reconnecting R1 to P (epoch check should fire) ---" -ForegroundColor Cyan
"replicaof 127.0.0.1:4000" | & $cliExe -addr "127.0.0.1:4001" | Out-Null
Start-Sleep -Seconds 1

# 6. Check logs for epoch detection
Write-Host ""
Write-Host "--- 6. Checking logs for epoch detection ---" -ForegroundColor Cyan
$epochLog = Get-Content "$dataDir\primary-err.log" | Select-String "incompatible replid"
if ($epochLog) {
    Write-Host "✓ DETECTED: Incompatible replid logged by primary:" -ForegroundColor Green
    $epochLog | ForEach-Object { Write-Host "  $_" -ForegroundColor Yellow }
} else {
    Write-Host "✗ NOT DETECTED: No 'incompatible replid' message in logs" -ForegroundColor Red
}

# 7. Verify full resync happened
Write-Host ""
Write-Host "--- 7. Verify full resync ---" -ForegroundColor Cyan
$r1KeyP = "get key_p" | & $cliExe -addr "127.0.0.1:4001" 2>&1
$r1KeyR = "get key_r" | & $cliExe -addr "127.0.0.1:4001" 2>&1

if ($r1KeyP -match "100") {
    Write-Host "✓ R1 received P's data (key_p=100)" -ForegroundColor Green
} else {
    Write-Host "✗ R1 did not sync key_p" -ForegroundColor Red
}

if ($r1KeyR -match "not found") {
    Write-Host "✓ R1's stale data cleared (key_r gone)" -ForegroundColor Green
} else {
    Write-Host "✗ R1 still has stale data" -ForegroundColor Red
}

# Cleanup
Write-Host ""
Write-Host "--- Cleaning up ---" -ForegroundColor Cyan
Stop-Process -Id $p.Id -Force -ErrorAction SilentlyContinue
Stop-Process -Id $r1.Id -Force -ErrorAction SilentlyContinue

Write-Host ""
Write-Host "=== SUMMARY ===" -ForegroundColor Cyan
Write-Host "Epoch detection prevents silent data corruption by:"
Write-Host "  1. Generating new replid on promote"
Write-Host "  2. Detecting incompatible replid on reconnect"
Write-Host "  3. Forcing full resync (clears stale data)"
Write-Host ""
Write-Host "Logs available at:"
Write-Host "  Primary: $pLog"
Write-Host "  Replica: $r1Log"
