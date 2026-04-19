#!/usr/bin/env bash
# Build kv-server for linux/amd64 and launch the Jepsen Docker cluster.
# Usage: ./run.sh [lein-args...]
#   ./run.sh                         # build + start cluster (interactive shell)
#   ./run.sh lein run test -w basic  # build + run basic workload
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
DOCKER_DIR="$SCRIPT_DIR"

echo "==> Cross-compiling kv-server for linux/amd64..."
mkdir -p "$DOCKER_DIR/bin"
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
    go build -C "$REPO_ROOT/src" -o "$DOCKER_DIR/bin/kv-server" ./cmd/kv-server

# Generate test-only SSH key pair (if not already present)
SSH_DIR="$DOCKER_DIR/docker/secret"
if [ ! -f "$SSH_DIR/id_rsa" ]; then
    echo "==> Generating SSH key pair for Jepsen nodes..."
    mkdir -p "$SSH_DIR"
    ssh-keygen -t rsa -b 4096 -f "$SSH_DIR/id_rsa" -N "" -q
fi

echo "==> Building Docker images..."
docker compose -f "$DOCKER_DIR/docker-compose.yml" build

if [ $# -eq 0 ]; then
    echo "==> Starting cluster (interactive control shell)..."
    echo "    Run:  lein run test -w basic"
    echo "    Exit: Ctrl-D, then: docker compose -f $DOCKER_DIR/docker-compose.yml down"
    docker compose -f "$DOCKER_DIR/docker-compose.yml" run --service-ports --rm control
else
    echo "==> Running: $*"
    docker compose -f "$DOCKER_DIR/docker-compose.yml" run --service-ports --rm control bash -c "$*"
fi

echo "==> Tearing down cluster..."
docker compose -f "$DOCKER_DIR/docker-compose.yml" down
