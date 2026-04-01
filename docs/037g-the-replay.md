# 037g — The Replay

Crash a node while the apply loop is in progress. Entries 1–100 are committed and persisted to the WAL. The engine has applied 1–95. The process dies. On restart, `newRaft()` loads `commitIndex = 100` from HardState and sets `appliedIndex = commitIndex`. `Ready().CommittedEntries` computes `entriesBetween(100, 100)` — nothing. Entries 96–100 are in the log but never reach the engine again. The state machine has a silent, permanent gap.

The gap exists because `Advance()` and `newRaft()` both treat committed as applied. In normal operation `Advance()` setting `appliedIndex = commitIndex` is safe — `appliedIndex` is volatile (in-memory only, not in HardState), and `Config.Applied` controls the restart position. The server calls `Advance()` after persisting entries but before the engine finishes applying. A crash between `Advance()` and the engine only loses the in-memory cursor — on restart, `Config.Applied` from the snapshot boundary recovers the correct position. But on restart, the old code used `commitIndex` directly, which told raft "the engine has everything" — a lie when the crash interrupted the apply loop.

## Boundary

Fix the restart path so that committed-but-unapplied entries are re-delivered to the state machine after a crash. The application tells raft where the state machine actually is via `Config.Applied`, rather than letting raft assume `appliedIndex = commitIndex`.

In scope:
- `Config.Applied` field — the application tells raft where the state machine actually is, rather than letting raft assume `appliedIndex = commitIndex`
- `newRaft()` initialization — use `Config.Applied` instead of `commitIndex` for `appliedIndex`. When unset (zero), `appliedIndex` stays 0 — full replay from the beginning of the log
- Server startup wiring — `NewRaftHost` reads `SnapshotMeta.LastIncludedIndex` from `DurableStorage` and passes it as `Config.Applied`
- Restart replay — after restart, `Ready().CommittedEntries` re-emits entries in `(Applied, commitIndex]`. The apply loop feeds them into `engine.DB`. PUTs are idempotent, so re-applying already-applied entries is safe

Deferred:
- Persisting `appliedIndex` separately — would shrink the replay window from ~10,000 entries (compaction threshold) to just the unapplied tail. Adds a write per apply batch. etcd chose not to do this; acceptable for now
- Snapshot transfer to slow followers — compaction can still delete entries a far-behind follower needs (remains open from 037f)

Out of scope:
- Non-idempotent commands — the KV layer is Put/Get/Delete, all idempotent. Redis-style INCR/APPEND would break under replay, but the SQL-on-KV path (TiDB, CockroachDB) keeps non-idempotent logic above the KV interface. Not a gap for our architecture
- Linearizable reads

## Design

### What etcd does

etcd's raft library exposes `Config.Applied`: "the last applied index. It should only be set when restarting raft. raft will not return entries to the application smaller or equal to Applied." The application is responsible for providing this value. etcd's server layer derives it from the snapshot metadata on disk:

```go
ep := etcdProgress{
    appliedi: sn.Metadata.Index,   // snapshot boundary
}
```

On restart, etcd replays all committed entries above the snapshot boundary through the apply loop. The replay window is bounded by `SnapshotCount` (10,000 by default) — that's how many entries can accumulate between snapshots. Applied entries that land in the engine a second time are idempotent (put-style overwrites).

etcd explicitly does *not* persist `appliedIndex` to a separate file. The snapshot boundary is the durable lower bound. The gap between the snapshot boundary and the true applied point is accepted as replay cost on restart.

### The fix: Config.Applied

Add an `Applied` field to `raft.Config`:

```go
type Config struct {
    // ...existing fields...
    Applied uint64
}
```

In `newRaft()`, the initialization changes from:

```go
r.appliedIndex = r.commitIndex
```

to:

```go
r.appliedIndex = cfg.Applied
```

One line, one owner. The raft layer does not peek at the snapshot or guess — the application says where the state machine is, and raft trusts it. On a fresh start, `Config.Applied` is 0, `commitIndex` is 0, nothing to replay. On restart, the server reads the compaction boundary from `DurableStorage.Snapshot()` and passes it. This tells raft "the engine may not have entries above this point" — even if raft committed them before the crash.

Two panic guards enforce the contract:

- `Applied > commitIndex` — the application claims to be ahead of what raft committed. Impossible; a caller bug.
- `0 < Applied < firstLogEntry - 1` — the application claims a position inside the compacted range. Raft can't replay entries between Applied and the first available log entry. Also a caller bug. `Applied = 0` is exempt — it means "no state machine position, replay from beginning of available log," and `entriesBetween` clamps to the first available entry.

### Server wiring

`NewRaftHost` already receives `cfg.Storage` (a `*DurableStorage`). On construction, it calls `Storage.Snapshot()` to read the compaction boundary and forwards `snap.LastIncludedIndex` as `raft.Config.Applied`.

No new RPC, no new file, no new persistence. The snapshot metadata file already exists (037f wrote it). We're reading what's already there and using it correctly.

### Replay cost

The worst-case replay on restart is `compactThreshold` entries (10,000). For PUT-heavy workloads, this is a burst of idempotent key-value writes — engine.DB overwrites existing keys. The engine's own WAL absorbs these as normal writes.

If replay becomes a bottleneck (measurable in benchmarks), persisting `appliedIndex` per-batch is the fix — but it costs one extra fsync per apply batch during normal operation. etcd accepted the replay cost. We accept it too, and the open thread stays open.

### What Ready looks like after the fix

Before fix (restart after crash at entry 95):
```
appliedIndex = 100 (from commitIndex)
Ready().CommittedEntries = entriesBetween(100, 100) = []  ← silent gap
```

After fix (restart after crash at entry 95, snapshot at 80):
```
Config.Applied = 80 (server reads snapshot boundary)
appliedIndex = 80
Ready().CommittedEntries = entriesBetween(80, 100) = [81..100]  ← replay from boundary
```

Entries 81–95 are re-applied (idempotent, safe). Entries 96–100 are applied for the first time. Gap closed.

## Minimum tests

**Invariant:** after a crash, restarting a node replays all committed entries above the compaction boundary into the state machine, closing any gap left by a partial apply.

1. **Config.Applied replays unapplied entries** — persist entries 1–10 with `commitIndex = 10` to DurableStorage. Construct a Raft with `Config.Applied = 0` and the same storage. `Ready().CommittedEntries` contains entries 1–10. Proves entries above Applied are re-emitted.
2. **Config.Applied skips already-applied entries** — persist entries 1–10 with `commitIndex = 10`. Construct with `Config.Applied = 7`. `Ready().CommittedEntries` contains entries 8–10 only. Proves Applied acts as a lower bound.
3. **Config.Applied > commitIndex panics** — persist entries 1–10 with `commitIndex = 10`. Construct with `Config.Applied = 15`. `newRaft()` panics. Proves the contract is enforced: Applied cannot exceed what raft has committed.
4. **Config.Applied below first log entry panics** — compact entries 1–10. Log starts at 11. Construct with `Config.Applied = 5`. `newRaft()` panics. Proves the contract is enforced: Applied cannot claim a position inside the compacted range (raft can't replay entries 6–10).
5. **Fresh start with zero Applied** — create a Raft with empty storage and `Config.Applied = 0`. `appliedIndex` is 0, `commitIndex` is 0, `Ready().CommittedEntries` is empty. No regression.
6. **Replay after compaction starts from log, not snapshot** — persist entries 1–20 with `commitIndex = 20`, compact at 10. Construct with `Config.Applied = 10`. `Ready().CommittedEntries` contains entries 11–20. Construct again with `Config.Applied = 0`. `Ready().CommittedEntries` still contains entries 11–20 — entries below the compaction boundary are gone from the log regardless of Applied.
7. **Server crash replays into engine.DB** — covered by the existing 037e integration test (`TestPutBeforeShutdownIsReadableAfterRestart_037e`). Three-node cluster, PUT, shutdown node 3, restart with same data dir, GET returns the correct value. This exercises the full `NewRaftHost` → `Config.Applied` → replay → engine.DB chain.
8. **Crash mid-apply with compaction recovers missing entries** — persist 100 entries with `commitIndex = 100` in DurableStorage (compact threshold = 80). Construct Raft with `Applied = 0`, get all 100 entries. Apply 1–95. Compact at 80. Close (crash). Reopen storage, read snapshot boundary (80). Construct Raft with `Applied = 80`. `Ready().CommittedEntries` returns entries 81–100. Apply all. Assert the fake state machine has all 100 entries.

## Open threads

1. **Persisted appliedIndex** — the replay window is bounded by `compactThreshold` (10,000 entries). Persisting `appliedIndex` per-batch would shrink it to just the unapplied tail, at the cost of one fsync per apply batch. Deferred until benchmarks show replay latency matters.
2. **Non-idempotent commands** — replay re-applies entries the engine already has. Safe for PUT/Delete (last-write-wins). Redis-style commands like INCR or APPEND would break under replay. etcd, TiDB, and CockroachDB avoid this by keeping the KV layer strictly idempotent — non-idempotent logic (e.g., SQL `balance + 100`) is computed above the KV interface and resolved to a plain Put before entering the log. If kv-go follows the same pattern toward its SQL-on-KV north star, the KV layer never needs non-idempotent commands. Noted as a constraint, not a gap.
3. **Slow follower snapshot** — compaction deletes WAL segments below the snapshot boundary. A follower behind the boundary still needs full snapshot transfer (open from 037f).
