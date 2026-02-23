# prevote-test.ps1 - Verify partitioned follower doesn't disrupt leader on rejoin
#
# Scenario: 3-node cluster → partition one follower → let it campaign N times
#           (term inflation) → heal partition → verify leader remains stable.
#
# Without PreVote: partitioned follower increments term on every election timeout.
#   When it reconnects, its inflated term forces the healthy leader to step down.
#
# With PreVote: partitioned follower's PreVoteRequests fail (no majority), so it
#   never increments its real term. Reconnection is harmless.
#
# Partition simulation: kill R2, restart with --replica-of pointing to an
#   unreachable address (port 4059). R2 boots as follower, can't reach leader,
#   election timeouts fire, and it campaigns alone. Peers are wiped so discovery
#   can't short-circuit the partition.
#
# Validates: ep034 (PreVote — Ongaro dissertation §4.2.3)
$ErrorActionPreference = "Stop"
. "$PSScriptRoot\election-helpers.ps1"

$passed = 0; $failed = 0
$dataDir = Initialize-DataDir "prevote"
New-Item -ItemType Directory -Path "$dataDir\p", "$dataDir\r1", "$dataDir\r2" -Force | Out-Null
Cleanup-All
Build-Binaries

$cliExe = Get-BinaryPath "kv-cli"

# ---------------------------------------------------------------------------
# Phase 1: Start 3-node cluster, establish baseline
# ---------------------------------------------------------------------------
Write-Host "`n--- Starting 3-node cluster (P=4050, R1=4051, R2=4052) ---" -ForegroundColor Cyan
$p  = Start-Node -Port 4050 -DataDir "$dataDir\p"  -LogFile "$dataDir\p.log"
Start-Sleep -Milliseconds 300
$r1 = Start-Node -Port 4051 -DataDir "$dataDir\r1" -ReplicaOf "127.0.0.1:4050" -LogFile "$dataDir\r1.log"
$r2 = Start-Node -Port 4052 -DataDir "$dataDir\r2" -ReplicaOf "127.0.0.1:4050" -LogFile "$dataDir\r2.log"
Start-Sleep -Seconds 2

"put baseline yes" | & $cliExe -addr "127.0.0.1:4050" | Out-Null
Start-Sleep -Milliseconds 500

Test-Case "1. Baseline replication works" {
    $ok1 = Wait-Replication "4051" "baseline" "yes"
    $ok2 = Wait-Replication "4052" "baseline" "yes"
    if (-not $ok1) { throw "R1 missing baseline" }
    if (-not $ok2) { throw "R2 missing baseline" }
}

# ---------------------------------------------------------------------------
# Phase 2: Partition R2
#   Kill R2 and restart as follower of an unreachable address (port 4059).
#   Wipe persisted peers so discovery can't find the real leader.
#   R2 boots as follower, can't reach its "leader", election timeout fires,
#   and it campaigns alone — inflating its term with each attempt.
# ---------------------------------------------------------------------------
Write-Host "`n--- Partitioning R2 ---" -ForegroundColor Red
Stop-Process -Id $r2.Id -Force
Start-Sleep -Milliseconds 500

# Wipe persisted peers so discovery can't short-circuit the partition.
$metaFile = "$dataDir\r2\replication.meta"
if (Test-Path $metaFile) {
    $lines = [System.IO.File]::ReadAllText($metaFile) -split "`n"
    $cleaned = @()
    foreach ($line in $lines) {
        $trimmed = $line.TrimEnd("`r")
        if ($trimmed -match "^peers:") {
            $cleaned += "peers:"
        } else {
            $cleaned += $trimmed
        }
    }
    [System.IO.File]::WriteAllText($metaFile, ($cleaned -join "`n") + "`n")
    Write-Host "  Cleared R2's persisted peers" -ForegroundColor Yellow
}

# Restart R2 as follower of unreachable address — it will campaign in isolation.
# Use port 4053 so existing nodes can't accidentally contact it.
$r2 = Start-Node -Port 4053 -DataDir "$dataDir\r2" -ReplicaOf "127.0.0.1:4059" -LogFile "$dataDir\r2-partitioned.log"

# Let it campaign a few times (election timeout ~1-2s, so 10s gives ~4-6 attempts)
Write-Host "  Letting R2 campaign in isolation for 10 seconds..." -ForegroundColor Yellow
Start-Sleep -Seconds 10

# ---------------------------------------------------------------------------
# Phase 3: Meanwhile, healthy cluster (P + R1) continues working
# ---------------------------------------------------------------------------
Write-Host "`n--- Healthy cluster continues ---" -ForegroundColor Cyan

Test-Case "2. Primary still accepts writes during partition" {
    $out = ("put during_partition alive" | & $cliExe -addr "127.0.0.1:4050" 2>&1) -join " "
    if ($out -notmatch "OK") { throw "Primary rejected write: $out" }
}

Test-Case "3. R1 still replicates during partition" {
    $ok = Wait-Replication "4051" "during_partition" "alive"
    if (-not $ok) { throw "R1 missing during_partition write" }
}

# ---------------------------------------------------------------------------
# Phase 4: Heal partition — kill isolated R2, restart on original port as
#   replica of P. R2's term is now inflated from repeated campaigning.
# ---------------------------------------------------------------------------
Write-Host "`n--- Healing partition: reconnecting R2 to primary ---" -ForegroundColor Cyan
Stop-Process -Id $r2.Id -Force
Start-Sleep -Milliseconds 500

$r2 = Start-Node -Port 4052 -DataDir "$dataDir\r2" -ReplicaOf "127.0.0.1:4050" -LogFile "$dataDir\r2-healed.log"
Start-Sleep -Seconds 3

# ---------------------------------------------------------------------------
# Phase 5: Verify leader stability — the core PreVote assertion
# ---------------------------------------------------------------------------
Test-Case "4. Primary survived R2's reconnection (no step-down)" {
    $out = ("put after_heal stable" | & $cliExe -addr "127.0.0.1:4050" 2>&1) -join " "
    if ($out -notmatch "OK" -or $out -match "redirect") {
        throw "Primary stepped down or rejected write: $out"
    }
}

Test-Case "5. Primary is still the SAME primary (port 4050)" {
    $leader = Find-Primary @("4050","4051","4052")
    if ($leader -ne "4050") { throw "Leader changed to port $leader — disruption occurred!" }
}

Test-Case "6. No unnecessary election occurred" {
    $pLog = if (Test-Path "$dataDir\p.log") { Get-Content "$dataDir\p.log" } else { @() }
    $healIndex = ($pLog | Select-String "after_heal" | Select-Object -First 1).LineNumber
    if ($healIndex) {
        $postHealLog = $pLog | Select-Object -Skip $healIndex
        $stepDown = $postHealLog | Select-String "became follower|became candidate"
        if ($stepDown) {
            throw "Primary had role changes after heal: $($stepDown -join '; ')"
        }
    }
}

# ---------------------------------------------------------------------------
# Phase 6: Verify R2 resyncs and cluster is fully healthy
# ---------------------------------------------------------------------------
Test-Case "7. R2 receives post-partition data after rejoin" {
    $ok = Wait-Replication "4052" "during_partition" "alive" 15
    if (-not $ok) { throw "R2 did not sync during_partition write" }
}

Test-Case "8. R2 receives post-heal writes" {
    $ok = Wait-Replication "4052" "after_heal" "stable" 10
    if (-not $ok) { throw "R2 did not sync after_heal write" }
}

Test-Case "9. All three nodes operational — new write replicates everywhere" {
    "put final_check three_healthy" | & $cliExe -addr "127.0.0.1:4050" | Out-Null
    $ok1 = Wait-Replication "4051" "final_check" "three_healthy" 10
    $ok2 = Wait-Replication "4052" "final_check" "three_healthy" 10
    if (-not $ok1) { throw "R1 missing final write" }
    if (-not $ok2) { throw "R2 missing final write" }
}

# ---------------------------------------------------------------------------
# Diagnostic: dump term inflation evidence from R2's partitioned log
# ---------------------------------------------------------------------------
Write-Host "`n--- Diagnostic: R2 term inflation during partition ---" -ForegroundColor Yellow
if (Test-Path "$dataDir\r2-partitioned.log") {
    $campaigns = Get-Content "$dataDir\r2-partitioned.log" | Select-String "became candidate|election started"
    if ($campaigns) {
        Write-Host "  R2 campaigned $($campaigns.Count) times while partitioned:" -ForegroundColor Yellow
        $campaigns | Select-Object -First 5 | ForEach-Object { Write-Host "    $_" -ForegroundColor DarkYellow }
        if ($campaigns.Count -gt 5) { Write-Host "    ... ($($campaigns.Count - 5) more)" -ForegroundColor DarkYellow }
    } else {
        Write-Host "  R2 did not campaign (PreVote blocked it or fenced)" -ForegroundColor Green
    }
}

# ---------------------------------------------------------------------------
# Cleanup
# ---------------------------------------------------------------------------
Cleanup-All
Print-Results
