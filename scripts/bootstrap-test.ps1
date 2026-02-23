# bootstrap-test.ps1 - Replica restart WITHOUT --replica-of preserves cluster
#
# Scenario: 3-node cluster → kill replica → restart WITHOUT --replica-of flag
#           → restarted node should rejoin as follower, NOT depose the leader
#
# The Sabotage (pre-033):
#   R1 restarts without --replica-of → starts as sovereign leader →
#   sends VOTE_REQUEST(higher term) → P steps down → cluster disrupted
#
# After 033:
#   R1 restarts, detects peers in replication.meta → starts as follower →
#   discovers P via CmdDiscovery → rejoins as follower → P remains leader
#
# Validates: ep033 (bootstrap - follower default + discovery)
# Pre-033:  FAILS (restarted replica deposes healthy leader)
# Post-033: PASSES (restarted replica rejoins without disruption)
$ErrorActionPreference = "Stop"
. "$PSScriptRoot\election-helpers.ps1"

$passed = 0; $failed = 0
$dataDir = Initialize-DataDir "bootstrap"
New-Item -ItemType Directory -Path "$dataDir\p", "$dataDir\r1", "$dataDir\r2" -Force | Out-Null
Cleanup-All
Build-Binaries

$cliExe = Get-BinaryPath "kv-cli"

# ---------------------------------------------------------------------------
# Phase 1: Start 3-node cluster, write baseline
# ---------------------------------------------------------------------------
Write-Host "`n--- Starting 3-node cluster (P=4050, R1=4051, R2=4052) ---" -ForegroundColor Cyan
$p  = Start-Node -Port 4050 -DataDir "$dataDir\p" -LogFile "$dataDir\p.log"
Start-Sleep -Milliseconds 300
$r1 = Start-Node -Port 4051 -DataDir "$dataDir\r1" -ReplicaOf "127.0.0.1:4050" -LogFile "$dataDir\r1.log"
$r2 = Start-Node -Port 4052 -DataDir "$dataDir\r2" -ReplicaOf "127.0.0.1:4050" -LogFile "$dataDir\r2.log"
Start-Sleep -Seconds 2

"put before_restart original" | & $cliExe -addr "127.0.0.1:4050" | Out-Null
Start-Sleep -Milliseconds 500

Test-Case "1. Baseline replication works" {
    $ok1 = Wait-Replication "4051" "before_restart" "original"
    $ok2 = Wait-Replication "4052" "before_restart" "original"
    if (-not $ok1) { throw "R1 missing baseline" }
    if (-not $ok2) { throw "R2 missing baseline" }
}

# ---------------------------------------------------------------------------
# Phase 2: Kill R1, write data while it's down
# ---------------------------------------------------------------------------
Write-Host "`n--- Killing R1 (port 4051) ---" -ForegroundColor Red
Stop-Process -Id $r1.Id -Force
Start-Sleep -Milliseconds 500

"put during_downtime new_data" | & $cliExe -addr "127.0.0.1:4050" | Out-Null
Start-Sleep -Milliseconds 500

Test-Case "2. P still accepts writes after R1 dies" {
    $out = ("put alive_check yes" | & $cliExe -addr "127.0.0.1:4050" 2>&1) -join " "
    if ($out -notmatch "OK") { throw "P rejected write: $out" }
}

# ---------------------------------------------------------------------------
# Phase 3: Restart R1 WITHOUT --replica-of (the sabotage scenario)
# R1's replication.meta has peers from the original cluster.
# Pre-033: R1 starts as leader, triggers election, deposes P.
# Post-033: R1 detects peers, starts as follower, discovers P, rejoins.
# ---------------------------------------------------------------------------
Write-Host "`n--- Restarting R1 (port 4051) WITHOUT --replica-of ---" -ForegroundColor Yellow
Write-Host "    (simulates process restart — no CLI args tell it who the leader is)" -ForegroundColor DarkGray
$r1 = Start-Node -Port 4051 -DataDir "$dataDir\r1" -LogFile "$dataDir\r1.log"
Start-Sleep -Seconds 5  # Give time for discovery + potential (bad) election

Test-Case "3. P is STILL the leader after R1 restart" {
    $out = ("put post_restart check" | & $cliExe -addr "127.0.0.1:4050" 2>&1) -join " "
    if ($out -notmatch "OK" -or $out -match "redirect") {
        throw "P lost leadership after R1 restart: $out"
    }
}

Test-Case "4. R1 does NOT accept writes (is follower, not sovereign leader)" {
    $out = ("put rogue_write reject_me" | & $cliExe -addr "127.0.0.1:4051" 2>&1) -join " "
    if ($out -match "OK" -and $out -notmatch "redirect") {
        throw "R1 accepted writes — it became a sovereign leader!"
    }
    Write-Host "  R1 correctly rejects/redirects writes" -ForegroundColor Yellow
}

Test-Case "5. R1 retains pre-restart data" {
    $v = ("get before_restart" | & $cliExe -addr "127.0.0.1:4051" 2>&1) -join " "
    if ($v -notmatch "original") { throw "R1 lost pre-restart data: $v" }
}

Test-Case "6. R1 syncs data written during its downtime" {
    $ok = Wait-Replication "4051" "during_downtime" "new_data" 10
    if (-not $ok) { throw "R1 did not sync data written while it was down" }
}

Test-Case "7. New writes after rejoin replicate to R1" {
    "put after_rejoin works" | & $cliExe -addr "127.0.0.1:4050" | Out-Null
    $ok = Wait-Replication "4051" "after_rejoin" "works" 10
    if (-not $ok) { throw "R1 not receiving new writes after rejoin" }
}

Test-Case "8. Exactly one primary in cluster" {
    $primaries = 0
    foreach ($port in @("4050","4051","4052")) {
        $out = ("put final_check $port" | & $cliExe -addr "127.0.0.1:$port" 2>&1) -join " "
        if ($out -match "OK" -and $out -notmatch "redirect") { $primaries++ }
    }
    if ($primaries -ne 1) { throw "Expected 1 primary, got $primaries" }
}

# ---------------------------------------------------------------------------
# Logs
# ---------------------------------------------------------------------------
Write-Host "`n--- Node Logs ---" -ForegroundColor Cyan
foreach ($name in @("p","r1","r2")) {
    $logPath = "$dataDir\$name.log"
    if (Test-Path $logPath) {
        Write-Host "`n=== $name ===" -ForegroundColor Yellow
        Get-Content $logPath
    }
}

# ---------------------------------------------------------------------------
# Cleanup
# ---------------------------------------------------------------------------
Cleanup-All
Print-Results
