# 036k - The Snapshot Transfer

We left 036 with message-driven replication working only while a lagging follower can still be caught up through retained log entries. Once compaction removes that missing history, append alone can no longer repair the gap.

Once entry-based replication cannot cross the compaction boundary, the leader must switch to snapshot transfer.

The follower-side rule stays small. `MsgApp` carries an append anchor through `(Index, LogTerm)` plus the entries that follow it. The follower answers only one question:

**Can I prove the append anchor this `MsgApp` claims?**

If the local retained log proves that anchor, the follower can append the new entries. If the local snapshot boundary proves that anchor exactly, the follower can also append. Otherwise it must reject. A follower cannot honestly accept an anchor older than its retained history, because `SnapshotMeta` remembers only one exact compacted point: `(LastIncludedIndex, LastIncludedTerm)`.

The leader-side question is different. A rejected append does not mean snapshot immediately. It means the leader must decide whether append repair is still possible. Snapshot transfer is the result of failing to repair with append, not the meaning of rejection itself.

Boundary crossing is therefore a leader-local fact. The leader keeps trying to repair replication through earlier append anchors. Once the next repair point would require an anchor older than the history the leader still retains, the leader can no longer construct an honest `MsgApp`. That is the point where append repair ends and `MsgSnap` becomes necessary.

`MsgSnap` in 036k carries only the leader's current compaction boundary as `SnapshotMeta`. That is enough for this bounded slice. The follower installs that boundary, reconciles its in-memory retained log against that boundary, and can then accept later append attempts anchored at or after the installed snapshot point. Full snapshot payload transfer, chunking, and transport reliability remain out of scope here.

In [036c-the-raft-storage](036c-the-raft-storage.md) we introduced `Storage`, and in [036e-the-compaction.md](036e-the-compaction.md) it became the owner of `SnapshotMeta`. So 036k should let `Raft` depend on `Storage` both to read the leader snapshot boundary and to install a received snapshot boundary on the follower.

That means follower install should not go through `Compact()`. `Compact()` is a local log-retention operation. Installing a leader snapshot is a different state transition and needs its own storage seam. It also means `Step(MsgSnap)` cannot stop at durable storage alone; the follower's in-memory retained state must stop proving anchors older than the installed boundary.

The shape of 036k is basically a new message type, a follower-side anchor check, and a storage-backed snapshot install path:
```go
const (
    ...
    MsgSnap MessageType = 5
)

type Message struct {
    ...
    Snapshot   SnapshotMeta
    RejectHint uint64
}

type Storage interface {
    ...
    Snapshot() (SnapshotMeta, error)
    ApplySnapshot(SnapshotMeta) error
}

type Raft struct {
    ...
    storage Storage
}

func (r *Raft) Step(m Message) error {
    ...
    case MsgApp:
        if anchorExists(m.Index, m.LogTerm) {
            // append entries, respond success
        } else {
            // reject, expose MsgAppResp with RejectHint
        }

    case MsgAppResp:
        if m.Reject {
            nextAnchor := m.RejectHint
            if nextAnchor < r.storage.FirstIndex()-1 {
                snap, _ := r.storage.Snapshot()
                r.messages = append(r.messages, Message{
                    Type: MsgSnap,
                    ...
                    Snapshot: snap,
                })
            } else {
                // continue append repair from an earlier anchor
            }
        }

    case MsgSnap:
        // install the compaction boundary through storage
        _ = r.storage.ApplySnapshot(m.Snapshot)
}
```

So 036k proves:

**when entry-based replication cannot cross the compaction boundary, the leader must transfer a snapshot before replication can continue.**

## Minimum tests

#1 `TestMsgAppIsRejectedWhenAnchorCannotBeProved_036k`
Follower rejects append when the claimed predecessor cannot be proved from either retained log or installed snapshot boundary.

#2 `TestMsgSnapIsReadyWhenRepairCrossesCompactionBoundary_036k`
Leader emits `MsgSnap` only after append repair has crossed the oldest retained leader boundary.

#3 `TestFollowerInstallsSnapshotBoundaryFromMsgSnap_036k`
Follower installs the received snapshot boundary and stops proving anchors older than that installed point.

#4 `TestStorageApplySnapshotDropsCoveredEntries_036k`
Storage persists the new snapshot boundary and discards retained entries covered by that boundary, including across restart.

#5 `TestAppendAfterInstalledSnapshotIsReady_036k`
Once a follower has installed a snapshot boundary, a later append anchored exactly at that boundary is accepted and exposed through `Ready()`.

## Bounded scope

036k is complete when:
- `Step(MsgApp)` exposes `MsgAppResp` with `Reject=true` when the follower cannot prove the append anchor locally
- `Step(MsgAppResp)` exposes `MsgSnap` only when append repair would cross the leader's retained boundary
- `Step(MsgSnap)` installs the snapshot boundary on the follower through `Storage.ApplySnapshot()` and reconciles in-memory retained state with that boundary
- `Storage.ApplySnapshot()` persists the boundary and drops covered retained entries
- tests pass