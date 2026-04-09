# Build kv-server for linux/amd64 and launch the Jepsen Docker cluster.
# Usage: .\run.ps1 [lein-args...]
#   .\run.ps1                         # build + start cluster (interactive shell)
#   .\run.ps1 "lein run test -w basic" # build + run basic workload
param(
    [Parameter(ValueFromRemainingArguments)]
    [string[]]$LeinArgs
)

$ErrorActionPreference = "Stop"

$ScriptDir   = $PSScriptRoot
$RepoRoot    = Resolve-Path "$ScriptDir\..\.."
$DockerDir   = $ScriptDir

Write-Host "==> Cross-compiling kv-server for linux/amd64..."
New-Item -ItemType Directory -Force -Path "$DockerDir\bin" | Out-Null
$env:GOOS = "linux"; $env:GOARCH = "amd64"; $env:CGO_ENABLED = "0"
go build -C "$RepoRoot\src" -o "$DockerDir\bin\kv-server" ./cmd/kv-server
Remove-Item Env:\GOOS, Env:\GOARCH, Env:\CGO_ENABLED

# Generate test-only SSH key pair (if not already present)
$SshDir = "$DockerDir\docker\secret"
if (-not (Test-Path "$SshDir\id_rsa")) {
    Write-Host "==> Generating SSH key pair for Jepsen nodes..."
    New-Item -ItemType Directory -Force -Path $SshDir | Out-Null
    ssh-keygen -t rsa -b 4096 -f "$SshDir/id_rsa" -N "" -q
}

Write-Host "==> Building Docker images..."
docker compose -f "$DockerDir\docker-compose.yml" build

if (-not $LeinArgs) {
    Write-Host "==> Starting cluster (interactive control shell)..."
    Write-Host "    Run:  lein run test -w basic"
    Write-Host "    Exit: Ctrl-D, then: docker compose -f $DockerDir\docker-compose.yml down"
    docker compose -f "$DockerDir\docker-compose.yml" run --service-ports --rm control
} else {
    $cmd = $LeinArgs -join " "
    Write-Host "==> Running: $cmd"
    docker compose -f "$DockerDir\docker-compose.yml" run --service-ports --rm control bash -c $cmd
}

Write-Host "==> Tearing down cluster..."
docker compose -f "$DockerDir\docker-compose.yml" down
