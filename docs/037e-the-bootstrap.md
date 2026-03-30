# 037e — The Bootstrap

Kill a follower. Restart it. `newRaft()` loads HardState but not log entries — the node comes up with the right term but an empty log. It looks brand new to the cluster.

## Boundary

A restarted node recovers its full Raft state — HardState, log entries, and snapshot metadata — and rejoins the cluster as a follower without disrupting the existing leader.

In scope:
- Log recovery in `newRaft()` — load entries from storage, initialize `lastLogIndex`, `stableIndex`, and `appliedIndex` from the recovered state
- Fresh vs restart distinction — if storage has entries, load them into `r.log` instead of starting empty
- Integration test — three-node cluster, PUT values, kill a follower, restart it, verify it catches up and serves correct GET responses
- `Advance()` correctness on restart — `stableIndex` must reflect what's already persisted, not zero

Deferred:
- Snapshot-based recovery (leader sends snapshot to a far-behind follower) — requires snapshot transfer (036k exists but isn't wired)
- Engine state recovery from Raft log — replaying committed entries into `engine.DB` on restart (the engine has its own WAL; this is the compaction/snapshot integration problem)

Out of scope:
- Dynamic membership changes
- `--force-new-cluster` disaster recovery

## Design

### What etcd does

etcd splits startup into `raft.StartNode()` and `raft.RestartNode()`. The decision is simple: does the WAL directory exist?

`StartNode` takes a peer list, creates a `ConfChange` entry for each peer, and appends them to an empty log. This is the genesis — the cluster's first entries are its own membership.

`RestartNode` takes no peers. It loads HardState and log entries from storage, then enters follower state. Peer membership is already encoded in the log (via ConfChange entries) or reconstructed from the backend store.

The bootstrap layer in etcd is ~850 lines because it handles dynamic membership, discovery services, learner nodes, WAL repair, and force-recovery. We need exactly one thing from it: the two-path fork.

### What we need

Our fork is simpler — no ConfChange, no dynamic membership. The decision lives entirely inside `newRaft()`:

```
storage.LastIndex() > 0  →  load entries, set lastLogIndex and stableIndex
storage.LastIndex() == 0 →  start with empty log (current behavior)
```

Peers always come from `cfg.Peers` (the `--peers` flag) in both paths — we don't store membership in the log yet. No change to the Node or RaftHost layers.

On restart, `stableIndex` must equal `lastLogIndex` — everything on disk is already stable. `appliedIndex` must equal `commitIndex` from HardState — the apply loop ran before the previous shutdown. If it didn't (crash mid-apply), some committed entries may re-apply. That's safe: PUT is idempotent at the KV layer.

### Overlapping entries on append

A restarted follower that has fallen behind is exactly the node that receives overlapping `MsgApp` from the leader doing log repair. `appendEntries()` previously appended blindly — a leader sending entries [55–65] to a follower with [51–60] produced duplicates. The fix: scan incoming entries against the existing log, skip matching entries (same index and term), truncate on conflict (different term at same index), and append only the new tail. This matches etcd's `findConflict` + `truncateAndAppend` pattern.

When truncation discards entries, `stableIndex` must be reset to the truncation point — otherwise the replacement entries won't appear in `Ready().Entries` for persistence.

### Storage truncate-and-replace

The WAL is append-only: overlapping replacement entries are written as a new batch. `DurableStorage.Save()` and `replayLog()` use `truncateAndAppend()` to reconcile overlapping entries in memory — truncate at the overlap point, then append the new tail. Gaps are still rejected.

## Minimum tests

**Invariant:** a restarted node recovers its Raft log from storage and rejoins the cluster without data loss or election disruption.

1. **Restarted Raft loads log entries from storage** — create a Raft with storage containing persisted entries. `newRaft()` initializes `r.log`, `r.lastLogIndex`, and `r.stableIndex` from storage. The Raft's in-memory state matches what was persisted.
2. **Restarted Raft does not re-emit stable entries** — after recovery, `Ready().Entries` is empty (stableIndex == lastLogIndex). The node doesn't ask the storage layer to re-persist what it already has.
3. **Restarted Raft preserves vote and term** — a node that voted in term 5 and restarts still has `votedFor` and `term` from HardState. It does not vote again in the same term. (Existing test, verify it still holds after log recovery changes.)
4. **Restarted follower receives new entries from leader** — a three-node Raft cluster. Follower recovers with entries 1–5. Leader sends MsgApp with entries 6–7. Follower appends them. Proves the recovered log is compatible with the replication protocol.
5. **Restarted node does not disrupt existing leader** — a leader is established (term 3). A follower restarts with term 3 from HardState. It enters follower state, does not campaign, and does not increment term. The leader's heartbeats suppress its election timer.
6. **Server restart: PUT before shutdown is readable after restart** — integration test. Three-node server cluster. PUT key=x. Shut down one follower. Restart it with the same data directory. GET key=x returns the value. Proves end-to-end recovery.
7. **Empty storage starts fresh** — a node with empty storage uses `cfg.Peers` and starts with an empty log. Existing behavior is preserved.
8. **Overlapping append skips duplicates** — append entries [2,3] (term 1) to a log that already has [1,2,3] (term 1). Log stays [1,2,3].
9. **Overlapping append truncates on conflict** — append entries [2,3,4] (term 2) to a log with [1,2,3] (term 1). Entry 2 conflicts, log becomes [1(t1),2(t2),3(t2),4(t2)].
10. **Overlapping append extends** — append entries [2,3] (term 1) to a log with [1,2] (term 1). Entry 2 matches, entry 3 extends. Log becomes [1,2,3].
11. **Storage truncation survives restart** — save entries [1,2,3] (term 1), then save overlapping [2,3,4] (term 2). Close and reopen storage. Replay produces [1(t1),2(t2),3(t2),4(t2)].
12. **Conflicting append emits replacement entries in Ready** — append [1,2,3] (term 1), advance stableIndex to 3, then conflicting append [2,3,4] (term 2). `stableIndex` resets to 1 and `Ready().Entries` contains the three replacement entries for persistence.
13. **Restart with snapshot and entries loads correctly** — storage has a snapshot at index 10 and entries [11,12,13]. `newRaft()` loads only the entries, with `lastLogIndex = 13` and `stableIndex = 13`.

## Open threads

1. **Committed-but-unapplied replay** — if the process crashes after Raft commits but before the engine applies, those entries are lost from the KV state. On restart, the apply loop must detect the gap (Raft `commitIndex` > engine's applied state) and replay.
2. **Snapshot recovery** — a node that's been down long enough will have its log entries compacted away on the leader. The leader must send a snapshot. This requires wiring 036k's snapshot transfer into the restart path.
3. **`appliedIndex` persistence** — we set `appliedIndex = commitIndex` on recovery, assuming all committed entries were applied before shutdown. A crash violates this assumption. Persisting `appliedIndex` separately (or deriving it from engine state) is the correct fix.
4. **Engine state consistency** — on restart, the engine replays its own WAL independently. The two WALs stay consistent as long as the process didn't crash between Raft commit and engine write. Crash-gap replay (thread 1) is the fix.

