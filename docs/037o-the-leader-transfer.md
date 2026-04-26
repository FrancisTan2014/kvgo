# 037o — The Leader Transfer

```
--- PASS: TestPlannedLeaderStepDownCostsFullElectionTimeout_037o
    raft_test.go:2951: ticksToNewLeader: 15  electionTimeout: 10
```

Three-node cluster, node 1 is leader. Planned restart: node 1 stops ticking. Nodes 2 and 3 continue. 15 ticks later, a new leader emerges. 10 of those ticks are pure waiting — no follower can start campaigning until its election timer expires. The remaining 5 are the PreVote round, the Vote round, and message delivery. Every planned leader step-down pays this cost. With `electionTimeout = 1s`, that's 1.5 seconds of write downtime on every rolling restart.

## Reasoning

### The wait is waste

The cost is expected — election timeout is designed for crash recovery, not deliberate restart. This is not a problem if there is no reason to manipulate the election process. But when the leader chooses to leave, the wait is pure waste. There are maintenance scenarios that require taking a leader offline intentionally:
- Rolling upgrade — deploy a new binary one node at a time.
- Hardware maintenance — the leader's host needs a kernel patch, disk replacement, or memory upgrade.
- Load rebalancing — one node is overloaded (CPU, disk I/O).
- Graceful shutdown — the operator runs kill or a drain command.
- Region/AZ migration — moving a node to a different availability zone.

They all suffer the downtime. We need a mechanism for improving the availability.

### The §3.10 mechanism

In Ongaro's full dissertation ([§3.10](https://web.stanford.edu/%7Eouster/cgi-bin/papers/OngaroPhD.pdf)) he describes the leadership transfer mechanism:
1. The leader stops accepting new proposals
2. The leader catches up the target follower via `MsgApp`
3. The leader sends a `TimeoutNow` to the target
4. The target immediately calls an election (skipping its election timer)

It is easy to add a switch to the server and reject client requests when the switch is on. But remember that followers may also forward proposals to the leader, thus the guard must stay inside the Raft model — a field `leadTransferee` on the `Raft` struct that, when non-zero, makes `stepLeader` drop `MsgProp`. The following process is straightforward. In the happy path, the leader steps down naturally when `MsgVote` arrives — forced by the higher-term authority. The leadership transfer completes when the prior leader's `Raft.lead` is set by the new leader's first authority message. What if the target crashes during the process?

It can crash before or after it starts an election. In the former case, the leader is still the leader. When its election timeout fires, it must abort the transfer and remove the proposal guard to resume client operations. The latter case is trickier because the cluster loses its leader. The Raft algorithm ensures a new leader will emerge naturally. For the prior leader, it clears the guard when it steps down — `reset()` runs on every state transition, so any path out of `Leader` (whether via `becomeFollower` on the `MsgVote` or via CheckQuorum demotion) clears `leadTransferee` automatically.

### The PreVote bypass

The target receives `MsgTimeoutNow` and calls `hup()`. But 037n routes every `hup()` through PreVote first. Should the transfer target do a PreVote round? There is no partition to protect against. PreVote would add a round trip for zero safety benefit. The target must skip PreVote and go straight to `becomeCandidate`. This requires a way to tell `hup()` which election path to take — a campaign type that distinguishes transfer from normal election.

What if two transfers overlap? The leader is transferring to node 2, and a second `MsgTransferLeader` arrives for node 3. etcd aborts the first and starts the second — only one transfer can be in flight. The timeout guard from the first transfer is already cleared by setting `leadTransferee` to the new target.

## Spec

### New message types: `MsgTransferLeader`, `MsgTimeoutNow`

Add to the protobuf `MessageType` enum. `MsgTransferLeader` carries the target node ID in `From`. `MsgTimeoutNow` carries no payload — it is a directive to campaign immediately.

### New field: `leadTransferee`

Add `leadTransferee uint64` to `Raft`. When non-zero, `stepLeader` drops `MsgProp` with `ErrProposalDropped`. Cleared by `reset()`.

### `stepLeader` changes

**`MsgTransferLeader`:**
1. Reject if the target is self.
2. If a transfer is already in progress to the same target, ignore.
3. If a different transfer is in progress, abort it (overwrite `leadTransferee`).
4. Set `leadTransferee = target`. Reset `electionElapsed = 0` (timeout starts).
5. If the target's `MatchIndex == lastLogIndex` → send `MsgTimeoutNow` immediately.
6. Otherwise → `sendAppend(target)` to catch it up. When the target's `MsgAppResp` advances `MatchIndex` to `lastLogIndex`, send `MsgTimeoutNow`.

**`MsgProp`:** reject with `ErrProposalDropped` when `leadTransferee != 0`.

**`tickHeartbeat` timeout:** if `leadTransferee != 0` and `electionElapsed >= electionTimeout`, clear `leadTransferee`. The transfer failed — resume normal operations.

### `stepFollower` changes

**`MsgTimeoutNow`:** call `hup(CampaignTransfer)`. No PreVote — go straight to `becomeCandidate()` and broadcast `MsgVote`.

### `Step` changes

**Campaign type:** introduce `CampaignType` to distinguish election paths. `MsgHup` currently hardcodes `becomePreCandidate()`. Replace with `hup(t CampaignType)`:

```go
type CampaignType string
const (
	CampaignPreElection CampaignType = "PreElection"
	CampaignElection    CampaignType = "Election"
	CampaignTransfer    CampaignType = "Transfer"
)
```

**`MsgHup` handler:** call `hup(CampaignPreElection)`.

**`hup()` dispatch:**
- `CampaignPreElection` → `becomePreCandidate()`, broadcast `MsgPreVote`
- `CampaignElection` → `becomeCandidate()`, broadcast `MsgVote`
- `CampaignTransfer` → `becomeCandidate()`, broadcast `MsgVote`

`CampaignTransfer` and `CampaignElection` share the same path — both skip PreVote. The type exists so `MsgTimeoutNow` can route through `hup()` instead of bypassing it.

## Minimum tests

**Invariant:** a leader that initiates a transfer hands over leadership within one round trip when the target is caught up, and aborts within one election timeout when the target is unreachable.

1. **Caught-up target receives MsgTimeoutNow immediately** — leader with `MatchIndex == lastLogIndex` sends `MsgTimeoutNow` on `MsgTransferLeader`.
2. **Behind target is caught up first** — leader sends `MsgApp` to the target; `MsgTimeoutNow` is sent only after `MatchIndex` reaches `lastLogIndex`.
3. **Proposals dropped during transfer** — `MsgProp` returns `ErrProposalDropped` while `leadTransferee != 0`.
4. **Transfer aborts on timeout** — if no `MsgTimeoutNow` is sent within `electionTimeout`, `leadTransferee` is cleared and proposals resume.
5. **Target campaigns without PreVote** — `MsgTimeoutNow` triggers `becomeCandidate` directly, not `becomePreCandidate`.
6. **Target wins election and old leader steps down** — the target's `MsgVote` at the next term forces `becomeFollower` on the old leader via the higher-term guard.
7. **Concurrent transfer replaces previous** — a second `MsgTransferLeader` for a different target aborts the first and starts the new one.
8. **Transfer to self is ignored** — `MsgTransferLeader` with `From == leader.id` is a no-op.
9. **Follower forwards MsgTransferLeader to leader** — a follower that receives `MsgTransferLeader` forwards it to `r.lead`.
10. **Guard cleared on any state transition** — `reset()` clears `leadTransferee`, so CheckQuorum demotion or higher-term messages automatically abort an in-flight transfer.

## Open threads

(None. All forks in Reasoning are resolved.)