# 036e - The Compaction

The Raft story continues. 036e deals with the unbounded replication log. We have seen compaction before in [006-the-log-compaction.md](006-the-log-compaction.md), but the invariant is different here.

036e protects: **compaction must preserve the meaning of the log.**

In 036c, we defined a `Storage` interface with `Entries(lo, hi)`. That is now part of the contract. 036e may remove old entries from the front of the log, but it must not break the semantics of `Entries`. A caller asking for removed entries should be told explicitly that those entries were compacted, not be given a vague “not found”. A caller asking for retained entries should still get the same answers as before.

This requires a boundary that represents the removed history. In full Raft that boundary is snapshot metadata. For 036e, instead of carrying two loose fields everywhere, I can give that boundary a name:

```go
type SnapshotMeta struct {
    LastIncludedIndex uint64
    LastIncludedTerm  uint64
}
```

That keeps the scope small while making the concept explicit. 036e still stops at metadata only. A fuller snapshot with payload can come later.

While implementing 036e, I also found that `FirstIndex` and `LastIndex` are better expressed as derived methods instead of stored fields. Once compaction introduces `SnapshotMeta`, the retained log range should be computed from the boundary plus the remaining entries, not maintained as another pair of independently mutable values.

A bounded design is:
```go
type Storage interface {
    ...
    FirstIndex() uint64
    LastIndex() uint64
    Compact(index uint64) error
} 

func (s *StorageImpl) Compact(index uint64) error {
    s.log.RemoveThrough(index)
    s.snapshot = SnapshotMeta{
        LastIncludedIndex: index,
        LastIncludedTerm:  termAt(index),
    }
    s.persistBoundary(s.snapshot)
}

func (s *StorageImpl) Entries(lo, hi uint64) ([]Entry, error) {
    if lo <= s.snapshot.LastIncludedIndex {
        return nil, ErrCompacted
    }
    ...
}

func (s *StorageImpl) FirstIndex() uint64 {
    return s.snapshot.LastIncludedIndex + 1
}

func (s *StorageImpl) LastIndex() uint64 {
    if len(s.entries) == 0 {
        return s.snapshot.LastIncludedIndex
    }
    return s.entries[len(s.entries)-1].Index
}
```

Compaction must also survive restart. The durable fact is not “compaction happened” as a separate operation. The durable fact is the boundary itself: the log up through a given index and term has already been captured and may no longer be served as ordinary entries. `SnapshotMeta` is already the minimum form of snapshot metadata, even if 036e does not yet carry a full snapshot payload.

One more validity rule appears here: after replay, the retained entries must be contiguous with the snapshot boundary. If the first retained entry does not begin at `LastIncludedIndex + 1`, local storage is semantically corrupt and startup should fail instead of trying to continue.

## Minimum test

#1 `TestEntriesBeforeBoundaryReturnErrCompacted` proves `Entries` keeps its meaning after compaction.

#2 `TestCompactionSurvivesRestart` proves the compacted boundary remains durable and retained entries are still readable after restart.

#3 `TestReplayFailsOnGapAfterSnapshotBoundary` proves a retained-log gap after the snapshot boundary is fatal corruption, not a recoverable tail issue.

## Bounded scope

036e is complete when:
- old entries before the boundary are no longer available through `Entries`
- requests before the boundary return `ErrCompacted`
- retained entries after the boundary are still readable
- a retained-log gap after the snapshot boundary fails startup as corruption
- the boundary survives restart
- tests pass
