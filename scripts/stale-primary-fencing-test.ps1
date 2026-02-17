# stale-primary-fencing-test.ps1 - Stale primary is fenced by higher-term replicas
#
# *** ASPIRATIONAL — depends on proactive term propagation (future episode) ***
#
# Scenario: P is alive but partitioned (simulated by killing replicas' connections).
#           R1/R2 elect a new leader at a higher term. When partition heals, P should
#           discover the higher term and step down automatically.
#
# Current gap: After R1 becomes leader, broadcastPromotion tries to reach P but P
# was dead/unreachable at that moment. broadcastPromotion is a one-shot — no retry.
# When P comes back (or partition heals), nobody proactively contacts P with the
# higher term. P sits as a stale leader until someone connects to it.
#
# What would fix this:
# - Periodic peer heartbeats (term gossip via peerManager)
# - Leader re-broadcasting promotion on timer
# - Followers periodically advertising their leader to all known peers
#
# Validates: ep031 phase 5 (stale primary fencing) + proactive discovery
# Expected: FAIL until proactive term propagation is implemented
$ErrorActionPreference = "Stop"
. "$PSScriptRoot\election-helpers.ps1"

$passed = 0; $failed = 0
$dataDir = Initialize-DataDir "stale-fencing"
New-Item -ItemType Directory -Path "$dataDir\p", "$dataDir\r1", "$dataDir\r2" -Force | Out-Null
Cleanup-All
Build-Binaries

$cliExe = Get-BinaryPath "kv-cli"

# ---------------------------------------------------------------------------
# Phase 1: Start cluster and establish baseline
# ---------------------------------------------------------------------------
Write-Host "`n--- Starting 3-node cluster (P=4040, R1=4041, R2=4042) ---" -ForegroundColor Cyan
$p  = Start-Node -Port 4040 -DataDir "$dataDir\p"
Start-Sleep -Milliseconds 300
$r1 = Start-Node -Port 4041 -DataDir "$dataDir\r1" -ReplicaOf "127.0.0.1:4040"
$r2 = Start-Node -Port 4042 -DataDir "$dataDir\r2" -ReplicaOf "127.0.0.1:4040"
Start-Sleep -Seconds 2

"put before_partition ok" | & $cliExe -addr "127.0.0.1:4040" | Out-Null
Start-Sleep -Milliseconds 500

Test-Case "1. Baseline replication works" {
    $ok1 = Wait-Replication "4041" "before_partition" "ok"
    $ok2 = Wait-Replication "4042" "before_partition" "ok"
    if (-not $ok1) { throw "R1 missing baseline" }
    if (-not $ok2) { throw "R2 missing baseline" }
}

# ---------------------------------------------------------------------------
# Phase 2: Simulate partition — kill R1/R2, restart them standalone.
# They use persisted peer topology to find each other and elect.
# P remains alive on port 4040, still thinking it's the leader.
# ---------------------------------------------------------------------------
Write-Host "`n--- Simulating partition: kill R1/R2, restart standalone ---" -ForegroundColor Red
Stop-Process -Id $r1.Id -Force
Stop-Process -Id $r2.Id -Force
Start-Sleep -Milliseconds 500

$r1 = Start-Node -Port 4041 -DataDir "$dataDir\r1"
$r2 = Start-Node -Port 4042 -DataDir "$dataDir\r2"

Test-Case "2. R1/R2 elect a new leader (higher term)" {
    $leader = Wait-Election @("4041","4042")
    if (-not $leader) { throw "No election among R1/R2" }
    Write-Host "  New leader: port $leader" -ForegroundColor Yellow
}

$newLeader = Find-Primary @("4041","4042")
"put after_partition new_era" | & $cliExe -addr "127.0.0.1:$newLeader" | Out-Null
Start-Sleep -Milliseconds 500

# ---------------------------------------------------------------------------
# Phase 3: Verify stale primary is fenced automatically
# This requires P to receive a higher-term message (PONG, VoteRequest, or
# broadcastPromotion). Currently no mechanism retries this contact.
# ---------------------------------------------------------------------------
Write-Host "`n--- [ASPIRATIONAL] Checking if stale primary steps down ---" -ForegroundColor Yellow

Test-Case "3. [ASPIRATIONAL] Old primary no longer accepts writes" {
    # Give time for any proactive discovery mechanism
    Start-Sleep -Seconds 8
    $out = ("put stale_write reject_me" | & $cliExe -addr "127.0.0.1:4040" 2>&1) -join " "
    if ($out -match "OK" -and $out -notmatch "redirect") {
        throw "Stale primary accepted writes — fencing failed!"
    }
    Write-Host "  Port 4040 correctly rejects writes" -ForegroundColor Yellow
}

Test-Case "4. [ASPIRATIONAL] Old primary syncs post-partition data" {
    $ok = Wait-Replication "4040" "after_partition" "new_era" 15
    if (-not $ok) { throw "Old primary did not sync post-partition data" }
}

Test-Case "5. [ASPIRATIONAL] Exactly one primary in cluster" {
    $primaries = 0
    foreach ($port in @("4040","4041","4042")) {
        $out = ("put count_check $port" | & $cliExe -addr "127.0.0.1:$port" 2>&1) -join " "
        if ($out -match "OK" -and $out -notmatch "redirect") { $primaries++ }
    }
    if ($primaries -ne 1) { throw "Expected 1 primary, got $primaries" }
}

# ---------------------------------------------------------------------------
# Cleanup
# ---------------------------------------------------------------------------
Cleanup-All
Print-Results
