# primary-rejoin-test.ps1 - Crashed primary restarts and rejoins as follower
# Scenario: 3-node cluster → kill primary → R1/R2 elect → restart old primary
#           with --replica-of new leader → old primary adopts higher term, syncs data
# Validates: ep031 phases 5-6 (term fencing via PING/PONG, step-down recovery)
# Edge cases: pre-crash data retained, post-election data synced, no split brain after rejoin
#
# Note: This test uses --replica-of to manually direct P to the new leader.
# Automatic discovery (P finds the new leader without manual intervention) is tested
# in stale-primary-fencing-test.ps1 (aspirational — requires proactive term propagation).
$ErrorActionPreference = "Stop"
. "$PSScriptRoot\election-helpers.ps1"

$passed = 0; $failed = 0
$dataDir = Initialize-DataDir "primary-rejoin"
New-Item -ItemType Directory -Path "$dataDir\p", "$dataDir\r1", "$dataDir\r2" -Force | Out-Null
Cleanup-All
Build-Binaries

$cliExe = Get-BinaryPath "kv-cli"

# ---------------------------------------------------------------------------
# Phase 1: Start cluster, write baseline, kill primary
# ---------------------------------------------------------------------------
Write-Host "`n--- Starting 3-node cluster (P=4030, R1=4031, R2=4032) ---" -ForegroundColor Cyan
$p  = Start-Node -Port 4030 -DataDir "$dataDir\p"
Start-Sleep -Milliseconds 300
$r1 = Start-Node -Port 4031 -DataDir "$dataDir\r1" -ReplicaOf "127.0.0.1:4030"
$r2 = Start-Node -Port 4032 -DataDir "$dataDir\r2" -ReplicaOf "127.0.0.1:4030"
Start-Sleep -Seconds 2

"put before_crash original" | & $cliExe -addr "127.0.0.1:4030" | Out-Null
Start-Sleep -Milliseconds 500

Test-Case "1. Baseline data replicated" {
    $ok = Wait-Replication "4031" "before_crash" "original"
    if (-not $ok) { throw "R1 missing baseline" }
    $ok = Wait-Replication "4032" "before_crash" "original"
    if (-not $ok) { throw "R2 missing baseline" }
}

# Kill primary — keep data dir intact for restart
Write-Host "`n--- Killing primary (port 4030) ---" -ForegroundColor Red
Stop-Process -Id $p.Id -Force
Start-Sleep -Milliseconds 500

# ---------------------------------------------------------------------------
# Phase 2: Election among R1/R2
# ---------------------------------------------------------------------------
Test-Case "2. New leader elected from R1/R2" {
    $leader = Wait-Election @("4031","4032")
    if (-not $leader) { throw "No election within 15s" }
    Write-Host "  New leader: port $leader" -ForegroundColor Yellow
}

# Write data while old primary is down
$newLeader = Find-Primary @("4031","4032")
"put during_outage new_data" | & $cliExe -addr "127.0.0.1:$newLeader" | Out-Null
Start-Sleep -Milliseconds 500

# ---------------------------------------------------------------------------
# Phase 3: Restart old primary with --replica-of new leader
# This causes P to connect to the new leader. The new leader sends PING with
# a higher term (term >= 2). P adopts the term and serves as a follower.
# ---------------------------------------------------------------------------
Write-Host "`n--- Restarting old primary (port 4030) with --replica-of $newLeader ---" -ForegroundColor Yellow
$p2 = Start-Node -Port 4030 -DataDir "$dataDir\p" -ReplicaOf "127.0.0.1:$newLeader"
Start-Sleep -Seconds 3

Test-Case "3. Restarted node does NOT accept writes (is follower)" {
    $out = ("put from_old_p reject_me" | & $cliExe -addr "127.0.0.1:4030" 2>&1) -join " "
    if ($out -match "OK" -and $out -notmatch "redirect") {
        throw "Old primary accepted writes directly — should be follower!"
    }
    Write-Host "  Port 4030 correctly rejects/redirects writes" -ForegroundColor Yellow
}

Test-Case "4. Old primary syncs data written during outage" {
    $ok = Wait-Replication "4030" "during_outage" "new_data" 15
    if (-not $ok) { throw "Old primary did not sync 'during_outage'" }
}

Test-Case "5. Old primary retains pre-crash data" {
    $v = ("get before_crash" | & $cliExe -addr "127.0.0.1:4030" 2>&1) -join " "
    if ($v -notmatch "original") { throw "Old primary lost pre-crash data: $v" }
}

Test-Case "6. Exactly one primary in cluster" {
    $primaries = 0
    foreach ($port in @("4030","4031","4032")) {
        $out = ("put final_check $port" | & $cliExe -addr "127.0.0.1:$port" 2>&1) -join " "
        if ($out -match "OK" -and $out -notmatch "redirect") { $primaries++ }
    }
    if ($primaries -ne 1) { throw "Expected 1 primary, got $primaries" }
}

# ---------------------------------------------------------------------------
# Cleanup
# ---------------------------------------------------------------------------
Cleanup-All
Print-Results
