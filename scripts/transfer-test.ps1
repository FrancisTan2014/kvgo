# transfer-test.ps1 - Verify graceful leader transfer via CmdTransferLeader / CmdTimeoutNow
#
# Scenario: 3-node cluster → write baseline → transfer leadership from P to R1
#           → verify R1 becomes the new leader → verify writes work on new leader
#           → verify old leader stepped down and rejoins as follower
#
# Validates: ep035 (Leader Transfer — graceful handoff via TimeoutNow)
$ErrorActionPreference = "Stop"
. "$PSScriptRoot\election-helpers.ps1"

$passed = 0; $failed = 0
$dataDir = Initialize-DataDir "transfer"
New-Item -ItemType Directory -Path "$dataDir\p", "$dataDir\r1", "$dataDir\r2" -Force | Out-Null
Cleanup-All
Build-Binaries

$cliExe = Get-BinaryPath "kv-cli"

# ---------------------------------------------------------------------------
# Helper: read nodeID from a node's replication.meta file
# ---------------------------------------------------------------------------
function Get-NodeID($dir) {
    $metaFile = Join-Path $dir "replication.meta"
    $deadline = (Get-Date).AddSeconds(10)
    while ((Get-Date) -lt $deadline) {
        if (Test-Path $metaFile) {
            $lines = Get-Content $metaFile
            foreach ($line in $lines) {
                if ($line -match "^nodeID:(.+)$") {
                    return $Matches[1]
                }
            }
        }
        Start-Sleep -Milliseconds 500
    }
    return $null
}

# ---------------------------------------------------------------------------
# Phase 1: Start 3-node cluster, establish baseline
# ---------------------------------------------------------------------------
Write-Host "`n--- Starting 3-node cluster (P=4060, R1=4061, R2=4062) ---" -ForegroundColor Cyan
$p  = Start-Node -Port 4060 -DataDir "$dataDir\p"  -LogFile "$dataDir\p.log"
Start-Sleep -Milliseconds 500
$r1 = Start-Node -Port 4061 -DataDir "$dataDir\r1" -ReplicaOf "127.0.0.1:4060" -LogFile "$dataDir\r1.log"
$r2 = Start-Node -Port 4062 -DataDir "$dataDir\r2" -ReplicaOf "127.0.0.1:4060" -LogFile "$dataDir\r2.log"
Start-Sleep -Seconds 3

# Write baseline data
for ($i = 1; $i -le 5; $i++) {
    "put baseline_$i value_$i" | & $cliExe -addr "127.0.0.1:4060" | Out-Null
}
Start-Sleep -Seconds 1

Test-Case "1. Baseline replication works" {
    $ok1 = Wait-Replication "4061" "baseline_5" "value_5"
    $ok2 = Wait-Replication "4062" "baseline_5" "value_5"
    if (-not $ok1) { throw "R1 missing baseline" }
    if (-not $ok2) { throw "R2 missing baseline" }
}

# ---------------------------------------------------------------------------
# Phase 2: Read node IDs and identify transfer target
# ---------------------------------------------------------------------------
$r1NodeID = Get-NodeID "$dataDir\r1"
Write-Host "  R1 nodeID: $r1NodeID" -ForegroundColor Yellow

Test-Case "2. R1 nodeID is available" {
    if (-not $r1NodeID) { throw "Could not read R1 nodeID from meta file" }
}

# ---------------------------------------------------------------------------
# Phase 3: Verify primary rejects writes from wrong target
# ---------------------------------------------------------------------------
Test-Case "3. Transfer rejects unknown target" {
    $out = ("transfer nonexistent-node" | & $cliExe -addr "127.0.0.1:4060" 2>&1) -join " "
    if ($out -notmatch "error|server error") { throw "Expected error for unknown target: $out" }
}

# ---------------------------------------------------------------------------
# Phase 4: Transfer leadership from P to R1
# ---------------------------------------------------------------------------
Write-Host "`n--- Transferring leadership P → R1 ---" -ForegroundColor Cyan

Test-Case "4. Transfer command succeeds" {
    $out = ("transfer $r1NodeID" | & $cliExe -addr "127.0.0.1:4060" 2>&1) -join " "
    if ($out -notmatch "OK") { throw "Transfer command failed: $out" }
}

# Wait for election to complete
Start-Sleep -Seconds 4

# ---------------------------------------------------------------------------
# Phase 5: Verify R1 is the new leader
# ---------------------------------------------------------------------------
Test-Case "5. R1 is now the leader" {
    $leader = Find-Primary @("4060","4061","4062")
    if ($leader -ne "4061") { throw "Expected R1 (4061) as leader, got $leader" }
}

Test-Case "6. New leader (R1) accepts writes" {
    $out = ("put after_transfer new_leader_write" | & $cliExe -addr "127.0.0.1:4061" 2>&1) -join " "
    if ($out -notmatch "OK" -or $out -match "redirect") {
        throw "New leader rejected write: $out"
    }
}

# ---------------------------------------------------------------------------
# Phase 6: Verify old leader stepped down
# ---------------------------------------------------------------------------
Test-Case "7. Old leader (P) stepped down" {
    # PUT to old leader should redirect or fail (it's now a follower)
    $out = ("put to_old_leader test" | & $cliExe -addr "127.0.0.1:4060" 2>&1) -join " "
    if ($out -match "^OK$" -or $out -match "OK$" -and $out -notmatch "redirect") {
        throw "Old leader still accepts writes: $out"
    }
}

# ---------------------------------------------------------------------------
# Phase 7: Verify cluster is fully healthy — new writes replicate
# ---------------------------------------------------------------------------
# After transfer, R2 must discover the new leader (R1) and reconnect.
# This involves the reconcile loop (3s interval) + full resync, so give
# extra settle time before checking replication.
Start-Sleep -Seconds 5

Test-Case "8. Post-transfer write replicates to all nodes" {
    $ok1 = Wait-Replication "4060" "after_transfer" "new_leader_write" 15
    $ok2 = Wait-Replication "4062" "after_transfer" "new_leader_write" 15
    if (-not $ok1) { throw "Old leader (P) missing post-transfer write" }
    if (-not $ok2) { throw "R2 missing post-transfer write" }
}

Test-Case "9. All baseline data still accessible on new leader" {
    for ($i = 1; $i -le 5; $i++) {
        $v = ("get baseline_$i" | & $cliExe -addr "127.0.0.1:4061" 2>&1) -join " "
        if ($v -notmatch "value_$i") { throw "Baseline key baseline_$i not found on new leader: $v" }
    }
}

# ---------------------------------------------------------------------------
# Phase 8: Write fence diagnostic — check TRANSFER log messages
# ---------------------------------------------------------------------------
Write-Host "`n--- Diagnostic: Transfer log messages ---" -ForegroundColor Yellow
if (Test-Path "$dataDir\p.log") {
    $transferLines = Get-Content "$dataDir\p.log" | Select-String "TRANSFER"
    if ($transferLines) {
        Write-Host "  Primary transfer log:" -ForegroundColor Yellow
        $transferLines | Select-Object -First 5 | ForEach-Object { Write-Host "    $_" -ForegroundColor DarkYellow }
    }
}
if (Test-Path "$dataDir\r1.log") {
    $timeoutLines = Get-Content "$dataDir\r1.log" | Select-String "TIMEOUT_NOW"
    if ($timeoutLines) {
        Write-Host "  R1 TimeoutNow log:" -ForegroundColor Yellow
        $timeoutLines | Select-Object -First 5 | ForEach-Object { Write-Host "    $_" -ForegroundColor DarkYellow }
    }
}

# ---------------------------------------------------------------------------
# Cleanup
# ---------------------------------------------------------------------------
Cleanup-All
Print-Results
