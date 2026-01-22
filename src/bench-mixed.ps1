# bench-mixed.ps1 - Mixed workload benchmark runner for kv-go
param(
    [int]$Count = 3,
    [string]$CPU = "8",
    [switch]$Full
)

$ErrorActionPreference = "Stop"

if ($Full) {
    $Count = 6
    $CPU = "1,4,8,16"
}

$benchCmd = "go test -bench=BenchmarkMixed -benchmem `"-cpu=$CPU`" -count=$Count"

function Format-MixedResults {
    param([string]$Output)
    
    $lines = $Output -split "`n" | Where-Object { $_ -match "^BenchmarkMixed" }
    
    # Parse results: BenchmarkMixed/Read90/Simple-8
    $results = @{}
    foreach ($line in $lines) {
        if ($line -match "^BenchmarkMixed/(\w+)/(\w+)(-(\d+))?\s+\d+\s+([\d.]+)\s+ns/op\s+(\d+)\s+B/op\s+(\d+)\s+allocs/op") {
            $ratio = $Matches[1]
            $impl = $Matches[2]
            $cpu = if ($Matches[4]) { $Matches[4] } else { "1" }
            $nsop = [double]$Matches[5]
            $bop = [int]$Matches[6]
            $allocs = [int]$Matches[7]
            
            $key = "$ratio-$impl-$cpu"
            $results[$key] = @{ ns = $nsop; b = $bop; allocs = $allocs }
        }
    }
    
    # Get unique values
    $ratios = @("Write100", "Read50", "Read90", "Read99") | Where-Object { 
        $r = $_; $results.Keys | Where-Object { $_ -match "^$r-" } 
    }
    $cpus = $results.Keys | ForEach-Object { ($_ -split "-")[2] } | Sort-Object { [int]$_ } -Unique
    $impls = @("Simple", "Sharded")
    
    foreach ($cpu in $cpus) {
        Write-Host ""
        Write-Host "=== CPU: $cpu ===" -ForegroundColor Green
        Write-Host ""
        
        # Header
        $header = "{0,-12} |" -f "Workload"
        $divider = "{0,-12} |" -f "------------"
        foreach ($impl in $impls) {
            $header += " {0,12} |" -f "$impl ns"
            $divider += " {0,12} |" -f "------------"
        }
        $header += " {0,10} | {1,-20}" -f "Speedup", "Visual"
        $divider += " {0,10} | {1,-20}" -f "----------", "--------------------"
        
        Write-Host $header -ForegroundColor Cyan
        Write-Host $divider -ForegroundColor Cyan
        
        foreach ($ratio in $ratios) {
            $row = "{0,-12} |" -f $ratio
            $nsValues = @()
            
            foreach ($impl in $impls) {
                $key = "$ratio-$impl-$cpu"
                if ($results.ContainsKey($key)) {
                    $ns = $results[$key].ns
                    $row += " {0,12:N1} |" -f $ns
                    $nsValues += $ns
                } else {
                    $row += " {0,12} |" -f "-"
                }
            }
            
            # Calculate speedup and visual bar
            if ($nsValues.Count -eq 2 -and $nsValues[1] -gt 0) {
                $speedup = $nsValues[0] / $nsValues[1]
                $barLen = [Math]::Min([Math]::Floor($speedup), 20)
                $bar = "█" * $barLen
                $color = if ($speedup -ge 10) { "Green" } elseif ($speedup -ge 5) { "Yellow" } else { "White" }
                
                $row += " {0,9:N1}x |" -f $speedup
                Write-Host $row -NoNewline
                Write-Host " $bar" -ForegroundColor $color
            } else {
                Write-Host $row
            }
        }
    }
    
    # Summary table
    Write-Host ""
    Write-Host "=== Summary (B/op and allocs) ===" -ForegroundColor Green
    Write-Host ""
    
    $cpu = $cpus | Select-Object -First 1
    $summaryHeader = "{0,-12} |" -f "Workload"
    $summaryDiv = "{0,-12} |" -f "------------"
    foreach ($impl in $impls) {
        $summaryHeader += " {0,8} {1,8} |" -f "$impl B", "allocs"
        $summaryDiv += " {0,8} {1,8} |" -f "--------", "--------"
    }
    
    Write-Host $summaryHeader -ForegroundColor Cyan
    Write-Host $summaryDiv -ForegroundColor Cyan
    
    foreach ($ratio in $ratios) {
        $row = "{0,-12} |" -f $ratio
        foreach ($impl in $impls) {
            $key = "$ratio-$impl-$cpu"
            if ($results.ContainsKey($key)) {
                $r = $results[$key]
                $row += " {0,8} {1,8} |" -f $r.b, $r.allocs
            } else {
                $row += " {0,8} {1,8} |" -f "-", "-"
            }
        }
        Write-Host $row
    }
    
    Write-Host ""
}

Write-Host "Running mixed workload benchmark..." -ForegroundColor Cyan
Write-Host "Workloads: 100% Write, 50/50, 90% Read, 99% Read" -ForegroundColor Gray
Write-Host "Keys: 10,000 pre-populated, random access" -ForegroundColor Gray
Write-Host ""

$result = Invoke-Expression $benchCmd | Out-String
Format-MixedResults $result
