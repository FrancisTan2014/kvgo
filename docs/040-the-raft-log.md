# 040 — The Raft Log

039 fixed single-node commit via self-ack. `kv-bench -c 1` works — 1000/1000, 144 req/s. But concurrent connections fail immediately.

```
Benchmark: 5000 requests, 5 connections, 128 byte values, 0% GETs
Target: 127.0.0.1:4100 (tcp)

No successful requests.
Errors: 5000
```

With 2 connections and 4 requests, 1 succeeds and 3 time out:

```
Successful:     1
Errors:         3
Duration:       5.01s
```

Server log:

```
level=ERROR msg="failed to process the request" cmd=2 error="context deadline exceeded"
level=ERROR msg="failed to process the request" cmd=2 error="context deadline exceeded"
```

Debug logging on the node loop reveals the sequence:

```
propose (advancec_nil=true)     → entry A at index 2
ready built (entries=1)         → A in Entries
ready sent                      → self-ack A collected
propose (advancec_nil=false)    → entry B at index 3 (arrives between Ready and Advance)
advance (stepsOnAdvance=1)      → steps self-ack A, commits A
ready built (entries=0, committed=1)  → A in CommittedEntries, but B missing from Entries
advance (stepsOnAdvance=1)      → steps self-ack B
matchTerm: storage.Term failed  → B never persisted, commit fails silently
```

Entry B was appended to the in-memory log but never appeared in any Ready's `Entries`. It was never persisted, yet `Advance()` set `stableIndex = lastLogIndex` — claiming B was stable.

## Reasoning

### Two bugs from one root cause

The node loop's `select` processes `propc` between Ready-send and Advance. Entry B arrives via `propc`, gets appended to `r.log`, and `appendEntry` queues a self-ack in `msgsAfterAppend`. Two things go wrong:

**Self-ack loss.** The self-ack collection happens at Ready-send time. B's self-ack was added after that collection. `Advance()` clears `msgsAfterAppend` with `[:0]`, destroying B's self-ack. Even if B later appears in a Ready, no self-ack exists to trigger its commit.

**Phantom stability.** `Advance()` sets `stableIndex = r.lastLogIndex`. But `lastLogIndex` now includes B, which was never in a Ready and never persisted. The next `Ready()` calls `entriesBetween(stableIndex, lastLogIndex)` — which returns nothing, because `stableIndex == lastLogIndex`. B is invisible: not in `Entries`, not in `CommittedEntries`, never persisted, yet the system thinks it's stable.

Both bugs share the same root cause: the raft state machine has a single flat log (`r.log`) and a single stability marker (`stableIndex`) that conflates "appended to memory" with "persisted to disk." `Advance()` assumes everything in the log is stable, because before self-ack there was no way for entries to exist in the log without having been in a Ready.

### Naive fix: block proposals

The simplest patch: set `propc = nil` when `advancec != nil`. No proposals can arrive between Ready and Advance. Both bugs vanish — every entry that exists at `Advance()` time was in the Ready and was persisted.

This works. Benchmark confirms:

```
Benchmark: 10000 requests, 50 connections, 128 byte values, 0% GETs
Successful:     10000
Errors:         0
Throughput:     173 req/s
P50:            291ms
```

But throughput doesn't scale with connections. All 50 workers serialize through the Ready/Advance gate. With 1 connection: 151 req/s, p50 6.5ms. With 50 connections: 173 req/s, p50 291ms. The pipeline is correct but narrow — proposals queue behind persistence.

### How etcd solves it

etcd never blocks proposals. Its `node.run()` loop accepts proposals freely between Ready and Advance. The key: etcd's log is not a flat slice. It's split into two layers.

`raftLog` owns a `Storage` (persisted entries) and an `unstable` struct (in-memory entries not yet persisted). Every new entry goes to `unstable`. `Ready()` exposes `unstable.entries` as `rd.Entries` — the caller persists them. `acceptReady` marks them as "in progress." `Advance()` calls `stableTo(index, term)` which moves entries from `unstable` to `Storage` up to the specified point. Entries that arrived after Ready was built stay in `unstable` and appear in the *next* Ready.

The `(index, term)` pair in `stableTo` prevents a stale response from marking wrong entries as stable — the ABA problem where a leader change overwrites the unstable tail between Ready and Advance.

`msgsAfterAppend` is handled similarly: `acceptReady` nils the slice. Late proposals write to a fresh slice. `Advance()` doesn't touch it. Late self-acks survive and are collected in the next `acceptReady`.

Two design decisions make this work:

1. **`stableTo(index, term)` instead of `stableIndex = lastLogIndex`.** Targeted promotion — only entries that were actually in the Ready and actually persisted graduate from unstable to stable. Late entries stay unstable. The term acts as a generation guard: between Ready-send and Advance, a leader change can overwrite the unstable tail with new entries at the same indices but different terms. A stale `stableTo` with the old term is a no-op — the term doesn't match, so nothing graduates.
2. **`term()` reads from both layers.** `matchTerm(i)` checks `unstable` first, then falls back to `Storage`. A committed entry can be verified against the in-memory log before it's persisted. Our `matchTerm` only checks `Storage` — exactly the bug in the trace: self-ack arrives for an entry still in `unstable`, `storage.Term(i)` returns `ErrUnavailable`, commit fails silently.

### Why `raftLog`, not `unstable` on Raft

The two-stage split could be done by adding `unstable` as a field on `Raft` directly — no new type needed. But the split creates an invariant: `applied ≤ committed ≤ lastIndex`, where `lastIndex` spans both storage and unstable. If `unstable`, `committed`, and `applied` are all fields on `Raft`, every method that touches the log must maintain that invariant: `appendEntry`, `maybeCommit`, `sendAppend`, `handleAppendEntries`, `Ready`, `Advance`. The invariant is scattered.

`raftLog` gives the invariant one owner. `commitTo` panics if you violate it. `appliedTo` can only advance forward. `appendEntries` guards against overwriting committed entries. The Raft struct becomes a consumer of the log, not a co-owner. This is the same reason etcd extracts it — `raftLog` is the seam between "what the state machine decided" and "what the caller persisted," and the Ready/Advance contract crosses that seam.

### Node loop: proposals stay open

With `raftLog`, the node loop doesn't need to block proposals during Ready/Advance — the naive fix from above becomes unnecessary. Proposals arriving between Ready and Advance append to `unstable`. Their self-acks go to a fresh `msgsAfterAppend` (nil'd at Ready-send). `Advance()` calls `stableTo` for only the persisted entries. Late entries stay unstable, appear in the next Ready, and their self-acks are collected in the next `acceptReady`.

`msgsAfterAppend` management changes:

- Ready-send: collect self-acks into `stepsOnAdvance`, nil `msgsAfterAppend`
- `Advance()`: does not touch `msgsAfterAppend`
- Late proposals: write to fresh slice, survive to next cycle

## Shape

Two new types: `raftLog` and `unstable`. `raftLog` replaces all log-related fields and methods on the Raft struct. The node loop removes proposal blocking and adopts etcd's `acceptReady`-style `msgsAfterAppend` management.

### What raftLog replaces

The flat `r.log` slice currently handles everything: in-memory appends, conflict detection, entry slicing, stability tracking. These concerns are spread across the Raft struct:

- `r.log []*raftpb.Entry` → `raftLog *raftLog`
- `r.lastLogIndex` → `raftLog.lastIndex()`
- `r.stableIndex` → owned by `raftLog.unstable` via `stableTo`
- `r.commitIndex` → `raftLog.committed`
- `r.appliedIndex` → `raftLog.applied`
- `entriesBetween(lo, hi)` → `raftLog.nextUnstableEnts()` + `raftLog.nextCommittedEnts()`
- `appendEntries(entries)` → `raftLog.appendEntries()`
- `findConflict(entries)` → removed. Conflict detection will move to `raftLog` when `maybeAppend` is built (open thread #5).
- `matchTerm(i)` → `raftLog.matchTerm()` (checks unstable first, then storage)
- `lastLog()` → `raftLog.lastIndex()`
- `anchorExists(index, term)` → `raftLog.matchTerm()`

### `unstable`

```go
type unstable struct {
    entries []*raftpb.Entry
    offset  uint64
}

func (u *unstable) stableTo(index, term uint64) {
    if index < u.offset { return }
    i := index - u.offset
    if int(i) >= len(u.entries) { return }
    if u.entries[i].Term != term { return }
    u.entries = u.entries[i+1:]
    u.offset = index + 1
}

func (u *unstable) truncateAndAppend(entries []*raftpb.Entry) {
    first := entries[0].Index
    if first <= u.offset {
        u.offset = first
        u.entries = entries
    } else if first == u.offset+uint64(len(u.entries)) {
        u.entries = append(u.entries, entries...)
    } else {
        u.entries = u.entries[:first-u.offset]
        u.entries = append(u.entries, entries...)
    }
}

func (u *unstable) maybeLastIndex() (uint64, bool) {
    if len(u.entries) == 0 { return 0, false }
    return u.entries[len(u.entries)-1].Index, true
}

func (u *unstable) maybeTerm(index uint64) (uint64, bool) {
    if index < u.offset || index >= u.offset+uint64(len(u.entries)) {
        return 0, false
    }
    return u.entries[index-u.offset].Term, true
}
```

### `raftLog`

```go
type raftLog struct {
    storage   Storage
    unstable  unstable
    committed uint64
    applied   uint64
}
```

`lastIndex()`, `term()`, `matchTerm()`, `appendEntries()`, `stableTo()`, `appliedTo()`, `commitTo()` delegate to `unstable` first (via `maybeLastIndex`, `maybeTerm`), then `storage`. All one-liners except:

- `nextCommittedEnts()` — delegates to `slice(applied+1, committed+1)`, the general cross-layer read.
- `slice(lo, hi)` — reads from storage up to `unstable.offset`, then from unstable for the remainder. Handles the boundary correctly when committed entries span both layers.
- `term(0)` — returns `(0, nil)` as a sentinel for the empty prev-log anchor. etcd achieves the same via a dummy entry at index 0 in `MemoryStorage`; our `DurableStorage` has no dummy, so the sentinel is explicit. After compaction, etcd's `term(0)` returns `ErrCompacted`; ours always returns `(0, nil)` — safe for now, may diverge when compaction produces its own sabotage.
- `commitTo(i)` — panics if `i > lastIndex()` (same as etcd). Never decreases committed.
- `appendEntries(ents)` — panics if `ents[0].Index - 1 < committed` (same as etcd). Guards against overwriting committed entries.
- `restore(snapIndex)` — resets unstable (`offset = snapIndex+1`, entries cleared) and advances `committed` and `applied` to the snapshot boundary. etcd's `restore` only sets `committed`; `applied` is advanced later when the snapshot is actually applied via `MsgStorageApplyResp`. Our eager approach is simpler but conflates "snapshot received" with "snapshot applied."
- `lastEntryID()` — returns `(lastIndex, term(lastIndex))`. Used for vote comparison.
- `isUpToDate(index, term)` — log comparison for elections.

### Raft struct changes

Remove `log`, `lastLogIndex`, `stableIndex`, `commitIndex`, `appliedIndex` from the Raft struct. Replace with `raftLog *raftLog`.

`Advance(rd Ready)` calls `raftLog.stableTo` with the last entry from `rd.Entries` and `raftLog.appliedTo` with the last from `rd.CommittedEntries`. `CommittedEntries` in a Ready means "applied" — the caller has processed them. `committed` was already set when the entries were committed; `Advance` advances `applied`.

`Ready()` uses `raftLog.nextUnstableEnts()` for `Entries` and `raftLog.nextCommittedEnts()` for `CommittedEntries`.

`matchTerm(i)` becomes `raftLog.matchTerm(i, term)` — checks unstable first, then storage.

### Follower append path

`handleAppendEntries` must append entries *before* advancing the commit index. The leader's `MsgApp` carries both entries and a commit index — but the follower may not have those entries locally until after appending. `commitTo` panics if the commit index exceeds `lastIndex()`, so the order matters. Additionally, the commit index is clamped to `min(m.Commit, lastIndex())` — the leader may be ahead of what the follower received in this batch.

### Node loop changes

Ready-send nils `msgsAfterAppend` (not `[:0]`). `Advance()` no longer touches `msgsAfterAppend`. `propc` stays enabled — no toggling needed.

## Minimum tests

**Invariant:** the raft log separates stable and unstable entries so that proposals arriving between Ready and Advance are persisted in a subsequent cycle without loss, and `stableTo` promotes only entries that were actually persisted.

1. **Late proposal survives Advance** — a proposal arriving between Ready-send and Advance appears in the next Ready's `Entries`.
2. **Late self-ack survives Advance** — a self-ack from a late proposal is collected in the next Ready cycle and commits after persistence.
3. **stableTo with matching term** — `stableTo(index, term)` removes entries up to `index` from unstable.
4. **stableTo with mismatched term** — `stableTo(index, wrongTerm)` is a no-op; entries remain unstable.
5. **Concurrent proposals all commit** — 5 proposals submitted across multiple Ready/Advance cycles all reach CommittedEntries, proving that no entry is lost or stuck in unstable.
6. **term() checks unstable first** — `raftLog.term(i)` returns the term from unstable for an entry not yet persisted.
7. **matchTerm succeeds for unstable entries** — `raftLog.matchTerm(i, t)` returns true for an entry in unstable with the correct term.
8. **nextUnstableEnts excludes stable** — after `stableTo`, entries that graduated don't appear in subsequent `nextUnstableEnts`.
9. **lastIndex spans both layers** — `raftLog.lastIndex()` returns the unstable tip when unstable has entries, storage tip otherwise.
10. **append with conflict truncates** — `raftLog.append` with overlapping entries at a different term truncates the unstable tail.
11. **Advance takes Ready** — `Advance(rd)` calls `stableTo` with the last entry from `rd.Entries`, not `lastLogIndex`.

## Open threads

Threads 1–3 are cuts from Reasoning. Threads 4–7 surfaced from reading etcd's `raftLog` implementation.

1. **Snapshot interaction** — `unstable` may also hold a pending snapshot (etcd's `unstable.snapshot`). Deferred until the snapshot episode.
2. **In-progress tracking** — etcd's `acceptInProgress` marks entries as handed out to avoid re-sending them in the next Ready. Without it, `nextUnstableEnts` re-delivers entries every cycle until `stableTo` clears them. Deferred until it causes a concrete problem.
3. **`msgsAfterAppend` test helpers** — resolved. `advanceAndStepSelfMsgs` and `drainReady` now persist entries to mock storage before calling `Advance(rd)`, and `Advance` no longer clears `msgsAfterAppend`.
4. **`applying` pointer** — etcd tracks three pointers: `applied ≤ applying ≤ committed`. `applying` marks entries handed to the state machine but not yet confirmed. Without it, `nextCommittedEnts` re-delivers the same entries if a new Ready cycle fires before `Advance()` updates `applied`.
5. **`findConflict` across both layers** — `Raft.findConflict` and the old `Raft.appendEntries` were removed during the migration. The follower path (`handleAppendEntries`) now delegates to `raftLog.appendEntries`, which calls `unstable.truncateAndAppend` — a dumb buffer that doesn't scan for conflicts. etcd's `findConflict` operates on the full `raftLog` via `matchTerm`, detecting conflicts across persisted and unstable entries. Needed for correct `maybeAppend` on the follower path.
6. **`slice` with size limits** — etcd's general-purpose cross-layer read with `maxSize` back-pressure. Our `nextCommittedEnts` is a special case without size limits. Will matter when entries are large.
7. **`allowUnstable` flag** — controls whether committed-but-unpersisted entries can be applied. Affects crash safety: applying before local persistence means a crash loses an applied entry.
