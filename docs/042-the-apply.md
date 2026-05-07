# 042 — The Apply

041 traced the throughput ceiling to server-side serialization: `handleRequest` blocks on each proposal's full round-trip before reading the next request. gRPC was identified as the fix — each RPC gets its own goroutine, proposals overlap on `propc`, throughput scales. That was the hypothesis.

```
kv-go with gRPC (single-node, 50000 requests):

1 conn:   180 req/s, P50 5.4ms
5 conns:  178 req/s, P50 26.6ms
50 conns: 169 req/s, P50 296ms
```

gRPC changes nothing. Same ~170 req/s ceiling. The hypothesis was wrong.

## Reasoning

### Why gRPC didn't help

Built a gRPC service (`kvpb.KV` with `Put` and `Get` RPCs), wired it into the server alongside the binary protocol, and added `-grpc` mode to `kv-bench`. Each gRPC RPC gets its own goroutine on the server — exactly what 041 said was needed.

Instrumented `handleBatch` to measure batch sizes under gRPC load at 50 connections. The [batch log](files/042-grpc-batch-log.txt) shows a repeating steady-state pattern:

```
entries=46 committed=1     ← 46 new entries batched, commit 1 from prior
entries=1  committed=46    ← 1 straggler, commit the 46
entries=1  committed=1     ← commit the straggler
entries=1  committed=1     ← one more
```

gRPC IS batching — `entries=46` in steady state, not 1. The Raft event loop receives 46 proposals per Ready cycle. The probabilistic `select` batching from 041 is working. But throughput is still 170 req/s.

Batch size went from 1 to 46 and throughput didn't move. That means the bottleneck is cycle time, not batch size. Something in each Ready/Advance cycle takes ~280ms regardless of how many entries it carries.

### Isolating the cost: fsync

Each cycle calls `handleBatch` → `Save()` → WAL fsync. Raft WAL fsync is the obvious suspect — Windows fsync is expensive. Disabled the Raft WAL fsync (`s.rw.sync()` in `DurableStorage.Save`):

```
gRPC 50c, Raft WAL fsync disabled:  169 req/s, P50 295ms
```

No change. Disabled the engine WAL fsyncs too (`writeIndex` and `writeValue` both call `file.Sync()`):

```
gRPC 50c, ALL fsyncs disabled:  185 req/s, P50 271ms
```

10% improvement at best. Fsync is not the bottleneck.

### The 5ms floor

With all fsyncs disabled, measured single-connection latency to isolate cycle time:

```
gRPC 1c, all fsyncs disabled:  175 req/s, P50 5.7ms
```

5.7ms per request with no fsync. That's the floor — the minimum time for one entry to travel through propose → Ready → handleBatch → apply → response. 1/0.0057 ≈ 175 req/s. This floor is the same at all concurrency levels, which means something in the pipeline adds ~5ms per cycle regardless of batch size.

### The group commit timer

Traced the apply path. After `handleBatch` sends committed entries to the apply loop via `applyc`, the apply loop calls `sm.Put()` for each entry. `Put()` enqueues to the engine's group committer and **blocks on `<-respCh`** until the group committer flushes.

The group committer flushes on two conditions:
1. Batch full: 100 entries accumulated
2. Timer tick: `DefaultSyncInterval = 10ms`

With 1 committed entry per cycle, the batch never fills. The timer fires every 10ms. Average wait: ~5ms. That's the entire P50.

Proved it by reducing the sync interval to 1ms (fsyncs still disabled):

```
gRPC 1c, all fsyncs disabled, sync=1ms:   1025 req/s, P50 1.0ms
gRPC 50c, all fsyncs disabled, sync=1ms:  1411 req/s, P50 35ms
binary 1c, all fsyncs disabled, sync=1ms: 1174 req/s, P50 0.66ms
binary 50c, all fsyncs disabled, sync=1ms: 1399 req/s, P50 36ms
```

6x improvement at 1c. 8x at 50c. Binary and gRPC produce identical throughput — the protocol never mattered. The bottleneck was always the engine's group commit timer blocking the apply path.

### What etcd does

Before building a fix, checked how etcd handles this. etcd uses bbolt (a B+ tree with transactions) as its state machine. bbolt commits are expensive — two fdatasyncs per transaction. etcd's default `BatchInterval` is **100ms** — 10x larger than our engine's 10ms. If etcd blocked on the bbolt commit per apply, it would be 10x slower than us. But etcd scales to 7000+ req/s.

The difference: etcd's apply path writes to an **in-memory buffer** (`batchTxBuffered`), not to disk. The `batchTx.Put()` call returns immediately. A background goroutine commits the buffer to bbolt every 100ms or every 10,000 operations — whichever comes first.

The state machine is eventually durable, not per-entry durable. On crash, etcd replays committed Raft WAL entries that are beyond bbolt's last persisted consistent index. The Raft WAL is the durability guarantee, not the state machine.

| | kv-go (before) | etcd |
|---|---|---|
| Apply one entry | `sm.Put()` → block on group committer → fsync | `batchTx.Put()` → in-memory buffer → return |
| Engine commit | every 10ms (group committer timer) | every 100ms (bbolt batch timer) |
| Apply blocks on commit? | **yes** | **no** |
| Recovery | replay engine WAL | replay Raft WAL beyond bbolt's consistent index |

The fix is the same pattern: don't block the apply path on the engine commit. **The Raft WAL already guarantees durability.**

### PutAsync

Added `PutAsync(key, value)` to the engine:
1. Apply to in-memory state immediately (`putInternal` under `s.mu.Lock`)
2. Enqueue WAL write to the group committer's buffered channel (200 slots) — blocks only if the buffer is full, which provides natural backpressure
3. Return without waiting for the flush timer or fsync

The server's apply path calls `PutAsync` instead of `Put`. The `proposePut` waiter is triggered immediately after the in-memory write, before the engine WAL flushes.

### Why skipping the engine WAL is safe

On crash, memory is lost. If PutAsync dropped some engine WAL writes, the engine state on disk is incomplete. Recovery works because the server already has this path:

1. Restart → `DurableStorage` loads Raft WAL → rebuilds committed entries and snapshot boundary
2. Server passes the snapshot boundary as `Config.Applied` to the Raft node
3. Raft re-emits all committed entries from `(Applied, commitIndex]` through the Ready channel
4. The apply loop receives them and calls `PutAsync` again → memory rebuilt, engine WAL catches up

This is the same path that handles normal crash recovery (episode 037g). The only difference is that before PutAsync, the engine WAL was always complete — now it may have gaps. But the Raft WAL never has gaps (it's the source of truth), and replay fills them.

The engine WAL still serves a purpose: it speeds up cold start by providing a partial checkpoint. Without it, every restart would replay the entire Raft WAL from the last snapshot. With it, most entries are already in the engine, and only the tail is replayed.

Reads are safe on both sides of a crash. Before crash, `PutAsync` applies to the in-memory map before triggering the waiter — any subsequent ReadIndex-gated `Get` sees the value. After crash, Raft WAL replay rebuilds the map before the server accepts connections — ReadIndex waits for `applyWait` to reach the read index, which means all committed entries up to that point have been replayed. The linearizability guarantee is unchanged: ReadIndex proves leadership via quorum, then waits for the apply loop to catch up. Whether the apply loop writes to disk or memory doesn't affect read safety.

```
kv-go with PutAsync (single-node):

1 conn:   934 req/s, P50 1.2ms     (was 170, 5.5x)
5 conns:  2424 req/s, P50 1.9ms    (was 175, 13.9x)
```

Throughput scales. The bottleneck is gone.

## Shape

### Engine

`DB.PutAsync(key string, value []byte)` — new method, supplements `Put`. Applies to in-memory map immediately, enqueues engine WAL write non-blocking. `Put` is unchanged for callers that need per-entry durability.

`shard.putAsync(key, value)`:
```go
func (s *shard) putAsync(key string, value []byte) {
    s.putInternal(key, value)        // memory — immediate, under s.mu
    select {
    case s.committer.reqCh <- req:   // WAL — enqueue (buffered: 200 slots)
    case <-s.committer.stopCh:       // shutdown
    }
}
```

The channel is buffered at `syncBatchSize*2 = 200`. The group committer drains up to 100 entries per flush at `DefaultSyncInterval = 10ms`, sustaining ~10,000 writes/s. Under normal load the send returns immediately. Under extreme burst, the send blocks until a slot opens — natural backpressure that slows the apply loop and the Raft node loop.

Writes must not be dropped. `MaybeCompact` deletes Raft WAL segments after the apply loop processes them, assuming the engine WAL has received every entry. A dropped write would create a gap: the Raft WAL segment is gone, the engine WAL never got it, and the entry is unrecoverable after crash.

Group committer `flush` guards `respCh` — async requests have `respCh == nil`:
```go
if r.respCh != nil {
    s.putInternal(r.key, r.value)    // sync path: memory after WAL
    r.respCh <- nil
}
// async path: memory already applied, WAL write done
```

### Server

`StateMachine` interface gains `PutAsync(key string, value []byte)`.

`applyEntry` calls `sm.PutAsync` instead of `sm.Put`. The waiter is triggered immediately — no blocking on engine commit.

### Dispatcher

`handleRequest` no longer kills connections on request errors. Decode failures, unsupported commands, and handler errors (including proposal timeouts) all send a `StatusError` response and continue the loop. The connection only closes on transport write failure (connection dead).

### gRPC (bonus, not the fix)

`kvpb/kv.proto` — `service KV { rpc Put; rpc Get }`.

`server/handler_grpc.go` — thin wrapper calling `proposePut` and `proposeRead`.

Server `Options.GRPCPort`, `--grpc-port` flag, `grpc.NewServer()` lifecycle in `Start`/`Shutdown`.

`kv-bench -grpc` flag — uses shared `grpc.ClientConn` across workers.

gRPC didn't improve throughput but it's a clean API surface for future use and makes the etcd comparison apples-to-apples.

## Minimum tests

**Invariant:** the apply path does not block on engine commits; throughput scales with concurrency on a single-node cluster.

1. **PutAsync applies to memory immediately** — after `PutAsync(k, v)`, `Get(k)` returns `v` without waiting for a group committer flush.
2. **PutAsync does not block** — `PutAsync` returns in under 1ms even when the group committer channel is full.
3. **Throughput scales** — single-node kv-go at 5 connections produces measurably higher throughput than at 1 connection (not the 1.2x ceiling from 041).
4. **Correctness preserved** — all requests complete with 0 errors at 5 connections.
5. **gRPC path equivalence** — gRPC `Put` and `Get` produce the same results as the binary protocol for the same keys.

## Open threads

1. **Double WAL** — the engine WAL and the Raft WAL both persist the same data. `PutAsync` makes the engine WAL best-effort, but it still runs. A cleaner architecture would make the engine WAL opt-out when Raft owns durability, or replace the engine entirely with a pure in-memory map + Raft WAL replay.
2. **Backpressure under extreme load** — `putAsync` enqueues to a 200-slot buffered channel. Under sustained bursts beyond the group committer's drain rate (~10,000 writes/s), the channel fills and the apply loop blocks, which stalls the Raft node loop. An unbounded async buffer would decouple them fully.
3. **Heartbeat-gated commit** — 3-node P50 of 100ms (from 041). Separate from the apply path issue.
4. **3-node error rate** — 679/1000 errors at 5 connections (from 041). Multi-node bugs under load.

## Benchmark results

All runs: single-node kv-go on localhost, 128-byte values, PUT-only.

### Before PutAsync (041 baseline)

| Conns | Binary | gRPC | Errors |
|-------|--------|------|--------|
| 1     | 170 req/s | 180 req/s | 0 |
| 5     | 175 req/s | 178 req/s | 0 |
| 50    | 178 req/s | 169 req/s | 0 |

### Isolation experiments (all fsyncs disabled)

| Conns | sync=10ms | sync=1ms |
|-------|----------|----------|
| 1     | 175 req/s | 1025 req/s |
| 50    | 185 req/s | 1411 req/s |

Reducing the group commit timer from 10ms to 1ms gave 6x. Disabling fsync alone gave 10%. The timer was the bottleneck.

### After PutAsync (real fsyncs, default 10ms sync interval)

| Conns | Throughput | P50 | Errors |
|-------|-----------|-----|--------|
| 1     | 934 req/s | 1.2ms | 0 |
| 5     | 2424 req/s | 1.9ms | 0 |
