# kv-go

A distributed key-value store built on a custom Raft consensus implementation in Go. Every layer — consensus, replication, storage, transport — is written from scratch. 200+ unit tests across the Raft, server, engine, and transport layers.

## Quick start

```bash
cd src
go run ./cmd/kv-server/ --node-id 1 --data-dir /tmp/kv1 --port 4000

# In another terminal:
go run ./cmd/kv-cli/ -addr 127.0.0.1:4000
> put mykey myvalue
OK
> get mykey
myvalue
> quit
```

## Architecture

```
            Client (binary TCP / gRPC / HTTP)
                        │
                    ┌───▼───┐
                    │Server │  proposePut / proposeRead
                    └───┬───┘
                        │ Propose
                ┌───────▼────────┐
                │   Raft Node    │  pure state machine, no I/O
                │  (event loop)  │  election, replication, ReadIndex
                └───────┬────────┘
                        │ Ready
                ┌───────▼────────┐
                │   Raft Host    │  persist WAL, send peer msgs
                │ Ready/Advance  │  committed → apply loop
                └───────┬────────┘
                        │ CommittedEntries
                ┌───────▼────────┐
                │  Apply Loop    │  PutAsync (memory-first)
                │                │  trigger waiters
                └───────┬────────┘
                        │
                ┌───────▼────────┐
                │    Engine      │  256 shards, WAL, compaction
                └────────────────┘
```

### Write path

1. Client sends `PUT key value` (binary protocol or gRPC)
2. Server registers a waiter, proposes entry to Raft via `propc`
3. Node loop appends to `unstable`, builds `Ready` with the entry
4. Raft host persists to WAL (`Save` + fsync), sends `MsgApp` to peers
5. Peers acknowledge → leader advances `commitIndex`
6. Next `Ready` carries the entry in `CommittedEntries`
7. Apply loop calls `PutAsync` → in-memory state updated immediately
8. Waiter triggered → client receives response

Single-node latency: ~1ms. Multi-node adds one network round-trip.

### Read path

1. Client sends `GET key`
2. Server calls `ReadIndex` — leader sends heartbeat to confirm it still holds quorum
3. Quorum responds → leader returns the committed index as proof
4. Server waits for apply loop to reach that index (`applyWait`)
5. Reads from in-memory map — guaranteed to reflect all committed writes

This is linearizable: the read sees every write that completed before it started.

## What's implemented

| Feature | Status | Episode |
|---------|--------|---------|
| Raft leader election (tick-driven) | ✅ | 036 |
| Log replication (MsgApp/MsgAppResp) | ✅ | 036 |
| Ready/Advance contract | ✅ | 036 |
| Segmented WAL with auto-compaction | ✅ | 037f |
| Crash recovery via `Config.Applied` | ✅ | 037g |
| Dedicated heartbeat messages | ✅ | 037h |
| Linearizable reads (ReadIndex) | ✅ | 037l |
| CheckQuorum (leader step-down) | ✅ | 037m |
| PreVote (partition protocol) | ✅ | 037n |
| Leader transfer | ✅ | 037o |
| Unified quorum primitive | ✅ | 038 |
| Self-ack + ProgressTracker | ✅ | 039 |
| Two-layer raft log (stable/unstable) | ✅ | 040 |
| Non-blocking apply (PutAsync) | ✅ | 042 |
| gRPC API | ✅ | 042 |
| Jepsen linearizability testing | ✅ | 037k–037l |
| Membership changes | ❌ | — |
| Snapshot transfer (full state) | ❌ | — |
| Lease-based reads | ❌ | — |
| Pipelined Ready/Advance | ❌ | — |

## Key design decisions

### 1. Two-layer raft log (episode 040)

**Problem:** Late proposals between `Ready()` and `Advance()` were silently lost — the node loop cleared the log on advance.

**Decision:** Split the log into `storage` (persisted) and `unstable` (in-memory). Entries enter `unstable` on propose, graduate to `storage` via `stableTo(index, term)` on advance. Same architecture as etcd/raft.

**Why it matters:** This is the foundation for concurrent proposals at any connection count. Without it, the system could only safely handle one proposal per Ready cycle.

### 2. Non-blocking apply path (episode 042)

**Problem:** Throughput plateaued at ~170 req/s regardless of concurrency. Traced through three hypotheses:
- gRPC? No — same throughput, but batch logs showed entries=46 (batching works)
- fsync? No — disabling all fsyncs gained only 10%
- Group commit timer? **Yes** — engine's 10ms flush timer blocked every `sm.Put()` for ~5ms average

**Decision:** `PutAsync` applies to in-memory state immediately, enqueues WAL write without waiting for flush. Same pattern as etcd's `batchTxBuffered` — the Raft WAL is the durability guarantee, not the state machine.

**Result:** 170 → 934 req/s at 1 connection (5.5x), 170 → 2424 req/s at 5 connections (13.9x).

### 3. ReadIndex over lease reads (episode 037l)

**Problem:** Followers served stale reads. Jepsen caught it: two concurrent reads returned different values for the same key.

**Decision:** Every read goes through ReadIndex — leader proves it still holds quorum via heartbeat round-trip, then waits for the apply loop to catch up. More expensive than lease reads, but correct without clock assumptions.

**Tradeoff:** One extra network round-trip per read vs. lease reads that assume bounded clock skew. Chose correctness over latency — clock skew bugs are silent and catastrophic.

### 4. PreVote + CheckQuorum (episodes 037m, 037n)

**Problem:** A partitioned node's term advances during repeated elections. When it rejoins, its higher term disrupts the healthy leader — even though the node has no useful data.

**Decision:** Two-stage election (PreVote filters disruptive candidates without advancing terms) + CheckQuorum (leader steps down if it can't confirm quorum within one election timeout). Together they bound both the disruption risk and the detection latency of a network partition.

## Benchmarks

Single-node, 128-byte values, PUT-only:

| Connections | Throughput | P50 | P99 |
|-------------|-----------|-----|-----|
| 1 | 934 req/s | 1.2ms | 2.9ms |
| 5 | 2424 req/s | 1.9ms | 10.3ms |

etcd comparison (same machine, same workload via etcd's own benchmark tool):

| Connections | etcd | kv-go |
|-------------|------|-------|
| 1 | 627 req/s | 934 req/s |
| 5 | 2103 req/s | 2424 req/s |

kv-go outperforms etcd at low concurrency because it is a much thinner system — no MVCC, no watch, no lease, no auth, no metrics middleware. At high concurrency (50 connections), etcd pulls ahead (7311 req/s) via HTTP/2 multiplexing and pipelined Ready/Advance, both absent in kv-go.

## Project structure

```
src/
├── raft/           # Raft state machine (pure logic, no I/O)
│   ├── tracker/    # ProgressTracker, quorum computations
│   └── quorum/     # MajorityConfig, vote tallying
├── raftpb/         # Protobuf: Raft messages, entries, hard state
├── kvpb/           # Protobuf: KV service (gRPC Put/Get)
├── server/         # Server orchestration, handlers, apply loop
├── engine/         # Sharded storage engine with WAL
├── transport/      # Network abstraction (StreamTransport, RequestTransport)
│   └── raft/       # Raft peer transport (TCP, per-peer reconnect)
├── protocol/       # Binary wire protocol (KV requests/responses)
├── pkg/wait/       # Propose-apply synchronization primitive
└── cmd/
    ├── kv-server/  # Server binary
    ├── kv-bench/   # Benchmark tool (binary + gRPC modes)
    └── kv-cli/     # CLI client
```

## Design documentation

Each episode is a self-contained thinking doc in [`docs/`](docs/). The system is built through a question-driven methodology — every feature exists because a specific failure mode demanded it.

**Phase 1 (001–035):** Redis-like store — crashes, replication, quorum writes/reads, fencing, failover.

**Episode 036 — The watershed:** The system stops being a KV store that bolts on consensus and becomes a consensus system with a KV state machine.

**Phase 2 (036–042):** Raft consensus system — state machine, Ready/Advance, WAL, ReadIndex, PreVote, CheckQuorum, leader transfer, raftLog, throughput diagnosis, non-blocking apply.

## Articles

- [Stop Before the Boundary and Prove What You Have](docs/articles/stop-before-the-boundary.md)
- [Software Design Moves Top-Down, Bottom-Up, and Back Again](docs/articles/back-and-forth-in-software-design.md)

## References

- [Raft paper](https://raft.github.io/raft.pdf) (Ongaro & Ousterhout)
- [Raft dissertation](https://github.com/ongardie/dissertation) (Diego Ongaro)
- [etcd](https://github.com/etcd-io/etcd) — architecture and failure handling reference
- [etcd/raft](https://github.com/etcd-io/raft) — Raft library design reference
- [bbolt](https://github.com/etcd-io/bbolt) — embedded storage engine reference
- [TiKV](https://github.com/tikv/tikv) — distributed KV design reference
