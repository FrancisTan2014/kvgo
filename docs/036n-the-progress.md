# 036n - The Progress

We left 036 after [036m-the-raft-host](036m-the-raft-host.md) with a gap before rewriting the old write path: the current quorum evidence collection is entry-based, but Raft requires the leader to track each follower's progress.

## The problem

A 3-node cluster. Follower A is at index 3, follower B is at index 4. The leader proposes entry 5. What does it send to each follower?

Currently it can only send entry 5 alone, like the old write path, and wait for quorum. In that model, follower A never gets entry 4 — so we need a separate replication loop to let followers catch up.

## How Raft does it

Raft has no separate catch-up path. Replication *is* the consensus mechanism.

When the leader proposes entry 5, it sends `Entry[4:]` to follower A and `Entry[5:]` to follower B. When both ack, entry 5 reaches quorum — and follower A catches up at the same time. One path, both purposes.

This requires the leader to track each follower's progress. The Raft paper represents progress as two per-follower values: `MatchIndex` and `NextIndex`. In `kv-go`, this replaces the per-entry `acks` map with `map[uint64]*Progress`.

There is a key shift here from the old write path to the Raft write path. The old path — described as a fake quorum write in [036a-the-raft](036a-the-raft.md) — was easy to understand: a client proposes a write, the leader attempts quorum within a timeout, and responds with success or failure. A Raft leader never reports failure. It never drops a single entry. Eventually all entries will be committed. In this model, the application layer handles timeout and duplicate proposals.

## How progress drives the cluster

`MatchIndex` is the highest index that a follower has confirmed. The leader uses the **median** of all `MatchIndex` values (including its own) to advance `commitIndex`: the median of N values is the highest value held by a majority. The updated `commitIndex` reaches followers on the next `MsgApp` — triggered by a new proposal or a heartbeat tick.

`MatchIndex` is initialized to `0` when a new leader wins election, because the leader has no evidence of what any follower has.

`NextIndex` tells the leader where to start the entry slice in a `MsgApp`. It is initialized to `lastLogIndex + 1` — optimistically assuming the follower is fully caught up. If the follower rejects the append, the leader backs `NextIndex` down so the next attempt starts earlier. If the backup crosses the compaction boundary, the leader falls through to snapshot transfer (already built in 036k).

When the leader receives a successful `MsgAppResp` with `Index = X`:

```
progress[from].MatchIndex = X
progress[from].NextIndex  = X + 1
```

When the leader receives a rejected `MsgAppResp`:

```
progress[from].NextIndex--
// if NextIndex is now below the retained log, fall through to snapshot
```

That is how the whole cluster moves forward.

## The design

```go
type Progress struct {
    MatchIndex uint64
    NextIndex  uint64
}

type Message struct {
    ...
    Commit uint64
}

type Raft struct {
    ...
    progress map[uint64]*Progress
}

func (r *Raft) Propose(data []byte) {
    // append new entry to r.log
    // construct MsgApp per follower with entries starting from NextIndex
}

func (r *Raft) Step(m Message) {
    ...
    case MsgApp:
        // append entries
        if m.Commit > r.commitIndex {
            r.CommitTo(m.Commit)
        }

    case MsgAppResp:
        if m.Reject {
            p := r.progress[m.From]
            p.NextIndex--
            // if below retained log → snapshot (036k path)
        } else {
            p := r.progress[m.From]
            p.MatchIndex = m.Index
            p.NextIndex = m.Index + 1
            // compute median of all MatchIndex values
            if median > r.commitIndex {
                r.CommitTo(median)
            }
        }

    case MsgVoteResp:
        ...
        if r.voteQuorumReached() {
            r.state = Leader
            r.progress = make(map[uint64]*Progress, len(r.peers))
            for _, pid := range r.peers {
                r.progress[pid] = &Progress{
                    MatchIndex: 0,
                    NextIndex:  r.lastLogIndex + 1,
                }
            }
        }
    ...
}
```

## Minimum tests

1. `TestProgressInitializedOnElection_036n` — progress is created for each peer with `MatchIndex=0` and `NextIndex=lastLogIndex+1` when a node becomes leader.

2. `TestProposeSendsEntriesFromNextIndex_036n` — leader constructs `MsgApp` per follower using each follower's `NextIndex` as the start of the entry slice.

3. `TestCommitAdvancesOnMajorityMatch_036n` — `commitIndex` advances to the median `MatchIndex` after `MsgAppResp` processing.

4. `TestFollowerAdvancesCommitFromMsgApp_036n` — follower advances `commitIndex` when `MsgApp` carries a higher `Commit`.

5. `TestNextIndexBacksUpOnRejection_036n` — rejected `MsgAppResp` decrements `NextIndex` for the rejecting follower.

## Bounded scope

036n is complete when:
- `r.progress` is initialized when the leader wins election
- `r.progress` replaces the per-entry `acks` map from [036g-the-response](036g-the-response.md)
- 036g tests are superseded with comments
- `Step(MsgAppResp)` advances `commitIndex` through the median `MatchIndex`
- `Step(MsgAppResp)` backs up `NextIndex` on rejection
- `Step(MsgApp)` advances `commitIndex` when `Commit` is higher
- all tests pass

## Out of scope

- Progress reinitialization on leadership change mid-term
- Heartbeat tick as a `MsgApp` trigger
- Progress reinitialization on leadership change mid-term
- Heartbeat tick as a `MsgApp` trigger
