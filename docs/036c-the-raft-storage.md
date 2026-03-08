# 036c — The Raft Storage

We built the node loop — which serves as a bridge between the pure Raft state machine and the outside world — in 036b. Same as the intention of 036b: without a loop, a pure state machine is worth nothing. Here, without a storage layer, a pure in-memory state machine cannot survive a crash — violating the safety invariant of Raft. (Which one? I see every one matching. The sharpest answer: §5.4.1 Election Safety and §5.2 Log Matching. Without persisted `Term` and `VotedFor`, a restarted node can vote twice in the same term. Without persisted log, a committed entry can vanish.)

Thus, 036c intends to protect the invariant: **persistent state survives restart**.

## Interface
We have `HardState` (borrowed from `etcd/raft`) and unstable entries that need to be persisted when consuming `Ready`, and reloaded on restart. The interface can be:
```go
type Storage interface {
    InitialState() (HardState, []Entry, error)
    SaveEntries(e []Entry) error
    SaveHardState(h HardState) error
}
```

But intuitively, two separate `Save` methods introduce a gap in the case where both `HardState` and `Ready.Entries` must be persisted. Let me read how `etcd/raft` does the design.
```go
type Storage interface {
	InitialState() (pb.HardState, pb.ConfState, error)
	Entries(lo, hi, maxSize uint64) ([]pb.Entry, error)
	Term(i uint64) (uint64, error)
	LastIndex() (uint64, error)
	FirstIndex() (uint64, error)
	Snapshot() (pb.Snapshot, error)
}
```

Oh God, what a surprise! There are no `Save` methods in it. Let me track the **save** path.
`etcd` uses a `MemoryStorage` as the implementation of `Storage`. It's read-only, because `etcd/raft` is a standalone package implementing the pure Raft algorithm for reuse, and the application that saves entries is responsible for durability.

That makes sense, but we don't need to follow the architecture of `etcd`. Since we own both sides, any implementation of our `Storage` interface owns the responsibility for durability. Let me keep my intuition — one `Save` method for simplicity. As we own the durability responsibility, the constructor replays the WAL and loads state — no separate `Start` needed. `InitialState()` is just a getter for `HardState`. During implementation, I found that returning all entries from `InitialState` doesn't scale — the caller needs range queries, not the full log dump. So `Entries(lo, hi)` is a better fit. Now the interface becomes:
```go
type Storage interface {
	InitialState() (HardState, error)
	Save(entries []Entry, hard HardState) error
    Entries(lo, hi uint64) ([]Entry, error)
    Close() error
}
```

Now we have the shape of Storage. We can write a simple test to protect the invariant of 036c:
```go
func TestPersistentStateSurvivesRestart(t *testing.T) {
    expectedHard := HardState{...}
    expectedEntries := []Entry{...}

	s1 := NewStorage(dir)
	err := s1.Save(expectedEntries, expectedHard)
	s1.Close()

	s2 := NewStorage(dir)
	defer s2.Close()

	actualHard, err := s2.InitialState()
    Assert.Equal(expectedHard, actualHard)

    actualEntries, err := s2.Entries(lo, hi)
    Assert.Equal(expectedEntries, actualEntries)
}
```

A `MemoryStorage` like `etcd/raft` would be enough to get the interface right. But `kv-go` needs a `DurableStorage` — we need to write data to disk. Fortunately, `wal.go` helps. The split becomes clear during implementation: `Storage` defines the batch payload (`HardState` + entry bytes), while `wal.go` owns the outer length-prefixed frame on disk. For 036c, a contiguous memory buffer is enough, and a simple length-prefixed frame header for each entry is sufficient for writing and replaying.

Let me check what we have in `wal.go` to decide the next step. Unfortunately, `wal.go` in `kv-go/engine` was designed deeply coupled with the `DB`. We need a pure one. I'm going to create an internal `wal.go` inside the raft package.

For replay, 036c scans the WAL frame by frame on startup — no indexing, no random access structure, just sequential decode and rebuild. If the tail is torn, replay truncates back to the last good frame.

## Conclusion
036c is complete when:
- an internal `wal.go` with simple APIs is built and tested
- `DurableStorage` is built and tested
- `TestPersistentStateSurvivesRestart` passes
- torn tail corruption is recoverable without violating restart safety
