# quorum-election-test.ps1 - Quorum is required for election
# Scenario: 3-node cluster → kill all three → restart R1 alone → R1 can't elect (1/3) →
#           restart R2 → R1+R2 (2/3) form majority → elect leader
# Validates: ep031 phase 4 (majority requirement), phase 7 (peer topology persistence)
# Edge cases: single node retries elections without success, quorum restored mid-retry
$ErrorActionPreference = "Stop"
. "$PSScriptRoot\election-helpers.ps1"

$passed = 0; $failed = 0
$dataDir = Initialize-DataDir "quorum-election"
New-Item -ItemType Directory -Path "$dataDir\p", "$dataDir\r1", "$dataDir\r2" -Force | Out-Null
Cleanup-All
Build-Binaries

$cliExe = Get-BinaryPath "kv-cli"

# ---------------------------------------------------------------------------
# Phase 1: Start cluster and establish baseline
# ---------------------------------------------------------------------------
Write-Host "`n--- Starting 3-node cluster (P=4020, R1=4021, R2=4022) ---" -ForegroundColor Cyan
$p  = Start-Node -Port 4020 -DataDir "$dataDir\p"
Start-Sleep -Milliseconds 300
$r1 = Start-Node -Port 4021 -DataDir "$dataDir\r1" -ReplicaOf "127.0.0.1:4020"
$r2 = Start-Node -Port 4022 -DataDir "$dataDir\r2" -ReplicaOf "127.0.0.1:4020"
Start-Sleep -Seconds 2

"put baseline ok" | & $cliExe -addr "127.0.0.1:4020" | Out-Null
Start-Sleep -Milliseconds 500

Test-Case "1. Baseline replication works" {
    $ok1 = Wait-Replication "4021" "baseline" "ok"
    $ok2 = Wait-Replication "4022" "baseline" "ok"
    if (-not $ok1) { throw "R1 missing baseline" }
    if (-not $ok2) { throw "R2 missing baseline" }
}

# ---------------------------------------------------------------------------
# Phase 2: Kill ALL three nodes
# ---------------------------------------------------------------------------
Write-Host "`n--- Killing all nodes ---" -ForegroundColor Red
Stop-Process -Id $p.Id  -Force
Stop-Process -Id $r1.Id -Force
Stop-Process -Id $r2.Id -Force
Start-Sleep -Milliseconds 500

# ---------------------------------------------------------------------------
# Phase 3: Restart ONLY R1 — R1 alone (1 of 3 = no quorum)
# R1 starts with --replica-of (same dead P), enters election cycle normally.
# ---------------------------------------------------------------------------
Write-Host "`n--- Restarting R1 alone — R1 is the only node (1/3) ---" -ForegroundColor Yellow
$r1 = Start-Node -Port 4021 -DataDir "$dataDir\r1" -ReplicaOf "127.0.0.1:4020"

Test-Case "2. R1 alone cannot become primary (no quorum)" {
    # Wait longer than max election timeout (4s) + one retry cycle
    Start-Sleep -Seconds 6
    $out = ("put solo_write nope" | & $cliExe -addr "127.0.0.1:4021" 2>&1) -join " "
    if ($out -match "OK" -and $out -notmatch "redirect") {
        throw "R1 became primary with 1/3 nodes — quorum violation!"
    }
    Write-Host "  R1 correctly cannot accept writes while alone" -ForegroundColor Yellow
}

# ---------------------------------------------------------------------------
# Phase 4: Restart R2 — now R1+R2 (2 of 3) can form majority
# R2 starts with --replica-of (same dead P), enters election cycle normally.
# Both nodes have persisted peer topology from phase 1, so they can find each other.
# ---------------------------------------------------------------------------
Write-Host "`n--- Restarting R2 — majority (R1+R2) can now elect ---" -ForegroundColor Green
$r2 = Start-Node -Port 4022 -DataDir "$dataDir\r2" -ReplicaOf "127.0.0.1:4020"

Test-Case "3. R1+R2 elect a leader after R2 returns" {
    $leader = Wait-Election @("4021","4022")
    if (-not $leader) { throw "No election among R1+R2" }
    Write-Host "  New leader: port $leader" -ForegroundColor Yellow
}

# Allow time for loser to connect to winner via broadcastPromotion/relocate
Start-Sleep -Seconds 3

Test-Case "4. Writes replicate within new cluster" {
    $leader = Find-Primary @("4021","4022")
    $follower = if ($leader -eq "4021") { "4022" } else { "4021" }
    "put post_quorum alive" | & $cliExe -addr "127.0.0.1:$leader" | Out-Null
    $ok = Wait-Replication $follower "post_quorum" "alive" 15
    if (-not $ok) { throw "Follower missing post-quorum write" }
}

# ---------------------------------------------------------------------------
# Cleanup
# ---------------------------------------------------------------------------
Cleanup-All
Print-Results
