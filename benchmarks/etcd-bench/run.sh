#!/bin/bash
# 041 — etcd single-node benchmark using etcd's official benchmark tool
#
# Usage: bash benchmarks/etcd-bench/run.sh
# Prerequisites: Docker, docker compose

set -euo pipefail
export MSYS_NO_PATHCONV=1

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
RESULTS_DIR="$SCRIPT_DIR/../../.test/etcd-bench"
mkdir -p "$RESULTS_DIR"

cd "$SCRIPT_DIR"

echo "=== Cleanup previous runs ==="
docker rm -f etcd-bench 2>/dev/null || true
docker compose down 2>/dev/null || true

echo "=== Building and starting etcd ==="
docker compose build
docker compose up -d etcd
sleep 5

echo ""
echo "=== etcd single-node: 1 conn, 1000 PUTs ==="
docker exec etcd-bench benchmark \
  --endpoints=http://127.0.0.1:2379 \
  --conns=1 --clients=1 \
  put --total=1000 --key-size=16 --val-size=128 \
  2>&1 | tee "$RESULTS_DIR/etcd-1c.txt"

echo ""
echo "=== etcd single-node: 5 conns, 5000 PUTs ==="
docker exec etcd-bench benchmark \
  --endpoints=http://127.0.0.1:2379 \
  --conns=5 --clients=5 \
  put --total=5000 --key-size=16 --val-size=128 \
  2>&1 | tee "$RESULTS_DIR/etcd-5c.txt"

echo ""
echo "=== etcd single-node: 50 conns, 10000 PUTs ==="
docker exec etcd-bench benchmark \
  --endpoints=http://127.0.0.1:2379 \
  --conns=50 --clients=50 \
  put --total=10000 --key-size=16 --val-size=128 \
  2>&1 | tee "$RESULTS_DIR/etcd-50c.txt"

echo ""
echo "=== Cleanup ==="
docker compose down

echo ""
echo "Results saved to $RESULTS_DIR/"
