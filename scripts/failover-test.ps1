# failover-test.ps1 - Verify 3-node failover (P -> R1, R2)
# DEPRECATED: Uses manual `promote` command which is now disabled.
# Replaced by: election-test.ps1 (automatic leader election after primary crash)
#              primary-rejoin-test.ps1 (crashed primary restarts and rejoins)
$ErrorActionPreference = "Stop"
$scriptDir = Split-Path -Parent $MyInvocation.MyCommand.Path
$srcDir = Join-Path $scriptDir "..\src"
$dataDir = Join-Path $scriptDir "..\.test\failover"

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

# 1. Start Cluster
Write-Host "--- 1. Starting 3-Node Cluster ---" -ForegroundColor Cyan
$p = Start-Process -FilePath $serverExe -ArgumentList "--port", "4000", "--data-dir", "$dataDir\p" -PassThru -NoNewWindow
Start-Sleep -Milliseconds 200
$r1 = Start-Process -FilePath $serverExe -ArgumentList "--port", "4001", "--data-dir", "$dataDir\r1", "--replica-of", "127.0.0.1:4000" -PassThru -NoNewWindow
$r2 = Start-Process -FilePath $serverExe -ArgumentList "--port", "4002", "--data-dir", "$dataDir\r2", "--replica-of", "127.0.0.1:4000" -PassThru -NoNewWindow

Write-Host "P: $($p.Id), R1: $($r1.Id), R2: $($r2.Id)" -ForegroundColor Yellow
Start-Sleep -Seconds 1

# 2. Kill Primary
Write-Host "--- 2. Killing Primary (P) ---" -ForegroundColor Red
Stop-Process -Id $p.Id -Force
Start-Sleep -Seconds 1

# 3. Promote R1
Write-Host "--- 3. Promoting R1 (4001) ---" -ForegroundColor Cyan
"promote" | & $cliExe -addr "127.0.0.1:4001" | Out-Null
Write-Host "R1 Promoted. Writing key 'new_king' to R1..."
"put new_king 1" | & $cliExe -addr "127.0.0.1:4001" | Out-Null

# 4. Check R2 Status (The Sabotage)
Write-Host "--- 4. Checking R2 (4002) Status ---" -ForegroundColor Cyan
Write-Host "Can R2 see 'new_king'? (Expect: No, it's orphaned)"
$val = "get new_king" | & $cliExe -addr "127.0.0.1:4002" 2>&1
if ($val -match "not found") {
    Write-Host "CONFIRMED: R2 did not receive update." -ForegroundColor Green
} else {
    Write-Host "SURPRISE: R2 got it? ($val)" -ForegroundColor Yellow
}

# 5. Attempt Repair (The Goal)
Write-Host "--- 5. Attempting to Re-point R2 to R1 ---" -ForegroundColor Cyan
Write-Host "Sending 'replicaof 127.0.0.1:4001' to R2..."
$out = "replicaof 127.0.0.1:4001" | & $cliExe -addr "127.0.0.1:4002" 2>&1

if ($out -match "unknown command") {
    Write-Host "FAILURE: Command not implemented yet." -ForegroundColor Red
} elseif ($out -match "OK") {
    Start-Sleep -Seconds 1
    $val = "get new_king" | & $cliExe -addr "127.0.0.1:4002" 2>&1
    if ($val -match "1") {
        Write-Host "SUCCESS: R2 is following R1!" -ForegroundColor Green
    } else {
        Write-Host "FAILURE: R2 accepted command but didn't sync." -ForegroundColor Red
    }
} else {
    Write-Host "OUTPUT: $out"
}

# Cleanup
Stop-Process -Id $r1.Id -ErrorAction SilentlyContinue
Stop-Process -Id $r2.Id -ErrorAction SilentlyContinue
