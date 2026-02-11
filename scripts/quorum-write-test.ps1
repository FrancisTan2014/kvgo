# quorum-write-test.ps1 - Validate Episode 026 quorum write implementation
$ErrorActionPreference = "Stop"
$scriptDir = Split-Path -Parent $MyInvocation.MyCommand.Path
$srcDir = Join-Path $scriptDir "..\src"
$dataDir = Join-Path $scriptDir "..\.test\quorum-write"

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
# Test 1: Quorum Write Success (3 nodes, all healthy)
# ========================================
Write-Host ""
Write-Host "=== TEST 1: Quorum Write Success (3 healthy nodes) ===" -ForegroundColor Magenta

# 1.1 Start 3-node cluster
Write-Host "--- 1.1 Starting 3-Node Cluster ---" -ForegroundColor Cyan
$p = Start-Process -FilePath $serverExe -ArgumentList "--port", "4000", "--data-dir", "$dataDir\p" -PassThru -NoNewWindow
Start-Sleep -Milliseconds 300
$r1 = Start-Process -FilePath $serverExe -ArgumentList "--port", "4001", "--data-dir", "$dataDir\r1", "--replica-of", "127.0.0.1:4000" -PassThru -NoNewWindow
$r2 = Start-Process -FilePath $serverExe -ArgumentList "--port", "4002", "--data-dir", "$dataDir\r2", "--replica-of", "127.0.0.1:4000" -PassThru -NoNewWindow
Write-Host "Primary: $($p.Id), R1: $($r1.Id), R2: $($r2.Id)" -ForegroundColor Yellow
Start-Sleep -Seconds 1

# 1.2 Quorum write (should succeed: 3/3 nodes)
Write-Host "--- 1.2 Writing with --quorum flag ---" -ForegroundColor Cyan
$out = "put --quorum qkey1 qval1" | & $cliExe -addr "127.0.0.1:4000" 2>&1
if ($out -match "OK") {
    Write-Host "✓ Quorum write succeeded" -ForegroundColor Green
} else {
    Write-Host "✗ Quorum write failed: $out" -ForegroundColor Red
    Stop-Process -Id $p.Id, $r1.Id, $r2.Id -Force -ErrorAction SilentlyContinue
    exit 1
}

# 1.3 Verify majority of nodes have the key
Write-Host "--- 1.3 Verifying quorum replication ---" -ForegroundColor Cyan
Start-Sleep -Milliseconds 500
$count = 0
$val = "get qkey1" | & $cliExe -addr "127.0.0.1:4000" 2>&1
if ($val -match "qval1") { $count++; Write-Host "  Primary: has key" -ForegroundColor Gray }

$val = "get qkey1" | & $cliExe -addr "127.0.0.1:4001" 2>&1
if ($val -match "qval1") { $count++; Write-Host "  R1: has key" -ForegroundColor Gray }

$val = "get qkey1" | & $cliExe -addr "127.0.0.1:4002" 2>&1
if ($val -match "qval1") { $count++; Write-Host "  R2: has key" -ForegroundColor Gray }

$quorum = 2 # (3+1)/2 = 2
if ($count -ge $quorum) {
    Write-Host "✓ Quorum replication: $count/3 nodes (need $quorum)" -ForegroundColor Green
} else {
    Write-Host "✗ Insufficient replication: $count/3 nodes (need $quorum)" -ForegroundColor Red
}

# Cleanup
Stop-Process -Id $p.Id, $r1.Id, $r2.Id -Force -ErrorAction SilentlyContinue
Start-Sleep -Seconds 1

# ========================================
# Test 2: Quorum Write Timeout (majority unavailable)
# ========================================
Write-Host ""
Write-Host "=== TEST 2: Quorum Write Timeout (1 of 3 nodes) ===" -ForegroundColor Magenta

# 2.1 Start only primary (no replicas)
Write-Host "--- 2.1 Starting Primary Only ---" -ForegroundColor Cyan
$p = Start-Process -FilePath $serverExe -ArgumentList "--port", "4000", "--data-dir", "$dataDir\p" -PassThru -NoNewWindow
Start-Sleep -Milliseconds 300
Write-Host "Primary: $($p.Id)" -ForegroundColor Yellow

# 2.2 Start replicas to register, then kill them
Write-Host "--- 2.2 Starting replicas, then killing them ---" -ForegroundColor Cyan
$r1 = Start-Process -FilePath $serverExe -ArgumentList "--port", "4001", "--data-dir", "$dataDir\r1", "--replica-of", "127.0.0.1:4000" -PassThru -NoNewWindow
$r2 = Start-Process -FilePath $serverExe -ArgumentList "--port", "4002", "--data-dir", "$dataDir\r2", "--replica-of", "127.0.0.1:4000" -PassThru -NoNewWindow
Start-Sleep -Seconds 1

Stop-Process -Id $r1.Id, $r2.Id -Force
Write-Host "Killed R1 and R2" -ForegroundColor Red
Start-Sleep -Seconds 1

# 2.3 Quorum write should timeout (1/3 nodes < quorum 2)
Write-Host "--- 2.3 Attempting quorum write (should timeout) ---" -ForegroundColor Cyan
$out = "put --quorum timeout_key timeout_val" | & $cliExe -addr "127.0.0.1:4000" 2>&1
if ($out -match "timeout|error|failed") {
    Write-Host "✓ Quorum write correctly timed out" -ForegroundColor Green
} else {
    Write-Host "✗ Expected timeout but got: $out" -ForegroundColor Yellow
}

# Cleanup
Stop-Process -Id $p.Id -Force -ErrorAction SilentlyContinue
Start-Sleep -Seconds 1

# ========================================
# Test 3: Async Write (doesn't wait for replicas)
# ========================================
Write-Host ""
Write-Host "=== TEST 3: Async Write (no --quorum flag) ===" -ForegroundColor Magenta

# 3.1 Start primary only
Write-Host "--- 3.1 Starting Primary Only ---" -ForegroundColor Cyan
$p = Start-Process -FilePath $serverExe -ArgumentList "--port", "4000", "--data-dir", "$dataDir\p" -PassThru -NoNewWindow
Start-Sleep -Milliseconds 300
Write-Host "Primary: $($p.Id)" -ForegroundColor Yellow

# 3.2 Async write (should succeed immediately)
Write-Host "--- 3.2 Writing WITHOUT --quorum flag ---" -ForegroundColor Cyan
$out = "put async_key async_val" | & $cliExe -addr "127.0.0.1:4000" 2>&1
if ($out -match "OK") {
    Write-Host "✓ Async write succeeded immediately" -ForegroundColor Green
} else {
    Write-Host "✗ Async write failed: $out" -ForegroundColor Red
}

# 3.3 Verify primary has the key
$val = "get async_key" | & $cliExe -addr "127.0.0.1:4000" 2>&1
if ($val -match "async_val") {
    Write-Host "✓ Primary persisted the write" -ForegroundColor Green
} else {
    Write-Host "✗ Key not found on primary" -ForegroundColor Red
}

# Cleanup
Stop-Process -Id $p.Id -Force -ErrorAction SilentlyContinue

# ========================================
# Test 4: Partial Failure (2 of 3 nodes, quorum succeeds)
# ========================================
Write-Host ""
Write-Host "=== TEST 4: Partial Failure (2 of 3 nodes, quorum succeeds) ===" -ForegroundColor Magenta

# 4.1 Start 3-node cluster
Write-Host "--- 4.1 Starting 3-Node Cluster ---" -ForegroundColor Cyan
$p = Start-Process -FilePath $serverExe -ArgumentList "--port", "4000", "--data-dir", "$dataDir\p" -PassThru -NoNewWindow
Start-Sleep -Milliseconds 300
$r1 = Start-Process -FilePath $serverExe -ArgumentList "--port", "4001", "--data-dir", "$dataDir\r1", "--replica-of", "127.0.0.1:4000" -PassThru -NoNewWindow
$r2 = Start-Process -FilePath $serverExe -ArgumentList "--port", "4002", "--data-dir", "$dataDir\r2", "--replica-of", "127.0.0.1:4000" -PassThru -NoNewWindow
Write-Host "Primary: $($p.Id), R1: $($r1.Id), R2: $($r2.Id)" -ForegroundColor Yellow
Start-Sleep -Seconds 1

# 4.2 Kill one replica
Write-Host "--- 4.2 Killing one replica (R2) ---" -ForegroundColor Cyan
Stop-Process -Id $r2.Id -Force
Start-Sleep -Milliseconds 500

# 4.3 Quorum write should still succeed (2/3 nodes >= quorum 2)
Write-Host "--- 4.3 Quorum write with 2/3 nodes ---" -ForegroundColor Cyan
$out = "put --quorum partial_key partial_val" | & $cliExe -addr "127.0.0.1:4000" 2>&1
if ($out -match "OK") {
    Write-Host "✓ Quorum write succeeded with 2/3 nodes" -ForegroundColor Green
} else {
    Write-Host "✗ Quorum write failed: $out" -ForegroundColor Red
}

# Cleanup
Stop-Process -Id $p.Id, $r1.Id -Force -ErrorAction SilentlyContinue

Write-Host ""
Write-Host "=== All Quorum Write Tests Complete ===" -ForegroundColor Green
