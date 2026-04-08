# 037j — The Bridge

Unit tests prove code paths, not system behavior. Nothing in the test suite today can tell us whether five nodes running concurrently actually produce linearizable results — or silently return stale data under partition. We have no system-level oracle. Before Jepsen can be that oracle, it needs to talk to kv-go — and kv-go only speaks a custom binary protocol.

## Boundary

HTTP API on the server, Jepsen test project with workload separation, cross-platform Docker Compose setup. One command runs a linearizability test against a live 5-node cluster.

In scope:
- `GET /kv/{key}` and `PUT /kv/{key}` on a dedicated HTTP port (`-http-port`, disabled when 0)
- Extracted `proposePut(key, value)` shared by binary and HTTP handlers
- Jepsen test project: `jepsen/kvgo/` with db lifecycle, HTTP client, basic workload, linearizability checker
- Docker Compose: 1 control node (Leiningen + test code) + 5 db nodes (Debian + SSH + kv-server binary)
- Cross-platform: same `docker compose up` on Windows, Mac, Linux
- Result visualization: `checker/perf` (latency/throughput graphs), `timeline/html` (interactive operation timeline), `lein run serve` (web UI to browse results, port exposed from Docker)

Deferred:
- Network partition nemesis (037k)
- ReadIndex (037l)
- CAS operations

Out of scope:
- Clock skew testing (Docker containers lack real clocks)

## Design

### HTTP API

Two endpoints on a dedicated port:

```
GET /kv/{key}  → 200 + body (value) | 404 (not found)
PUT /kv/{key}  → 200 (committed)    | 503 (error/timeout)
```

GET reads from the local state machine — no ReadIndex, no leader check. This means reads are intentionally unsafe: a partitioned leader will serve stale values. That's deliberate; 037k will add a partition nemesis and prove it fails. PUT proposes through Raft — only the leader can propose, so PUTs to a follower return 503. The Jepsen client reports these as `:info` (outcome unknown). The HTTP layer is a thin translation — no new state, no new paths. Uses Go 1.22+ `http.ServeMux` routing (`GET /kv/{key}`), no dependencies.

### Extracted propose path

`proposePut(key, value) error` — shared by `handlePut` (binary) and `httpPut` (HTTP). One code path for writes, two wire formats.

### Jepsen workload separation

```
src/jepsen/kvgo.clj          ← shared infra (db, client, config) + CLI dispatcher
src/jepsen/kvgo/basic.clj    ← basic register workload (PUT/GET, no nemesis)
```

Workloads are selected via `-w` flag: `lein run test -w basic`. New workloads are new files registered in the `workloads` map.

### `db` lifecycle

Jepsen's `setup!` SSHes into each node and calls `cu/start-daemon!` with the kv-server binary at `/opt/kvgo/kv-server`. Flags are derived from the node name: `-node-id` from the numeric suffix (n1 → 1), `-peers` built by enumerating all other test nodes as `id=hostname:5000`. A 5-second sleep follows to let the cluster elect a leader.

`teardown!` calls `cu/stop-daemon!` then `rm -rf /var/lib/kvgo` to reset state between runs. `log-files` returns the kv-server log for Jepsen's artifact collection.

### Docker architecture

All containers share a `jepsen` bridge network. Docker hostnames (`n1`..`n5`) are the addresses Jepsen uses for SSH and the addresses nodes use for Raft peer communication — no IP coordination needed.

```
docker compose up
  ├─ jepsen-control   ← Leiningen, drives the test
  ├─ n1..n5           ← Debian + SSH + kv-server binary
```

Node images: Debian trixie-slim + openssh-server + iptables (for nemesis) + the cross-compiled binary. Control image: JDK 21 + Leiningen + gnuplot + openssh-client. Cross-compile: `GOOS=linux GOARCH=amd64 go build -o jepsen/kvgo/bin/kv-server ./cmd/kv-server`. `run.ps1` / `run.sh` automate the build-and-launch sequence.

### Visualization

`checker/compose` in the basic workload includes three checkers: `linearizable` (WGL algorithm — polynomial, memory-efficient), `perf` (gnuplot latency/throughput graphs), and `timeline/html` (interactive operation timeline). gnuplot runs inside the control container — it's installed in `control.Dockerfile`. Results land in `bin/store/` (Docker volume). Port 8080 is forwarded from the control container to the host so `lein run serve` is reachable from a browser on the host machine.

## Minimum tests

**Invariant:** the HTTP API provides the same read/write semantics as the binary protocol.

1. **HTTP GET returns value after PUT** — proves the HTTP bridge reaches the state machine through propose-wait-apply.
2. **HTTP GET returns 404 for missing key** — proves the not-found path works through HTTP.
3. **HTTP PUT returns 200 after Raft commit** — proves the HTTP handler shares the propose-wait-apply path.
4. **proposePut shared path** — binary and HTTP both call `proposePut`, producing identical state machine mutations.
5. **Full-cluster HTTP round-trip** — PUT then GET against a real 3-node Raft cluster via HTTP. Integration test.

## Open threads

1. **Jepsen client error refinement** — read timeouts should return `:fail` (idempotent), write timeouts `:info` (unknown). Current client does this but needs validation under load.
2. **Basic workload may pass without nemesis** — without partitions, replication lag is usually too brief to produce visible stale reads, but the checker can return either `{:valid? true}` or `{:valid? false}` depending on timing. 037k adds a partition nemesis to make violations reliable and reproducible.
