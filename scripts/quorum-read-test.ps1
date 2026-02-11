# quorum-read-test.ps1 - Validate Episode 027 quorum read implementation
$ErrorActionPreference = "Stop"
$scriptDir = Split-Path -Parent $MyInvocation.MyCommand.Path
$srcDir = Join-Path $scriptDir "..\src"
$dataDir = Join-Path $scriptDir "..\.test\quorum-read"

# Clean up
if (Test-Path $dataDir) { Remove-Item -Recurse -Force $dataDir }
New-Item -ItemType Directory -Path "$dataDir\p" | Out-Null
New-Item -ItemType Directory -Path "$dataDir\r1" | Out-Null
New-Item -ItemType Directory -Path "$dataDir\r2" | Out-Null

# Build
Write-Host "--- Building ---" -ForegroundColor Cyan
Push-Location $srcDir
go build -o kv-server.exe ./cmd/kv-server
go build -o kv-cli.exe ./cmd/kv-cli
Pop-Location
$serverExe = Join-Path $srcDir "kv-server.exe"
$cliExe = Join-Path $srcDir "kv-cli.exe"

# ========================================
# Test 1: Quorum Read from Primary (3 healthy nodes)
# ========================================
Write-Host ""
Write-Host "=== TEST 1: Quorum Read from Primary ===" -ForegroundColor Magenta

# 1.1 Start 3-node cluster
Write-Host "--- 1.1 Starting 3-Node Cluster ---" -ForegroundColor Cyan
$p = Start-Process -FilePath $serverExe -ArgumentList "--port", "4000", "--data-dir", "$dataDir\p" -PassThru -NoNewWindow
Start-Sleep -Milliseconds 300
$r1 = Start-Process -FilePath $serverExe -ArgumentList "--port", "4001", "--data-dir", "$dataDir\r1", "--replica-of", "127.0.0.1:4000" -PassThru -NoNewWindow
$r2 = Start-Process -FilePath $serverExe -ArgumentList "--port", "4002", "--data-dir", "$dataDir\r2", "--replica-of", "127.0.0.1:4000" -PassThru -NoNewWindow
Write-Host "Primary: $($p.Id), R1: $($r1.Id), R2: $($r2.Id)" -ForegroundColor Yellow
Start-Sleep -Seconds 1

# 1.2 Write data
Write-Host "--- 1.2 Writing test data ---" -ForegroundColor Cyan
"put qread1 value1" | & $cliExe -addr "127.0.0.1:4000" | Out-Null
Start-Sleep -Milliseconds 500

# 1.3 Quorum read from primary
Write-Host "--- 1.3 Quorum read from primary ---" -ForegroundColor Cyan
$val = "get --quorum qread1" | & $cliExe -addr "127.0.0.1:4000" 2>&1
if ($val -match "value1") {
    Write-Host "✓ Quorum read returned correct value" -ForegroundColor Green
} else {
    Write-Host "✗ Quorum read failed: $val" -ForegroundColor Red
    Stop-Process -Id $p.Id, $r1.Id, $r2.Id -Force -ErrorAction SilentlyContinue
    exit 1
}

# Cleanup
Stop-Process -Id $p.Id, $r1.Id, $r2.Id -Force -ErrorAction SilentlyContinue
Start-Sleep -Seconds 1

# ========================================
# Test 2: Quorum Read from Replica
# ========================================
Write-Host ""
Write-Host "=== TEST 2: Quorum Read from Replica ===" -ForegroundColor Magenta

# 2.1 Start 3-node cluster
Write-Host "--- 2.1 Starting 3-Node Cluster ---" -ForegroundColor Cyan
$p = Start-Process -FilePath $serverExe -ArgumentList "--port", "4000", "--data-dir", "$dataDir\p" -PassThru -NoNewWindow
Start-Sleep -Milliseconds 300
$r1 = Start-Process -FilePath $serverExe -ArgumentList "--port", "4001", "--data-dir", "$dataDir\r1", "--replica-of", "127.0.0.1:4000" -PassThru -NoNewWindow
$r2 = Start-Process -FilePath $serverExe -ArgumentList "--port", "4002", "--data-dir", "$dataDir\r2", "--replica-of", "127.0.0.1:4000" -PassThru -NoNewWindow
Write-Host "Primary: $($p.Id), R1: $($r1.Id), R2: $($r2.Id)" -ForegroundColor Yellow
Start-Sleep -Seconds 1

# 2.2 Write data
Write-Host "--- 2.2 Writing test data ---" -ForegroundColor Cyan
"put qread2 value2" | & $cliExe -addr "127.0.0.1:4000" | Out-Null
Start-Sleep -Milliseconds 500

# 2.3 Quorum read from replica
Write-Host "--- 2.3 Quorum read from replica (R1) ---" -ForegroundColor Cyan
$val = "get --quorum qread2" | & $cliExe -addr "127.0.0.1:4001" 2>&1
if ($val -match "value2") {
    Write-Host "✓ Replica quorum read returned correct value" -ForegroundColor Green
} else {
    Write-Host "✗ Replica quorum read failed: $val" -ForegroundColor Red
}

# Cleanup
Stop-Process -Id $p.Id, $r1.Id, $r2.Id -Force -ErrorAction SilentlyContinue
Start-Sleep -Seconds 1

# ========================================
# Test 3: Quorum Read vs Eventual Read (consistency)
# ========================================
Write-Host ""
Write-Host "=== TEST 3: Quorum Read Consistency (delayed replica) ===" -ForegroundColor Magenta

# 3.1 Start primary and one replica
Write-Host "--- 3.1 Starting Primary and R1 ---" -ForegroundColor Cyan
$p = Start-Process -FilePath $serverExe -ArgumentList "--port", "4000", "--data-dir", "$dataDir\p" -PassThru -NoNewWindow
Start-Sleep -Milliseconds 300
$r1 = Start-Process -FilePath $serverExe -ArgumentList "--port", "4001", "--data-dir", "$dataDir\r1", "--replica-of", "127.0.0.1:4000" -PassThru -NoNewWindow
Write-Host "Primary: $($p.Id), R1: $($r1.Id)" -ForegroundColor Yellow
Start-Sleep -Seconds 1

# 3.2 Write initial data
Write-Host "--- 3.2 Writing initial data (k1=old) ---" -ForegroundColor Cyan
"put k1 old" | & $cliExe -addr "127.0.0.1:4000" | Out-Null
Start-Sleep -Milliseconds 500

# 3.3 Stop R1 (simulate slow/partitioned replica)
Write-Host "--- 3.3 Stopping R1 (simulate partition) ---" -ForegroundColor Cyan
Stop-Process -Id $r1.Id -Force
Start-Sleep -Milliseconds 500

# 3.4 Update key on primary
Write-Host "--- 3.4 Updating key (k1=new) on primary ---" -ForegroundColor Cyan
"put k1 new" | & $cliExe -addr "127.0.0.1:4000" | Out-Null
Start-Sleep -Milliseconds 300

# 3.5 Start R2 (will have latest value)
Write-Host "--- 3.5 Starting R2 (will sync latest) ---" -ForegroundColor Cyan
$r2 = Start-Process -FilePath $serverExe -ArgumentList "--port", "4002", "--data-dir", "$dataDir\r2", "--replica-of", "127.0.0.1:4000" -PassThru -NoNewWindow
Start-Sleep -Seconds 1

# 3.6 Restart R1 (still has old value)
Write-Host "--- 3.6 Restarting R1 (will have old value initially) ---" -ForegroundColor Cyan
$r1 = Start-Process -FilePath $serverExe -ArgumentList "--port", "4001", "--data-dir", "$dataDir\r1", "--replica-of", "127.0.0.1:4000" -PassThru -NoNewWindow
Start-Sleep -Milliseconds 500

# 3.7 Eventual read from R1 might return stale
Write-Host "--- 3.7 Eventual read from R1 (may be stale) ---" -ForegroundColor Cyan
$val = "get k1" | & $cliExe -addr "127.0.0.1:4001" 2>&1
Write-Host "  Eventual read: $val" -ForegroundColor Gray

# 3.8 Quorum read should return latest (from majority)
Write-Host "--- 3.8 Quorum read from R1 (should be latest) ---" -ForegroundColor Cyan
$val = "get --quorum k1" | & $cliExe -addr "127.0.0.1:4001" 2>&1
if ($val -match "new") {
    Write-Host "✓ Quorum read returned latest value (linearizable)" -ForegroundColor Green
} else {
    Write-Host "✗ Quorum read returned: $val (expected 'new')" -ForegroundColor Yellow
}

# Cleanup
Stop-Process -Id $p.Id, $r1.Id, $r2.Id -Force -ErrorAction SilentlyContinue
Start-Sleep -Seconds 1

# ========================================
# Test 4: Quorum Read Timeout (insufficient nodes)
# ========================================
Write-Host ""
Write-Host "=== TEST 4: Quorum Read Timeout (1 of 3 nodes) ===" -ForegroundColor Magenta

# 4.1 Start 3-node cluster
Write-Host "--- 4.1 Starting 3-Node Cluster ---" -ForegroundColor Cyan
$p = Start-Process -FilePath $serverExe -ArgumentList "--port", "4000", "--data-dir", "$dataDir\p" -PassThru -NoNewWindow
Start-Sleep -Milliseconds 300
$r1 = Start-Process -FilePath $serverExe -ArgumentList "--port", "4001", "--data-dir", "$dataDir\r1", "--replica-of", "127.0.0.1:4000" -PassThru -NoNewWindow
$r2 = Start-Process -FilePath $serverExe -ArgumentList "--port", "4002", "--data-dir", "$dataDir\r2", "--replica-of", "127.0.0.1:4000" -PassThru -NoNewWindow
Write-Host "Primary: $($p.Id), R1: $($r1.Id), R2: $($r2.Id)" -ForegroundColor Yellow
Start-Sleep -Seconds 1

# 4.2 Write data
"put timeout_key timeout_val" | & $cliExe -addr "127.0.0.1:4000" | Out-Null
Start-Sleep -Milliseconds 500

# 4.3 Kill 2 nodes (only R1 remains)
Write-Host "--- 4.2 Killing Primary and R2 ---" -ForegroundColor Cyan
Stop-Process -Id $p.Id, $r2.Id -Force
Start-Sleep -Seconds 1

# 4.4 Quorum read from R1 should fail (1/3 < quorum 2)
Write-Host "--- 4.3 Attempting quorum read from R1 (should timeout) ---" -ForegroundColor Cyan
$val = "get --quorum timeout_key" | & $cliExe -addr "127.0.0.1:4001" 2>&1
if ($val -match "timeout|failed|error|quorum") {
    Write-Host "✓ Quorum read correctly failed (insufficient nodes)" -ForegroundColor Green
} else {
    Write-Host "✗ Expected failure but got: $val" -ForegroundColor Yellow
}

# Cleanup
Stop-Process -Id $r1.Id -Force -ErrorAction SilentlyContinue

# ========================================
# Test 5: Quorum Read Returns Highest Seq
# ========================================
Write-Host ""
Write-Host "=== TEST 5: Quorum Read Returns Highest Seq ===" -ForegroundColor Magenta

# 5.1 Start 3-node cluster
Write-Host "--- 5.1 Starting 3-Node Cluster ---" -ForegroundColor Cyan
$p = Start-Process -FilePath $serverExe -ArgumentList "--port", "4000", "--data-dir", "$dataDir\p" -PassThru -NoNewWindow
Start-Sleep -Milliseconds 300
$r1 = Start-Process -FilePath $serverExe -ArgumentList "--port", "4001", "--data-dir", "$dataDir\r1", "--replica-of", "127.0.0.1:4000" -PassThru -NoNewWindow
$r2 = Start-Process -FilePath $serverExe -ArgumentList "--port", "4002", "--data-dir", "$dataDir\r2", "--replica-of", "127.0.0.1:4000" -PassThru -NoNewWindow
Write-Host "Primary: $($p.Id), R1: $($r1.Id), R2: $($r2.Id)" -ForegroundColor Yellow
Start-Sleep -Seconds 1

# 5.2 Write multiple versions
Write-Host "--- 5.2 Writing multiple versions of same key ---" -ForegroundColor Cyan
"put seqkey v1" | & $cliExe -addr "127.0.0.1:4000" | Out-Null
Start-Sleep -Milliseconds 200
"put seqkey v2" | & $cliExe -addr "127.0.0.1:4000" | Out-Null
Start-Sleep -Milliseconds 200
"put seqkey v3" | & $cliExe -addr "127.0.0.1:4000" | Out-Null
Start-Sleep -Milliseconds 500

# 5.3 Quorum read should return v3 (highest seq)
Write-Host "--- 5.3 Quorum read (should return v3, highest seq) ---" -ForegroundColor Cyan
$val = "get --quorum seqkey" | & $cliExe -addr "127.0.0.1:4000" 2>&1
if ($val -match "v3") {
    Write-Host "✓ Quorum read returned highest seq value" -ForegroundColor Green
} else {
    Write-Host "✗ Quorum read returned: $val (expected 'v3')" -ForegroundColor Yellow
}

# Cleanup
Stop-Process -Id $p.Id, $r1.Id, $r2.Id -Force -ErrorAction SilentlyContinue

Write-Host ""
Write-Host "=== All Quorum Read Tests Complete ===" -ForegroundColor Green
