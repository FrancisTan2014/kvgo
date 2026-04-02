# 037h — The Heartbeat

Heartbeats are empty `MsgApp` messages. Followers reply with `MsgAppResp` — the same type they use for replication. The leader has no way to tell "I acked your heartbeat" apart from "I acked your entries."

ReadIndex will need to broadcast a heartbeat and count quorum responses *to that specific round*. With a shared message type, a stale `MsgAppResp` from an earlier replication batch satisfies the check. The leader thinks it proved liveness; it proved nothing.

## Boundary

Separate heartbeats from log replication by introducing dedicated `MsgHeartbeat` and `MsgHeartbeatResp` message types. The leader broadcasts `MsgHeartbeat` on tick; followers respond with `MsgHeartbeatResp`. Neither message carries log entries.

In scope:
- `MsgHeartbeat` and `MsgHeartbeatResp` as dedicated protobuf types — gives the leader a clean signal that a peer responded to *this* heartbeat round, not to an earlier replication batch
- Commit capping — the leader sets `Commit` on each heartbeat to `min(MatchIndex, commitIndex)`. A lagging follower must not be told to commit entries it hasn't received. etcd enforces the same cap in `sendHeartbeat`
- Heartbeat-triggered catch-up — when the leader receives `MsgHeartbeatResp` from a peer whose log is behind, it sends a `MsgApp` to close the gap. Replaces the implicit path where empty `MsgApp` heartbeats would trigger `MsgAppResp` which would trigger replication

Deferred:
- ReadIndex protocol (`MsgReadIndex` / `MsgReadIndexResp`) — the consumer of heartbeat ack counting. Next episode (037i)
- CheckQuorum — uses heartbeat responses to detect partition. Separate concern

Out of scope:
- `Context` field on heartbeat messages — etcd uses it for ReadIndex request correlation. We add it when ReadIndex needs it (037i), not before
- Flow control and inflight tracking on heartbeat-triggered appends

## Design

### What etcd does

etcd separates heartbeats (`MsgHeartbeat`, type 8) from appends (`MsgApp`, type 3). The leader caps commit per peer; the follower handler is minimal:

```go
// sendHeartbeat — leader side
commit := min(pr.Match, r.raftLog.committed)
r.send(pb.Message{To: to, Type: pb.MsgHeartbeat, Commit: commit, Context: ctx})
```

```go
// handleHeartbeat — follower side
r.raftLog.commitTo(m.Commit)
r.send(pb.Message{To: m.From, Type: pb.MsgHeartbeatResp, Context: m.Context})
```

We follow the same pattern, minus `Context` (deferred to 037i).

### Protobuf changes

Add two message types to the enum:

```protobuf
enum MessageType {
    ...
    MsgHeartbeat      = 9;   // ← new
    MsgHeartbeatResp  = 10;  // ← new
}
```

No new fields on `Message` — `Commit`, `From`, `To`, and `Term` already exist.

### Leader: broadcast heartbeat

The `stepLeader` handler for `MsgBeat` changes from broadcasting empty `MsgApp` to broadcasting `MsgHeartbeat`:

```go
case raftpb.MessageType_MsgBeat:
    for _, pid := range r.peers {
        prs := r.progress[pid]
        r.send(&raftpb.Message{
            To:     pid,
            Type:   raftpb.MessageType_MsgHeartbeat,
            Commit: min(prs.MatchIndex, r.commitIndex),
        })
    }
```

### Follower: handle heartbeat

Add a `MsgHeartbeat` case to `stepFollower`:

```go
case raftpb.MessageType_MsgHeartbeat:
    r.electionElapsed = 0
    r.lead = m.From
    if m.Commit > r.commitIndex {
        r.CommitTo(m.Commit)
    }
    r.send(&raftpb.Message{
        To:   m.From,
        Type: raftpb.MessageType_MsgHeartbeatResp,
    })
```

No log-matching logic. No entries. No reject path. Strictly lighter than `MsgApp`.

### Leader: handle heartbeat response

Add a `MsgHeartbeatResp` case to `stepLeader`:

```go
case raftpb.MessageType_MsgHeartbeatResp:
    prs, exists := r.progress[m.From]
    if !exists {
        return nil
    }
    if prs.MatchIndex < r.lastLogIndex {
        r.sendAppend(m.From)
    }
```

If the peer's log is behind, the leader sends a `MsgApp` to close the gap — the peer gets back on track after the next heartbeat round without waiting for a new proposal. This requires extracting the per-peer append logic from `MsgProp` into a `sendAppend(to)` helper.

### What doesn't change

- `tickHeartbeat` still fires `MsgBeat` on the heartbeat interval. The trigger is the same; only what `MsgBeat` produces changes.
- `MsgApp` / `MsgAppResp` handling is untouched. Replication works exactly as before.

## Minimum tests

**Invariant:** heartbeats and heartbeat responses flow through dedicated message types, separate from log replication, enabling the leader to distinguish heartbeat acknowledgements from append acknowledgements.

1. **Leader broadcasts MsgHeartbeat on tick** — proves `MsgBeat` produces `MsgHeartbeat`, not empty `MsgApp`.
2. **Heartbeat carries capped commit index** — proves commit is `min(MatchIndex, commitIndex)` per peer.
3. **Follower resets election timer on heartbeat** — proves heartbeats suppress elections.
4. **Follower advances commit on heartbeat** — proves commit advancement works without entries.
5. **Follower responds with MsgHeartbeatResp** — proves the response path.
6. **Leader sends MsgApp after MsgHeartbeatResp from behind peer** — proves heartbeat responses trigger log catch-up.
7. **Leader skips MsgApp after MsgHeartbeatResp from caught-up peer** — proves caught-up peers don't get redundant appends.
8. **Heartbeat does not interfere with normal replication** — proves `MsgApp`/`MsgAppResp` are unaffected by the new types.

## Open threads

1. **Heartbeat Context for ReadIndex** — etcd's `MsgHeartbeat` carries a `Context` field that the leader uses to correlate a heartbeat round with a pending ReadIndex request. We omit `Context` until 037i, when ReadIndex needs it. If 037i requires adding a field to `Message`, the protobuf change happens there.
2. **CheckQuorum** — etcd uses heartbeat responses to track which peers are recently active. If a leader hasn't heard from a quorum within an election timeout, it steps down. This prevents a partitioned leader from serving stale reads. Deferred until liveness guarantees are in scope.
