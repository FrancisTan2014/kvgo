# split-brain-test.ps1 - Observe data loss from split brain scenario
$ErrorActionPreference = "Stop"
$scriptDir = Split-Path -Parent $MyInvocation.MyCommand.Path
$srcDir = Join-Path $scriptDir "..\src"
$dataDir = Join-Path $scriptDir "..\.bench-data"

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

# 1. Start Cluster (P + R1)
Write-Host "--- 1. Starting P (4000) and R1 (4001) ---" -ForegroundColor Cyan
$p = Start-Process -FilePath $serverExe -ArgumentList "--port", "4000", "--data-dir", "$dataDir\p" -PassThru -NoNewWindow
Start-Sleep -Milliseconds 200
$r1 = Start-Process -FilePath $serverExe -ArgumentList "--port", "4001", "--data-dir", "$dataDir\r1", "--replica-of", "127.0.0.1:4000" -PassThru -NoNewWindow
Write-Host "P: $($p.Id), R1: $($r1.Id)" -ForegroundColor Yellow
Start-Sleep -Seconds 1

# 2. Baseline: Write to P, verify R1 sees it
Write-Host "--- 2. Baseline: Write 'baseline=sync' to P ---" -ForegroundColor Cyan
"put baseline sync" | & $cliExe -addr "127.0.0.1:4000" | Out-Null
Start-Sleep -Milliseconds 500
$val = "get baseline" | & $cliExe -addr "127.0.0.1:4001" 2>&1
if ($val -match "sync") {
    Write-Host "R1 sees baseline. Replication is working." -ForegroundColor Green
} else {
    Write-Host "ERROR: R1 did not replicate baseline." -ForegroundColor Red
}

# 3. Simulate Partition: Promote R1 (but keep P alive)
Write-Host "" 
Write-Host "--- 3. SIMULATE PARTITION: Promote R1 (P is still alive) ---" -ForegroundColor Red
"promote" | & $cliExe -addr "127.0.0.1:4001" | Out-Null
Write-Host "R1 is now a Primary. We have TWO primaries." -ForegroundColor Yellow

# 4. Split Brain: Write to both sides
Write-Host ""
Write-Host "--- 4. SPLIT BRAIN: Write to both sides ---" -ForegroundColor Cyan
"put key_p 100" | & $cliExe -addr "127.0.0.1:4000" | Out-Null
Write-Host "Wrote key_p=100 to P (4000)"
"put key_r 200" | & $cliExe -addr "127.0.0.1:4001" | Out-Null
Write-Host "Wrote key_r=200 to R1 (4001)"

# 5. Verify divergence
Write-Host ""
Write-Host "--- 5. Verify Divergence ---" -ForegroundColor Cyan
$pVal = "get key_r" | & $cliExe -addr "127.0.0.1:4000" 2>&1
$r1Val = "get key_p" | & $cliExe -addr "127.0.0.1:4001" 2>&1
if ($pVal -match "not found" -and $r1Val -match "not found") {
    Write-Host "CONFIRMED: P and R1 have diverged." -ForegroundColor Green
    Write-Host "  P does not have 'key_r'" 
    Write-Host "  R1 does not have 'key_p'"
} else {
    Write-Host "UNEXPECTED: Divergence not observed." -ForegroundColor Yellow
}

# 6. Heal the partition: Re-point R1 back to P
Write-Host ""
Write-Host "--- 6. HEAL PARTITION: Re-point R1 to P ---" -ForegroundColor Cyan
"replicaof 127.0.0.1:4000" | & $cliExe -addr "127.0.0.1:4001" | Out-Null
Write-Host "R1 is now following P again."
Start-Sleep -Seconds 1

# 7. Check Data Loss
Write-Host ""
Write-Host "--- 7. Verify Data Loss ---" -ForegroundColor Cyan
$r1KeyP = "get key_p" | & $cliExe -addr "127.0.0.1:4001" 2>&1
$r1KeyR = "get key_r" | & $cliExe -addr "127.0.0.1:4001" 2>&1

if ($r1KeyP -match "100") {
    Write-Host "R1 now sees key_p=100 (from P)." -ForegroundColor Green
} else {
    Write-Host "ERROR: R1 did not sync key_p from P." -ForegroundColor Red
}

if ($r1KeyR -match "not found") {
    Write-Host "R1's own write (key_r=200) is LOST." -ForegroundColor Red
    Write-Host "This is the Split Brain data loss." -ForegroundColor Yellow
} else {
    Write-Host "UNEXPECTED: R1 still has key_r." -ForegroundColor Yellow
}

# 8. Summary
Write-Host ""
Write-Host "=== SUMMARY ===" -ForegroundColor Cyan
Write-Host "Split Brain creates two independent histories."
Write-Host "When the partition heals, the rejoining node discards its history."
Write-Host "Data written to R1 during the split is permanently lost."
Write-Host ""
Write-Host "To prevent this, we would need:"
Write-Host "  1. Epoch/Term numbers to detect stale leaders"
Write-Host "  2. Fencing tokens to reject writes from old leaders"
Write-Host "  3. Quorum-based writes (Raft/Paxos)"

# Cleanup
Stop-Process -Id $p.Id -ErrorAction SilentlyContinue
Stop-Process -Id $r1.Id -ErrorAction SilentlyContinue
