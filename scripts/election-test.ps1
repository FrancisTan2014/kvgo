# election-test.ps1 - Core leader election after primary crash
# Scenario: 3-node cluster → kill primary → verify election + data + replication
# Validates: ep031 phases 1-4 (term counter, role state machine, failure detection, vote protocol)
# Edge cases: split brain prevention, data continuity across election, follower replication recovery
$ErrorActionPreference = "Stop"
. "$PSScriptRoot\election-helpers.ps1"

$passed = 0; $failed = 0
$dataDir = Initialize-DataDir "election"
New-Item -ItemType Directory -Path "$dataDir\p", "$dataDir\r1", "$dataDir\r2" -Force | Out-Null
Cleanup-All
Build-Binaries

$cliExe = Get-BinaryPath "kv-cli"

# ---------------------------------------------------------------------------
# Start 3-node cluster: P(4000), R1(4001), R2(4002)
# ---------------------------------------------------------------------------
Write-Host "`n--- Starting 3-node cluster ---" -ForegroundColor Cyan
$p  = Start-Node -Port 4000 -DataDir "$dataDir\p"
Start-Sleep -Milliseconds 300
$r1 = Start-Node -Port 4001 -DataDir "$dataDir\r1" -ReplicaOf "127.0.0.1:4000"
$r2 = Start-Node -Port 4002 -DataDir "$dataDir\r2" -ReplicaOf "127.0.0.1:4000"
Start-Sleep -Seconds 2

"put baseline yes" | & $cliExe -addr "127.0.0.1:4000" | Out-Null
Start-Sleep -Milliseconds 500

Test-Case "1. Baseline replication works" {
    $v1 = ("get baseline" | & $cliExe -addr "127.0.0.1:4001" 2>&1) -join " "
    $v2 = ("get baseline" | & $cliExe -addr "127.0.0.1:4002" 2>&1) -join " "
    if ($v1 -notmatch "yes") { throw "R1 missing baseline: $v1" }
    if ($v2 -notmatch "yes") { throw "R2 missing baseline: $v2" }
}

# ---------------------------------------------------------------------------
# Kill primary, wait for election
# ---------------------------------------------------------------------------
Write-Host "`n--- Killing primary (port 4000) ---" -ForegroundColor Red
Stop-Process -Id $p.Id -Force
Start-Sleep -Milliseconds 500

Test-Case "2. New primary elected after kill" {
    $leader = Wait-Election @("4001","4002")
    if (-not $leader) { throw "No node accepted writes within 15s" }
    Write-Host "  New primary on port $leader" -ForegroundColor Yellow
}

Test-Case "3. Only one primary exists (no split brain)" {
    $primaries = 0
    foreach ($port in @("4001","4002")) {
        $out = ("put split_check $port" | & $cliExe -addr "127.0.0.1:$port" 2>&1) -join " "
        if ($out -match "OK" -and $out -notmatch "redirect") { $primaries++ }
    }
    if ($primaries -ne 1) { throw "Expected 1 primary, got $primaries" }
}

Test-Case "4. Pre-crash data survives election" {
    $leader = Find-Primary @("4001","4002")
    $v = ("get baseline" | & $cliExe -addr "127.0.0.1:$leader" 2>&1) -join " "
    if ($v -notmatch "yes") { throw "Baseline lost after election: $v" }
}

Test-Case "5. Writes after election replicate to follower" {
    $leader = Find-Primary @("4001","4002")
    $follower = if ($leader -eq "4001") { "4002" } else { "4001" }
    "put after_election works" | & $cliExe -addr "127.0.0.1:$leader" | Out-Null
    $ok = Wait-Replication $follower "after_election" "works" 15
    if (-not $ok) { throw "Follower missing post-election write after 15s" }
}

# ---------------------------------------------------------------------------
# Cleanup
# ---------------------------------------------------------------------------
Cleanup-All
Print-Results
