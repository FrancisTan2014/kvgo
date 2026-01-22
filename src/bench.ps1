# bench.ps1 - Quick benchmark runner for kv-go
param(
    [int]$Count = 3,
    [string]$CPU = "1,8",
    [switch]$Compare,
    [switch]$Save,
    [switch]$Raw,
    [switch]$Full   # Use -Full for comprehensive benchmarks
)

$ErrorActionPreference = "Stop"

if ($Full) {
    $Count = 6
    $CPU = "1,4,8,16,32,64"
}

$benchCmd = "go test -bench=BenchmarkDB -benchmem `"-cpu=$CPU`" -count=$Count"

function Format-BenchResults {
    param([string]$Output)
    
    $lines = $Output -split "`n" | Where-Object { $_ -match "^BenchmarkDB" }
    
    # Parse results into hashtable by implementation and CPU
    $results = @{}
    foreach ($line in $lines) {
        if ($line -match "^BenchmarkDB/(\w+)(-(\d+))?\s+\d+\s+([\d.]+)\s+ns/op\s+(\d+)\s+B/op\s+(\d+)\s+allocs/op") {
            $impl = $Matches[1]
            $cpu = if ($Matches[3]) { $Matches[3] } else { "1" }
            $nsop = [double]$Matches[4]
            $bop = [int]$Matches[5]
            $allocs = [int]$Matches[6]
            
            $key = "$impl-$cpu"
            $results[$key] = @{ ns = $nsop; b = $bop; allocs = $allocs }
        }
    }
    
    # Get unique CPUs and implementations (Simple first)
    $cpus = $results.Keys | ForEach-Object { ($_ -split "-")[1] } | Sort-Object { [int]$_ } -Unique
    $impls = @("Simple", "Sharded") | Where-Object { $results.Keys -match "^$_-" }
    
    # If no known impls, discover them
    if ($impls.Count -eq 0) {
        $impls = $results.Keys | ForEach-Object { ($_ -split "-")[0] } | Sort-Object -Unique
    }
    
    Write-Host ""
    
    # Build header row
    $header = "{0,-5} |" -f "CPU"
    $divider = "{0,-5} |" -f "-----"
    foreach ($impl in $impls) {
        $header += " {0,10} {1,8} {2,6} |" -f "$impl ns", "B/op", "alloc"
        $divider += " {0,10} {1,8} {2,6} |" -f "----------", "--------", "------"
    }
    $header += " {0,8}" -f "Speedup"
    $divider += " {0,8}" -f "--------"
    
    Write-Host $header -ForegroundColor Cyan
    Write-Host $divider -ForegroundColor Cyan
    
    # Output rows by CPU
    foreach ($cpu in $cpus) {
        $row = "{0,-5} |" -f $cpu
        $nsValues = @()
        foreach ($impl in $impls) {
            $key = "$impl-$cpu"
            if ($results.ContainsKey($key)) {
                $r = $results[$key]
                $row += " {0,10:N1} {1,8} {2,6} |" -f $r.ns, $r.b, $r.allocs
                $nsValues += $r.ns
            } else {
                $row += " {0,10} {1,8} {2,6} |" -f "-", "-", "-"
            }
        }
        # Calculate speedup if exactly 2 implementations
        if ($impls.Count -eq 2 -and $nsValues.Count -eq 2 -and $nsValues[1] -gt 0) {
            $speedup = $nsValues[0] / $nsValues[1]
            $row += " {0,7:N1}x" -f $speedup
        }
        Write-Host $row
    }
    Write-Host ""
}

if ($Raw) {
    # Raw benchstat output (3 tables)
    Write-Host "Running benchmark..." -ForegroundColor Cyan
    Invoke-Expression $benchCmd | benchstat -
}
elseif ($Compare) {
    # Compare mode: save old, run new, show diff
    if (Test-Path "old.txt") { Remove-Item "old.txt" }
    if (Test-Path "new.txt") { Rename-Item "new.txt" "old.txt" }
    
    Write-Host "Running benchmark..." -ForegroundColor Cyan
    $result = Invoke-Expression $benchCmd
    $result | Out-File "new.txt"
    
    if (Test-Path "old.txt") {
        Write-Host "`n=== Comparison (use -Raw for detailed stats) ===" -ForegroundColor Green
        benchstat old.txt new.txt
    } else {
        Write-Host "`n=== Results ===" -ForegroundColor Green
        Format-BenchResults $result
    }
}
elseif ($Save) {
    $timestamp = Get-Date -Format "yyyyMMdd-HHmmss"
    $filename = "bench-$timestamp.txt"
    
    Write-Host "Running benchmark..." -ForegroundColor Cyan
    $result = Invoke-Expression $benchCmd | Out-String
    $result | Out-File $filename
    
    Write-Host "`nSaved to: $filename" -ForegroundColor Green
    Format-BenchResults $result
}
else {
    # Default: single table output
    Write-Host "Running benchmark..." -ForegroundColor Cyan
    $result = Invoke-Expression $benchCmd | Out-String
    Format-BenchResults $result
}
