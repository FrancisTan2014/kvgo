# 037m — The CheckQuorum

A three-node leader in term 1. Zero `MsgHeartbeatResp` ever arrives. Ten heartbeat ticks pass — a full election-timeout window. The leader is still `Leader`, still claims `lead = self`. No code path noticed it went blind.

```
--- FAIL: TestLeaderStepsDownWhenQuorumSilent_037m
    Error: Not equal:
        expected: 0x0  (Follower)
        actual  : 0x2  (Leader)
    Messages: leader must step down after electionTimeout with no peer responses
```

Jepsen saw none of this. 037l's ReadIndex already stops stale data from reaching clients, so partition runs return `{:valid? true}` whether the leader steps down or not. The failure is internal: the leader's own state lies. A unit test reads that state directly.

## Reasoning

### What 037l already fixed, and what it did not

037l's ReadIndex closes the *safety* hole: a stale leader cannot serve stale data, because a read requires quorum proof. If the leader can't reach a majority, `ctx.DeadlineExceeded` fires at `ReadTimeout` and the client sees `504`. No bad bytes on the wire.

But the leader's internal state is a lie. From its perspective nothing has changed — `state = Leader`, `lead = self`, `term = T`. Every read and every write burns the full timeout (seconds) before failing. A client told "the leader is N1" has no cheap way to discover it's wrong — N1 itself says it's still leader. The node is serving a falsehood to anyone who asks what role it has.

ReadIndex guards safety; CheckQuorum guards *honesty* and *liveness*. They attack different halves of the same failure. ReadIndex refuses to serve without proof. CheckQuorum refuses to claim leadership without proof.

### What proof, and how often

The leader already has the evidence. Every `MsgHeartbeatResp` and every `MsgAppResp` from a peer is a liveness signal — the peer received the leader's message at the current term and is alive enough to reply. The only thing missing is: the leader never looks.

A naïve design: step down on any missing heartbeat round. A successful round to a quorum *does* prove leadership — at that instant — which is exactly what ReadIndex relies on. But the converse isn't symmetric. A single failed round means "I could not confirm leadership in this window," not "I have lost it." The costs of the two possible wrong answers are asymmetric: if the leader wrongly thinks it's still leader for one more round, ReadIndex already prevents the stale read; if the leader wrongly steps down, the cluster loses its leader, burns an election, and interrupts every in-flight write and ReadIndex round. The second error is strictly more disruptive, so a single missed round isn't warrant enough to act. Demand sustained absence.

The idiomatic resolution — etcd, tikv — is a two-clock protocol:

- **heartbeat clock** (fast, ~100ms): leader sends `MsgHeartbeat` to every peer; peers reply with `MsgHeartbeatResp`. This is already implemented. It does liveness *sensing*.
- **election clock** (slow, ~1s): once per election-timeout window, the leader *tallies* how many peers it heard from during that window. If fewer than a quorum (self included), step down. Reset all recent-activity flags; start a new window.

The ratio matters. `electionTimeout / heartbeatTimeout ≈ 10` means roughly ten heartbeat rounds must be silent before a leader demotes itself. Not because ten rounds *is* partition in any measurable sense — but because Raft already uses the election timeout as its single coarse threshold for "sustained absence." Followers use it before calling an election; the leader can reuse it before stepping down. One constant, both directions. Not a discovery, a reuse.

The protocol is settled. The liveness bit needs somewhere to live.

### What to record, and where

Per-peer `Progress` already tracks `MatchIndex` and `NextIndex` for replication. CheckQuorum adds one bit: `RecentActive`. Stamp it `true` on any message from that peer that proves liveness at the current term (`MsgHeartbeatResp`, `MsgAppResp`). The leader counts itself as always active. The `Progress` map already exists per-peer on the leader — no new data structure, just one field.

"At the current term" is load-bearing. Messages carry the sender's term at the moment they were sent; the network does not. A `MsgHeartbeatResp` with `m.Term < r.term` is an echo from a past epoch — delayed in flight, or from a peer that hasn't yet caught up to the leader's current term. It proves the peer was alive *then*, not now. Stamping on it would let a leader survive CheckQuorum on ghosts: enough delayed responses from the last epoch and the current window looks healthy. The guard is `m.Term == r.term` at the stamp site. Higher-term responses don't reach this code path — they demote the leader at the top of `Step` before `stepLeader` sees them.

### Which clock ticks the check

`tickHeartbeat` runs on the fast clock. It already fires `MsgBeat` every heartbeat-timeout ticks. The election clock is not a separate timer — it is a counter that `tickHeartbeat` also increments (`electionElapsed++`). When `electionElapsed >= electionTimeout`, the election-window has closed. Reuse the existing field; the leader's tick path owns both clocks.

This is cleaner than it sounds. Before 037m, `electionElapsed` was dormant on leaders — only followers and candidates cared about it. After 037m, the leader uses the same field for the opposite question: "how long since I last *confirmed* I'm still leader?" The field already exists because `becomeFollower` and `becomeCandidate` both reset it; turning it on for leaders only requires one line in `tickHeartbeat`.

The check fires. The tally comes up short. What state does the leader become?

### What state to transition to

A leader that fails CheckQuorum must demote itself. Three options in the state machine: candidate, follower, or something new.

**Candidate.** Bumps the term, starts an election. Wrong. From this node's perspective, no term change is warranted — it just can't prove it's leader *right now*. Aggressive demotion to candidate would flood the cluster with stale-term vote requests after every partition. And if the node is the minority-side partition, it can never win anyway — it just raises the cluster term pointlessly.

**Follower with `lead = self`.** Preserves "I think I'm the leader, I just can't act on it." Wrong for a different reason: it confuses every downstream check. `stepFollower` uses `r.lead` to forward proposals. A follower whose `lead == self` would forward proposals to itself in a loop. HEALTH would lie in a new direction.

**Follower with `lead = None`, term unchanged.** The honest state. "I am in term T, I do not know who the leader is, I am waiting." Incoming proposals fast-fail with `ErrProposalDropped` (037c behavior). Incoming `MsgApp` or `MsgHeartbeat` from a real higher-term leader will be accepted through the standard higher-term-authority rule (036j) — term advances to T+1, `lead` aligns. If the partition was one-way and our node is actually still viable, the next `MsgHup` tick will trigger a normal election.

No new state, no new message type, no new edge case. The Raft invariants correct the node on their own.

But the leader wasn't idle at the moment it stepped down. Writes were in flight.

### In-flight proposals at step-down

The leader may have uncommitted entries in its log when it demotes. What happens to them?

Two cases. *Case A:* the entries were replicated to a quorum before demotion. They are committed — Raft safety says so, regardless of this node's state transition. When the partition heals, the new leader has them too (it was on the majority side), and normal replication keeps them. The proposer was unblocked when `apply` fired earlier. Nothing to do.

*Case B:* the entries never reached quorum. They sit in this node's log as uncommitted tail. On demotion, nothing rewrites them. Later, an `MsgApp` from the new (higher-term) leader arrives with a `PrevLogIndex` before our uncommitted tail — the log-matching rule (036j) truncates our extra entries. The proposer on this node is still blocked in the wait map. It unblocks on `ctx.Done()` — the client sees `WriteTimeout`, retries against the new leader, and the retry is idempotent.

Step-down touches no tail, writes no log, sends no message. It is a pure state transition plus a `Progress` reset. The log's rewrite is handled by the protocol that already handles it everywhere else.

### ReadIndex requests pending at step-down

Two leader-only queues hold ReadIndex requests mid-flight:

- `readOnly.pendingReadIndex` — requests awaiting quorum ack, built from `MsgHeartbeatResp` contexts. Only the leader receives those acks.
- `pendingReadIndexRequests` — requests received before any current-term entry has committed, waiting for `drainPendingReadIndexRequests` to fire on the next commit advance. Only the leader commits.

Neither is useful after step-down. The natural place to wipe them is `reset(term)` — already called by `becomeFollower`, `becomeCandidate`, and `becomeLeader` — so every state transition clears both. The client, blocked on its own request context, unblocks through the normal `ctx.Done()` path at `ReadTimeout` and retries against the new leader.

One last case: what if there are no peers to be silent from?

### Standalone cluster

A single-node cluster has quorum by itself. `RecentActive` tally is always `>= quorum`. CheckQuorum is a no-op. The same branch that let singleton ReadIndex skip the heartbeat applies here — no extra code, the math works out.

## Spec

### `Progress` struct — one new field

```go
type Progress struct {
    MatchIndex   uint64
    NextIndex    uint64
    RecentActive bool  // NEW: set on MsgHeartbeatResp / MsgAppResp at current term
}
```

### `stepLeader` — stamp liveness

On `MsgHeartbeatResp` and `MsgAppResp`, after existing handling:

```go
if pr, ok := r.progress[m.From]; ok {
    if m.Term == r.term {
        pr.RecentActive = true
    }
}
```

### `tickHeartbeat` — drive the election clock too

```go
func (r *Raft) tickHeartbeat() {
    r.heartbeatElapsed++
    r.electionElapsed++  // NEW

    if r.electionElapsed >= r.electionTimeout {  // NEW block
        r.electionElapsed = 0
        if err := r.Step(&raftpb.Message{From: r.id, Type: MsgCheckQuorum}); err != nil {
            r.logger.Debug("check quorum step failed", "error", err)
        }
    }

    // ... existing heartbeat dispatch ...
}
```

### `MsgCheckQuorum` — new local-only message

A local directive, never sent on the wire. Handled only by `stepLeader`. Counts active peers (self + peers with `RecentActive == true`). If the tally is a quorum, reset all `RecentActive = false` for the next window. Otherwise, `becomeFollower(r.term, None)`.

Kept as a message rather than inlined in `tickHeartbeat` so the check is reachable from tests without ticking a clock, and so the state transition goes through the single `Step` entry point like every other transition.

```go
// stepLeader, new case:
case MsgCheckQuorum:
    if !r.hasQuorumActive() {
        r.logger.Info("leader lost quorum, stepping down",
            "term", r.term, "id", r.id)
        r.becomeFollower(r.term, None)
        return nil
    }
    r.resetRecentActive()
```

Helpers:

```go
func (r *Raft) hasQuorumActive() bool {
    active := 1 // self
    for _, pr := range r.progress {
        if pr.RecentActive {
            active++
        }
    }
    return active >= (len(r.peers)+1)/2+1
}

func (r *Raft) resetRecentActive() {
    for _, pr := range r.progress {
        pr.RecentActive = false
    }
}
```

### `reset` — clear ReadIndex queues on every transition

`reset(term)` already runs on every state change. Add two lines so leader-only ReadIndex state doesn't leak forward:

```go
r.readOnly = newReadOnly()
r.pendingReadIndexRequests = nil
```

### `becomeLeader` — start clean

After building the `progress` map, peers start with `RecentActive = false`. Self is implicit in the tally. `electionElapsed` resets in `r.reset(r.term)` already, along with the ReadIndex queues (see `reset` above). No extra lines here.

### `becomeFollower` — nothing new

Existing code resets `electionElapsed` and clears leader fields. CheckQuorum adds no step-down-specific cleanup; the ReadIndex wipe rides on `reset(term)`, and in-flight proposals unblock through the client's context-timeout path.

## Minimum tests

**Invariant:** a leader that cannot confirm quorum activity within one election-timeout window demotes itself to follower at the same term with `lead = None`.

1. **Leader steps down when no peers respond** — tick through a full election-timeout window with zero `MsgHeartbeatResp`; assert `state == Follower`, `lead == None`, `term` unchanged.
2. **Leader stays leader with quorum responses** — tick through a full window while a quorum of peers sends `MsgHeartbeatResp` periodically; assert `state == Leader`.
3. **Sub-quorum responses are not enough** — in a five-node cluster (quorum 3), tick through a window while only one peer responds; assert step-down.
4. **RecentActive resets across windows** — pass a window with full quorum (stay leader); pass a second window with zero responses; assert step-down in the second window. Proves the flag is cleared between windows.
5. **Singleton cluster never steps down** — one-node cluster, tick arbitrarily many windows, assert `state == Leader`.
6. **MsgAppResp counts as liveness** — tick through a window while peers send only `MsgAppResp` (no heartbeat responses); assert leader stays leader.
7. **Stale-term responses don't count** — send `MsgHeartbeatResp` with `m.Term < r.term`; tick through a window; assert step-down. Proves CheckQuorum is term-aware.
8. **Stepped-down node accepts higher-term `MsgApp`** — after CheckQuorum demotion, deliver `MsgApp` at term T+1; assert term advances, `lead` becomes the new leader. Proves the Raft model corrects the node naturally on rejoin.
9. **Step-down clears pending ReadIndex queues** — seed `r.readOnly.pendingReadIndex` and `r.pendingReadIndexRequests` with one entry each, trigger CheckQuorum step-down with empty `RecentActive`, assert both are empty. Proves leader-only ReadIndex state does not leak past demotion.

## Open threads

1. **Clean step-down notification** — in-flight proposals and pending ReadIndex requests unblock on their own context timeout at step-down, not on a fast signal from the demoted leader.
2. **Jepsen leader-isolation workload** — a direct probe against a stale-leader partition was cut in favor of a unit test; becomes viable when an honest `/health`-style view of raft state exists to assert against.
3. **Unified quorum primitive** — `hasQuorumActive` is the third ad-hoc quorum tally in the codebase (after vote counting and ReadIndex ack counting); a single `VoteResult`-style primitive would collapse all three and is a prerequisite for joint-consensus membership changes.
