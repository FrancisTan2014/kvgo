# election-helpers.ps1 - Shared helpers for ep031 election integration tests
# Dot-source from test scripts:  . "$PSScriptRoot\election-helpers.ps1"

$script:rootDir = Join-Path $PSScriptRoot ".."
$script:srcDir  = Join-Path $script:rootDir "src"

function Build-Binaries {
    param([string[]]$Targets = @("kv-server", "kv-cli"))
    Write-Host "--- Building ---" -ForegroundColor Cyan
    Push-Location $script:srcDir
    foreach ($t in $Targets) {
        go build -o "$t.exe" "./cmd/$t"
        if ($LASTEXITCODE -ne 0) { throw "Failed to build $t" }
    }
    Pop-Location
}

function Get-BinaryPath($name) {
    return Join-Path $script:srcDir "$name.exe"
}

function Cleanup-All {
    Get-Process kv-server, kv-cli, kv-bench -ErrorAction SilentlyContinue | Stop-Process -Force
    Start-Sleep -Milliseconds 500
}

function Initialize-DataDir($testName) {
    $dir = Join-Path $script:rootDir ".test\$testName"
    if (Test-Path $dir) { Remove-Item -Recurse -Force $dir }
    return $dir
}

function Test-Case($name, $block) {
    Write-Host "`n--- $name ---" -ForegroundColor Cyan
    try {
        & $block
        Write-Host "PASS: $name" -ForegroundColor Green
        $script:passed++
    } catch {
        Write-Host "FAIL: $name - $_" -ForegroundColor Red
        $script:failed++
    }
}

function Find-Primary($ports) {
    $cli = Get-BinaryPath "kv-cli"
    foreach ($port in $ports) {
        $out = ("put __probe__ 1" | & $cli -addr "127.0.0.1:$port" 2>&1) -join " "
        if ($out -match "OK" -and $out -notmatch "redirect") { return $port }
    }
    return $null
}

function Wait-Election($ports, $timeoutSec = 15) {
    $deadline = (Get-Date).AddSeconds($timeoutSec)
    while ((Get-Date) -lt $deadline) {
        $primary = Find-Primary $ports
        if ($primary) { return $primary }
        Start-Sleep -Milliseconds 500
    }
    return $null
}

function Wait-Replication($port, $key, $expectedVal, $timeoutSec = 10) {
    $cli = Get-BinaryPath "kv-cli"
    $deadline = (Get-Date).AddSeconds($timeoutSec)
    while ((Get-Date) -lt $deadline) {
        $v = ("get $key" | & $cli -addr "127.0.0.1:$port" 2>&1) -join " "
        if ($v -match $expectedVal) { return $true }
        Start-Sleep -Milliseconds 500
    }
    return $false
}

function Start-Node {
    param(
        [string]$Port,
        [string]$DataDir,
        [string]$ReplicaOf = $null
    )
    $serverExe = Get-BinaryPath "kv-server"
    $nodeArgs = @("--port", $Port, "--data-dir", $DataDir)
    if ($ReplicaOf) {
        $nodeArgs += @("--replica-of", $ReplicaOf)
    }
    return Start-Process -FilePath $serverExe -ArgumentList $nodeArgs -PassThru -NoNewWindow
}

function Print-Results {
    Write-Host "`n=== Results: $script:passed passed, $script:failed failed ===" -ForegroundColor $(if ($script:failed -eq 0) { "Green" } else { "Red" })
    if ($script:failed -gt 0) { exit 1 }
}
