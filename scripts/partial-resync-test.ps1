# partial-resync-test.ps1 - Validate 016 partial resync implementation
$ErrorActionPreference = "Stop"
$scriptDir = Split-Path -Parent $MyInvocation.MyCommand.Path
$srcDir = Join-Path $scriptDir "..\src"
$dataDir = Join-Path $scriptDir "..\.test\partial-resync"

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

# ========================================
# Scenario 1: Replica Restart → Partial Resync
# ========================================
Write-Host ""
Write-Host "=== SCENARIO 1: Replica Restart (Partial Resync) ===" -ForegroundColor Magenta

# 1.1 Start primary
Write-Host "--- 1.1 Starting Primary (4000) ---" -ForegroundColor Cyan
$p = Start-Process -FilePath $serverExe -ArgumentList "--port", "4000", "--data-dir", "$dataDir\p", "--debug" -PassThru -NoNewWindow -RedirectStandardError "$dataDir\p.log"
Write-Host "  Logs: $dataDir\p.log" -ForegroundColor Gray
Start-Sleep -Milliseconds 500

# 1.2 Write initial data
Write-Host "--- 1.2 Writing initial data (k1, k2) ---" -ForegroundColor Cyan
"put k1 v1" | & $cliExe -addr "127.0.0.1:4000" | Out-Null
"put k2 v2" | & $cliExe -addr "127.0.0.1:4000" | Out-Null
Start-Sleep -Milliseconds 200

# 1.3 Start replica
Write-Host "--- 1.3 Starting Replica (4001) ---" -ForegroundColor Cyan
$r1 = Start-Process -FilePath $serverExe -ArgumentList "--port", "4001", "--data-dir", "$dataDir\r1", "--replica-of", "127.0.0.1:4000", "--debug" -PassThru -NoNewWindow -RedirectStandardError "$dataDir\r1.log"
Write-Host "  Logs: $dataDir\r1.log" -ForegroundColor Gray
Write-Host "Primary: $($p.Id), Replica: $($r1.Id)" -ForegroundColor Yellow
Start-Sleep -Seconds 2

# 1.4 Verify initial replication
Write-Host "--- 1.4 Verify replica has k1, k2 ---" -ForegroundColor Cyan
$k1 = "get k1" | & $cliExe -addr "127.0.0.1:4001" 2>&1
$k2 = "get k2" | & $cliExe -addr "127.0.0.1:4001" 2>&1
if ($k1 -match "v1" -and $k2 -match "v2") {
    Write-Host "✓ Replica has initial data" -ForegroundColor Green
} else {
    Write-Host "✗ Initial replication failed" -ForegroundColor Red
    Stop-Process -Id $p.Id -Force
    Stop-Process -Id $r1.Id -Force
    exit 1
}

# 1.5 Stop replica (simulate restart)
Write-Host ""
Write-Host "--- 1.5 Stopping replica ---" -ForegroundColor Cyan
Stop-Process -Id $r1.Id -Force
Start-Sleep -Seconds 1

# 1.6 Write more data while replica is down
Write-Host "--- 1.6 Writing delta data (k3, k4) while replica is offline ---" -ForegroundColor Cyan
"put k3 v3" | & $cliExe -addr "127.0.0.1:4000" | Out-Null
"put k4 v4" | & $cliExe -addr "127.0.0.1:4000" | Out-Null
Start-Sleep -Milliseconds 200

# 1.7 Restart replica (should trigger partial resync)
Write-Host "--- 1.7 Restarting replica (expects partial resync) ---" -ForegroundColor Cyan -RedirectStandardError "$dataDir\r1.log"
$r1 = Start-Process -FilePath $serverExe -ArgumentList "--port", "4001", "--data-dir", "$dataDir\r1", "--replica-of", "127.0.0.1:4000", "--debug" -PassThru -NoNewWindow
Start-Sleep -Seconds 2

# 1.8 Verify replica received delta
Write-Host "--- 1.8 Verify replica has k3, k4 (delta only) ---" -ForegroundColor Cyan
$k3 = "get k3" | & $cliExe -addr "127.0.0.1:4001" 2>&1
$k4 = "get k4" | & $cliExe -addr "127.0.0.1:4001" 2>&1
if ($k3 -match "v3" -and $k4 -match "v4") {
    Write-Host "✓ PASS: Partial resync worked (replica caught up with delta)" -ForegroundColor Green
} else {
    Write-Host "✗ FAIL: Partial resync failed (replica missing k3/k4)" -ForegroundColor Red
}

# 1.9 Cleanup
Write-Host ""
Write-Host "--- 1.9 Cleaning up Scenario 1 ---" -ForegroundColor Cyan
Stop-Process -Id $p.Id -Force
Stop-Process -Id $r1.Id -Force
Start-Sleep -Seconds 1

# ========================================
# Scenario 2: Primary Restart → Full Resync
# ========================================
Write-Host ""
Write-Host "=== SCENARIO 2: Primary Restart (Full Resync) ===" -ForegroundColor Magenta

# Clean data dirs for fresh start
Remove-Item -Recurse -Force "$dataDir\p"
Remove-Item -Recurse -Force "$dataDir\r1"
New-Item -ItemType Directory -Path "$dataDir\p" | Out-Null
New-Item -ItemType Directory -Path "$dataDir\r1" | Out-Null

# 2.1 Start primary
Write-Host "--- 2.1 Starting Primary (4000) ---" -ForegroundColor Cyan
$p = Start-Process -FilePath $serverExe -ArgumentList "--port", "4000", "--data-dir", "$dataDir\p", "--debug" -PassThru -NoNewWindow -RedirectStandardError "$dataDir\p.log"
Write-Host "  Logs: $dataDir\p.log" -ForegroundColor Gray
Start-Sleep -Milliseconds 500

# 2.2 Write data
Write-Host "--- 2.2 Writing data (k1, k2) ---" -ForegroundColor Cyan
"put k1 v1" | & $cliExe -addr "127.0.0.1:4000" | Out-Null
"put k2 v2" | & $cliExe -addr "127.0.0.1:4000" | Out-Null
Start-Sleep -Milliseconds 200

# 2.3 Start replica
Write-Host "--- 2.3 Starting Replica (4001) ---" -ForegroundColor Cyan -RedirectStandardError "$dataDir\r1.log"
Write-Host "  Logs: $dataDir\r1.log" -ForegroundColor Gray
$r1 = Start-Process -FilePath $serverExe -ArgumentList "--port", "4001", "--data-dir", "$dataDir\r1", "--replica-of", "127.0.0.1:4000", "--debug" -PassThru -NoNewWindow
Start-Sleep -Seconds 2

# 2.4 Verify replication
Write-Host "--- 2.4 Verify replica has k1, k2 ---" -ForegroundColor Cyan
$k1 = "get k1" | & $cliExe -addr "127.0.0.1:4001" 2>&1
$k2 = "get k2" | & $cliExe -addr "127.0.0.1:4001" 2>&1
if ($k1 -match "v1" -and $k2 -match "v2") {
    Write-Host "✓ Replica synced" -ForegroundColor Green
} else {
    Write-Host "✗ Replication failed" -ForegroundColor Red
    Stop-Process -Id $p.Id -Force
    Stop-Process -Id $r1.Id -Force
    exit 1
}

# 2.5 Stop primary (simulate crash/restart)
Write-Host ""
Write-Host "--- 2.5 Stopping primary (simulate restart) ---" -ForegroundColor Cyan
Stop-Process -Id $p.Id -Force
Start-Sleep -Seconds 1

# 2.6 Restart primary (new replid generated)
Write-Host "--- 2.6 Restarting primary (NEW replid, ephemeral) ---" -ForegroundColor Cyan
Remove-Item -Recurse -Force "$dataDir\p"
New-Item -ItemType Directory -Path "$dataDir\p" | Out-Null
$p = Start-Process -FilePath $serverExe -ArgumentList "--port", "4000", "--data-dir", "$dataDir\p", "--debug" -PassThru -NoNewWindow -RedirectStandardError "$dataDir\p.log"
Start-Sleep -Milliseconds 500

# 2.7 Write new data
Write-Host "--- 2.7 Writing new data (k3) to restarted primary ---" -ForegroundColor Cyan
"put k3 v3" | & $cliExe -addr "127.0.0.1:4000" | Out-Null
Start-Sleep -Seconds 2

# 2.8 Check if replica detected replid mismatch and did full resync
Write-Host "--- 2.8 Check if replica did full resync (should only have k3, not k1/k2) ---" -ForegroundColor Cyan
$k1_after = "get k1" | & $cliExe -addr "127.0.0.1:4001" 2>&1
$k3_after = "get k3" | & $cliExe -addr "127.0.0.1:4001" 2>&1

if ($k1_after -match "not found" -and $k3_after -match "v3") {
    Write-Host "✓ PASS: Full resync triggered (old data cleared, new data present)" -ForegroundColor Green
} else {
    Write-Host "✗ FAIL: Full resync did not work correctly" -ForegroundColor Red
    Write-Host "  k1 result: $k1_after (should be 'not found')"
    Write-Host "  k3 result: $k3_after (should be 'v3')"
}

# 2.9 Cleanup
Write-Host ""
Write-Host "--- 2.9 Cleaning up Scenario 2 ---" -ForegroundColor Cyan
Stop-Process -Id $p.Id -Force
Stop-Process -Id $r1.Id -Force

# ========================================
# Summary
# ========================================
Write-Host ""
Write-Host "=== TEST SUMMARY ===" -ForegroundColor Magenta
Write-Host "Scenario 1: Replica restart → Partial resync should work"
Write-Host "Scenario 2: Primary restart → Full resync should trigger (ephemeral replid)"
Write-Host ""
Write-Host "If both scenarios passed, 016 is working as designed." -ForegroundColor Cyan
Write-Host "Known limitations (by design for 016):" -ForegroundColor Yellow
Write-Host "  - Unbounded backlog (OOM under heavy load)"
Write-Host "  - Ephemeral primary replid (restart breaks partial resync)"
Write-Host "  - O(n) seq lookup (slow with large backlog)"
Write-Host ""
Write-Host "Done." -ForegroundColor Green
