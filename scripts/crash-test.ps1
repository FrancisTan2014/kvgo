# crash-test.ps1 - Kill primary mid-benchmark, verify automatic election + recovery
# Scenario: 3-node cluster under write load → kill primary → cluster self-heals
# Validates: election under stress, data durability through crash, automatic failover
# Edge cases: in-flight writes during crash, bench errors are transient not permanent
param(
    [int]$Requests = 50000,
    [int]$Concurrency = 50,
    [int]$KillAfterMs = 500
)

$ErrorActionPreference = "Stop"
. "$PSScriptRoot\election-helpers.ps1"

$passed = 0; $failed = 0
$dataDir = Initialize-DataDir "crash"
New-Item -ItemType Directory -Path "$dataDir\p", "$dataDir\r1", "$dataDir\r2" -Force | Out-Null
Cleanup-All
Build-Binaries @("kv-server", "kv-cli", "kv-bench")

$cliExe   = Get-BinaryPath "kv-cli"
$benchExe = Get-BinaryPath "kv-bench"

# ---------------------------------------------------------------------------
# Start 3-node cluster
# ---------------------------------------------------------------------------
Write-Host "`n--- Starting 3-node cluster (P=4010, R1=4011, R2=4012) ---" -ForegroundColor Cyan
$p  = Start-Node -Port 4010 -DataDir "$dataDir\p"
Start-Sleep -Milliseconds 300
$r1 = Start-Node -Port 4011 -DataDir "$dataDir\r1" -ReplicaOf "127.0.0.1:4010"
$r2 = Start-Node -Port 4012 -DataDir "$dataDir\r2" -ReplicaOf "127.0.0.1:4010"
Start-Sleep -Seconds 2

"put pre_crash baseline" | & $cliExe -addr "127.0.0.1:4010" | Out-Null
Start-Sleep -Milliseconds 500

# ---------------------------------------------------------------------------
# Start benchmark, then kill primary mid-run
# ---------------------------------------------------------------------------
Write-Host "`n--- Starting benchmark ($Requests reqs, $Concurrency concurrency, kill after ${KillAfterMs}ms) ---" -ForegroundColor Cyan
$benchJob = Start-Job -ScriptBlock {
    param($exe, $n, $c)
    & $exe -addr "127.0.0.1:4010" -n $n -c $c 2>&1
} -ArgumentList $benchExe, $Requests, $Concurrency

Start-Sleep -Milliseconds $KillAfterMs
Write-Host "--- Killing primary ---" -ForegroundColor Red
Stop-Process -Id $p.Id -Force

$benchOutput = Receive-Job -Job $benchJob -Wait
Remove-Job -Job $benchJob
Write-Host "`n--- Benchmark output (errors expected) ---" -ForegroundColor DarkGray
$benchOutput | Select-Object -First 8 | ForEach-Object { Write-Host $_ }

# ---------------------------------------------------------------------------
# Verify automatic election + recovery
# ---------------------------------------------------------------------------
Test-Case "1. Replicas elect a new leader automatically" {
    $leader = Wait-Election @("4011","4012")
    if (-not $leader) { throw "No election among R1/R2 after crash" }
    Write-Host "  New leader: port $leader" -ForegroundColor Yellow
}

$newLeader = Find-Primary @("4011","4012")

Test-Case "2. New leader accepts writes" {
    $out = ("put recovery_key works" | & $cliExe -addr "127.0.0.1:$newLeader" 2>&1) -join " "
    if ($out -notmatch "OK") { throw "New leader rejected write: $out" }
}

Test-Case "3. Pre-crash data survives on new leader" {
    $val = ("get pre_crash" | & $cliExe -addr "127.0.0.1:$newLeader" 2>&1) -join " "
    if ($val -notmatch "baseline") { throw "Pre-crash data lost: $val" }
}

Test-Case "4. Post-election write replicates to follower" {
    $follower = if ($newLeader -eq "4011") { "4012" } else { "4011" }
    $ok = Wait-Replication $follower "recovery_key" "works" 10
    if (-not $ok) { throw "Follower missing post-election write" }
}

# ---------------------------------------------------------------------------
# Cleanup
# ---------------------------------------------------------------------------
Cleanup-All
Print-Results
