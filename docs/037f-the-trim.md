# 037f — The Trim

Run a three-node cluster. PUT one million keys. Every entry stays in `DurableStorage.entries` forever and the `replication.log` file grows without bound. Compaction exists but nobody calls it. The Raft log is a memory leak.

## Boundary

Wire Raft log compaction into the server's apply path. After entries are applied to the state machine, periodically compact the Raft log to reclaim both memory and disk.

In scope:
- Buffered apply channel — widen `applyc` from unbuffered to capacity 1. Currently `handleBatch()` blocks on an unbuffered channel send; a slow state machine stalls ticks, heartbeats, and message processing. A buffer of 1 absorbs one slow apply without stalling the raft loop; if apply falls two batches behind, the raft loop blocks — correct backpressure.
- Compaction call site — after applying a batch, `server.run()` calls `DurableStorage.MaybeCompact(appliedIndex)`. The caller doesn't decide *when* to compact; it just reports progress.
- Threshold policy inside storage — `MaybeCompact` checks `appliedIndex - snapshotIndex > threshold` internally. If the gap is too small, it returns immediately. Default threshold: 10,000 entries (matching etcd's `SnapshotCount`), configurable.
- WAL segments — replace the single-file `replication.log` with numbered segment files. `Save()` rotates to a new segment when the current one exceeds a size limit. Compaction deletes segments whose entries are entirely below the snapshot boundary. No blocking rewrite, no temp-file ceremony.

Deferred:
- Snapshot payload — compaction persists `SnapshotMeta` (index + term boundary) but not a state machine snapshot. Full snapshot transfer to slow followers (036k) remains unwired.
- Dedicated apply goroutine — etcd runs apply in a separate goroutine with a FIFO scheduler, fully decoupled from the raft loop. We use a buffered channel (capacity 1) which partially decouples them; a dedicated goroutine inside `raftHost` with callback registration is deferred.

Out of scope:
- Compaction of the engine's own WAL (already handled by `engine/compaction.go`)
- Linearizable reads, membership changes

## Design

### What etcd does

etcd's compaction is snapshot-driven. When `appliedIndex - lastSnapshotIndex > 10,000`, etcd takes a state machine snapshot, persists it to disk, then calls `raftStorage.Compact(appliedIndex - 5,000)`. The 5,000-entry overlap (`SnapshotCatchUpEntries`) lets slow followers catch up from the log without needing a full snapshot transfer.

etcd reclaims disk through segmented WAL files (64MB each). After a snapshot, it releases segments whose entries are entirely below the snapshot boundary. Each entry is fully contained in one segment — the size check happens *after* writing, so segments can exceed 64MB but never split an entry.

### What we do

Our compaction follows etcd’s pattern but without state machine snapshots:

1. **Trigger.** After each `applyBatch`, call `DurableStorage.MaybeCompact(appliedIndex)`. The storage decides internally whether the gap warrants compaction.
2. **Compact.** If `appliedIndex - snap.LastIncludedIndex > threshold`, persist the new `SnapshotMeta` boundary and trim in-memory entries.
3. **Delete old segments.** After compaction, delete WAL segment files whose entries are entirely below the new snapshot boundary.

`MaybeCompact` is the only public compaction entry point. There is no public `Compact` method — compaction is always threshold-gated. This matches etcd, where `Compact` lives on the concrete `MemoryStorage` type, not on the `raft.Storage` interface.

### Segmented WAL

The current single-file WAL (`replication.log`) is replaced by numbered segment files: `wal-NNNNNNNN.log` (zero-padded sequence number). The segment naming is simpler than etcd’s `seq-index.wal` — we don’t encode the first index in the filename because we derive that from the entries inside.

**Write path.** `Save()` writes to the current (last) segment. After writing, if the segment exceeds the size limit (default 64MB), it calls `cut()` to close the current segment and open a new one. `cut()` is a separate method following etcd's pattern — `Save()` owns encoding and persistence, `cut()` owns rotation. Entries never span segments — the size check happens *after* writing, so a segment can exceed the limit but every entry is self-contained.

**Replay path.** `replayLog()` reads all segment files in sorted order, decoding batches from each sequentially. When one segment reaches EOF, it moves to the next. The existing `truncateAndAppend()` handles overlapping entries across segments.

**Compaction path.** Compaction deletes segment files whose *last* entry index is ≤ the snapshot boundary. The segment containing the boundary itself is kept — it may have entries both before and after the boundary. `filterRetainedEntries()` already handles this during replay.

### Buffered apply channel

The raft goroutine (`raftHost.start()`) currently blocks on an unbuffered channel send when handing committed entries to the server. A slow apply (engine write, compaction) stalls the entire raft loop — no ticks fire, heartbeats stop, elections trigger.

We widen `applyc` from unbuffered to buffered (capacity 1). The raft loop sends committed entries on the channel and calls `Advance()` after persistence completes. A buffer of 1 absorbs one slow apply without stalling the raft loop; if apply falls two batches behind, the raft loop blocks — correct backpressure.

Calling `Advance()` before apply finishes is safe: `Advance()` tells raft "I've consumed this Ready, give me the next one." Raft only needs entries *persisted* (crash-safe) before advancing — it doesn't care whether the state machine has applied them. etcd works the same way. `server.run()` drains the channel, applies entries, and calls `MaybeCompact(appliedIndex)` after each batch. The sequence is:

```
Raft goroutine:  Save(entries, hardState) → Send(messages) → applyc <- CommittedEntries → Advance()
server.run():    <- applyc → applyBatch(entries) → MaybeCompact(appliedIndex)
```

`MaybeCompact` is called from `server.run()` while `Save()` is called from the raft goroutine. Both touch `DurableStorage`, which is already protected by `s.mu` — no new synchronization needed.

### WAL segment safety

Segment deletion is crash-safe by ordering: `SnapshotMeta` is persisted *before* any segment is deleted. If a crash happens between persisting the boundary and deleting old segments, replay skips entries below the boundary via `filterRetainedEntries()` — the old segments are harmless dead weight until the next compaction deletes them.

## Minimum tests

**Invariant:** applied entries are periodically removed from the Raft log and WAL, reclaiming memory and disk, without losing data that the state machine hasn't yet absorbed.

1. **Compaction trims in-memory entries and returns ErrCompacted** — compact at index 5 on a storage with entries [1..10]. `FirstIndex()` returns 6, `Entries(1,6)` returns `ErrCompacted`, `Entries(6,11)` returns entries 6–10. (036e invariant preserved with segmented WAL.)
2. **Compaction boundary persists across restart** — compact at index 5, close storage, reopen. `FirstIndex()` returns 6 and `Snapshot()` returns the boundary. (036e invariant preserved with segmented WAL.)
3. **Segment deletion reclaims disk** — save entries across multiple segments (use a small segment size), compact past the first segment's entries, verify the old segment file is deleted. Reopen storage, verify retained entries are correct.
4. **Segment deletion is crash-safe** — save entries across segments, persist snapshot boundary without deleting segments (simulating crash). Reopen storage — replay skips compacted entries, retained entries are correct.
5. **MaybeCompact skips when below threshold** — call `MaybeCompact(50)` with threshold 100. `FirstIndex()` stays at 1 (no compaction). Call `MaybeCompact(150)`. `FirstIndex()` advances (compaction ran).
6. **Compaction preserves entries above the boundary** — storage has entries [1..100]. Compact at 80. Entries [81..100] are retained, entries [1..80] are trimmed.
7. **Segment rotation on size limit** — write entries until the segment exceeds the size limit. Verify a new segment file was created and subsequent writes go to the new segment.
8. **Replay stitches multiple segments** — write entries across 3 segments, close, reopen. All entries are recovered in order.

## Open threads

1. **Slow follower overlap** — etcd keeps 5,000 entries after the snapshot boundary so slow followers can catch up from the log. We compact everything up to `appliedIndex`, which means a slow follower that falls behind the boundary needs a full snapshot transfer. This is acceptable while snapshot transfer (036k) isn't wired.
2. **Compaction during snapshot inflight** — etcd skips compaction if snapshot transfer is in progress to a slow follower (the leader still needs those entries). We don’t track inflight snapshots yet.
