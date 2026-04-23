# 037n — The PreVote

```
--- FAIL: TestPartitionedNodeDisruptsLeaderWithStaleVote_037n
    Error: Not equal:
        expected: 0x2  (Leader)
        actual  : 0x0  (Follower)
    Messages: leader must not step down from a partitioned node's stale vote request
```

Partition a node, let its election timer fire, reconnect it, watch the leader yield to a stale-term vote request from a node with no quorum chance. The cluster takes an unnecessary availability hit.

## Reasoning

### Election safety (§5.2)

At most one leader can be elected in a given term (§5.2). A leader must step down when receiving any message from a higher term. The test fails, but the guard works correctly. The partitioned node from the minority takes the authority from the leader in the majority — that's the problem. It increases its term indefinitely per `electionTimeout` until reconnected. We need a mechanism to change the behavior.

### Two-stage election

`etcd` solves this by splitting the election into two stages. A node starts an election by broadcasting `MsgPreVote` to its peers. Unlike `MsgVote`, `MsgPreVote` does not advance the node's local term — it sends `r.term + 1` in the message to ask "would you vote for me at the next term?" while keeping `r.term` unchanged. Peers evaluate the request with the same logic as `MsgVote` — reject when the incoming term is a past epoch, or the peer has already voted for another node in the current term, or the requesting node's log isn't at least as up-to-date. When the PreVote wins majority consensus, the node advances the term and sends `MsgVote` for the second stage. Otherwise, it either retries PreVote or votes for another node — either way, the local term does not increase.

In the partitioned case, the node keeps trying PreVote again and again, but each time it fails to proceed since it cannot reach quorum. When it reconnects, its PreVote is rejected either by the freshness check or by the higher-term authority guard (036j). The first authority message from the leader stops its further PreVote attempts.

The two-stage election solves the partition disruption. But `MsgHup` is not the only election trigger — 035 introduced `campaignTransfer`, where the current leader sends `MsgTimeoutNow` to a target node to hand over leadership. Does that path also need PreVote? No. The leader initiated the transfer, so it already knows the target is reachable and up-to-date. There is no partition to protect against; the extra round trip would only slow down the transfer. This bypass is deferred.

But splitting the process into two rounds opens a question: what if multiple nodes start PreVote simultaneously? The Raft paper proposes randomizing the election timeout to avoid most split votes. Under this mechanism, a split vote should resolve within a few rounds.

What matters here is not the split vote itself — Raft already handles that — but how PreVote behaves when concurrent candidates both pass the pre-check.

### PreVote is non-exclusive

A naive implementation of PreVote would copy the current logic for `MsgVote` — set `votedFor` on grant, reject duplicates. The bad code smell tells me it's no good. At least it conflates with `MsgVote`; from that perspective we should not introduce PreVote into our Raft model at all. There must be a deeper reason `etcd` does it differently. By digging into `etcd/raft` I found it:

```go
if m.Type == pb.MsgVote {
    // Only record real votes.
    r.electionElapsed = 0
    r.Vote = m.From
}
```

This guard says PreVote is non-exclusive: `votedFor` is never set for `MsgPreVote`, so a node can grant PreVote to multiple candidates. Why? Think from the opposite direction: what if PreVote were exclusive? In a 3-node cluster where all 3 nodes start PreVote simultaneously, each votes for itself and no one else — no one wins. If the randomized timeout doesn't spread them well, it repeats. The system's convergence time becomes unpredictable. Thus PreVote cannot be exclusive.

The purpose of PreVote is filtering out disruptive candidates (partitioned nodes with stale logs), not selecting a winner. Making it exclusive would conflate the filter with the election, adding a liveness risk for zero safety benefit — since the real `MsgVote` round already guarantees at-most-one-leader-per-term.

Non-exclusive PreVote means multiple nodes can pass the pre-check and campaign simultaneously at the same advanced term. What happens to the losers?

### Candidate step-down

Two nodes both win PreVote, both call `becomeCandidate()`, both advance to term T. One wins the real `MsgVote` majority, becomes leader, and starts broadcasting authority via `MsgApp`, `MsgHeartbeat`, and `MsgSnap`. The loser is still a candidate at term T. Since `m.Term == r.Term`, the higher-term authority guard (036j) does not fire — it only acts on `m.Term > r.Term`. Without explicit handling, the loser keeps ticking elections forever.

That's why `etcd` handles `MsgApp`, `MsgHeartbeat`, and `MsgSnap` in `stepCandidate`: the candidate recognizes the sender as the legitimate leader of the same term, steps down to follower, and begins replicating immediately.

```go
case pb.MsgApp:
    r.becomeFollower(m.Term, m.From) // always m.Term == r.Term
    r.handleAppendEntries(m)
```

The global guard handles cross-term authority. Same-term authority — another node won the election you were competing in — is handled here.

### The time window

Splitting a process into two steps always creates a time window. What if a node reaches the election timeout right after it has voted for a candidate and before the candidate's `MsgVote` arrives? The answer comes immediately: the `MsgVote` carries a higher term that forces the node to vote for the candidate. And the `MsgPreVote` it sends is harmless — it will be rejected naturally by nodes that already have a leader in the current term.

## Spec

### New state: `PreCandidate`

Add `PreCandidate` to the `State` enum. A PreCandidate uses `stepCandidate` (shared with `Candidate`) but does not increment the term or set `votedFor`.

### New message types: `MsgPreVote`, `MsgPreVoteResp`

Add to the protobuf `MessageType` enum. `MsgPreVote` carries `Term = r.term + 1` (the term the node *would* campaign for), `Index`, and `LogTerm` (same as `MsgVote`). `MsgPreVoteResp` carries `Reject` and the responding node's term.

### `becomePreCandidate()`

```go
func (r *Raft) becomePreCandidate() {
    r.state = PreCandidate
    r.step = stepCandidate
    r.tick = r.tickElection
    r.lead = None
    // Does NOT call reset() — that would clear the term and votedFor,
    // which must be preserved. Only the votes map needs resetting.
    r.resetVotes()
}
```

`resetVotes()` is extracted from `becomeCandidate`, which does the same map initialization. Both callers share it.

### `Step` changes

**`MsgHup`:** call `becomePreCandidate()` instead of `becomeCandidate()`. Broadcast `MsgPreVote` with `Term = r.term + 1`.

**`m.Term > r.Term` guard:** add two exceptions that do not trigger `becomeFollower`:
- `MsgPreVote`: never change term in response to a PreVote.
- `MsgPreVoteResp`: the term in a PreVote response is either the future term (granted) or the responder's higher term (rejected). Neither should trigger step-down — let `stepCandidate` tally the result.

**`m.Term < r.Term` guard:** reject `MsgPreVote` from lower terms with `MsgPreVoteResp{Reject: true}`.

**`MsgPreVote` handling (same term):** evaluate `canVote` and log freshness with the same logic as `MsgVote`. Grant or reject. Do **not** set `votedFor`.

### Three-state vote result

The current `voteQuorumReached()` returns a boolean — it can't distinguish "not yet decided" from "lost." PreVote needs early rejection detection: if a PreCandidate loses majority, it should step down immediately rather than wait for the election timeout.

Change `resetVotes()` to only record self-vote. Peer entries are added on response, not pre-populated. This distinguishes "not responded" (absent) from "rejected" (`false`).

```go
func (r *Raft) resetVotes() {
    r.votes = make(map[uint64]bool)
    r.votes[r.id] = true
}
```

Replace `voteQuorumReached()` with `voteResult()` returning a three-state enum:

```go
type VoteResult uint8
const (
    VotePending VoteResult = iota
    VoteWon
    VoteLost
)

func (r *Raft) voteResult() VoteResult {
    granted, rejected := 0, 0
    for _, v := range r.votes {
        if v { granted++ } else { rejected++ }
    }
    quorum := (len(r.peers)+1)/2 + 1
    if granted >= quorum {
        return VoteWon
    }
    total := len(r.peers) + 1
    if granted + (total - len(r.votes)) < quorum {
        return VoteLost
    }
    return VotePending
}
```

### `stepCandidate` changes

**`MsgPreVoteResp` and `MsgVoteResp`:** record vote, call `voteResult()`. On `VoteWon` → PreCandidate calls `becomeCandidate()` and broadcasts `MsgVote`; Candidate calls `becomeLeader()`. On `VoteLost` → `becomeFollower(r.term, None)`. On `VotePending` → wait.

**`MsgApp`, `MsgHeartbeat`, `MsgSnap`:** step down to follower, handle the message. This covers same-term authority from a winning candidate.

### `send` changes

`MsgPreVote` and `MsgPreVoteResp` must carry an explicit term (like `MsgVote`/`MsgVoteResp`). Add them to the term-required check in `send()`.

## Minimum tests

**Invariant:** a node that cannot win quorum consensus on PreVote will not advance the term.

1. **Partitioned node does not disrupt leader** — a reconnected node whose log is stale has its PreVote rejected; the leader remains in its original term.
2. **PreVote quorum required before term advance** — a PreCandidate that receives only minority grants stays at its original term and does not send `MsgVote`.
3. **PreVote grants are non-exclusive** — a voter grants PreVote to two different candidates in the same round; both proceed to the real election.
4. **Candidate steps down on same-term authority** — a candidate at term T that receives `MsgApp` at term T becomes a follower and accepts the entries.
5. **PreVote does not set votedFor** — a node that grants PreVote can still vote for a different candidate in the subsequent `MsgVote` round.
6. **Simultaneous PreVote resolves in one round** — two nodes that both win PreVote and campaign at the same term: one becomes leader, the other steps down on the first authority message.
7. **PreVote rejected by higher-term node** — a node at term T rejects a PreVote for term T (same term, already has a leader) with `Reject: true`.
8. **Majority rejection triggers immediate step-down** — a PreCandidate that receives majority rejections becomes a follower without waiting for the election timeout.
9. **Higher-term PreVote does not demote receiver** — a node at term T that receives `MsgPreVote` with term T+1 does not change its local term or step down.
10. **Granted PreVoteResp does not demote sender** — a PreCandidate at term T that receives `MsgPreVoteResp` with term T+1 and `Reject=false` does not step down.

## Open threads

1. **Leader transfer bypass** — `campaignTransfer` skips PreVote in `etcd` because the transfer is initiated by the current leader, so there's no partition to protect against.