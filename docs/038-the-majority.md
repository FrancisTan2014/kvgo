# 038 — The Majority

Single-node server, single-node Raft. A benchmark run to measure baseline throughput against etcd. The server starts, binds port 4100, accepts connections. `kv-bench` fires 10,000 PUT requests. 10,000 errors. Zero successful operations.

```
Benchmark: 10000 requests, 10 connections, 128 byte values, 0% GETs
Target: 127.0.0.1:4100 (tcp)

No successful requests.
Errors: 10000
```

The server log shows nothing — no errors, no warnings, no leadership messages. The node accepted TCP connections, decoded valid protocol frames, dispatched them to `handlePut`, and every one of them silently failed. The Raft subsystem never elected a leader. Every `Propose` returned `ErrProposalDropped`.

## Reasoning

### Why a single node cannot elect itself

`tickElection` fires `MsgHup` after the randomized election timeout. `hup()` calls `becomePreCandidate()` then `bcastVote()`. `bcastVote` iterates `r.peers` and sends `MsgPreVote` to each one. With no peers, the loop body never executes. No messages are sent. No responses arrive.

In `stepCandidate`, `MsgPreVoteResp` triggers `r.votes[m.From] = !m.Reject` and then calls `voteResult()`. No responses means `r.votes` stays empty. `voteResult()` computes quorum as `(len(r.peers)+1)/2 + 1 = 1`, counts 0 grants, and returns `VotePending`. The node waits for a response that will never come. Next election timeout, same cycle. Forever.

The fix is one line: after `bcastVote`, record `r.votes[r.id] = true`. With 0 peers, quorum is 1, self-vote makes granted = 1 ≥ 1, `VoteWon`, `becomeCandidate`, same self-vote, `becomeLeader`. Done.

But this is the third time a quorum tally has been wrong because self-handling was ad-hoc. 037m's open thread #3 called this out:

> **Unified quorum primitive** — `hasQuorumActive` is the third ad-hoc quorum tally in the codebase (after vote counting and ReadIndex ack counting); a single `VoteResult`-style primitive would collapse all three and is a prerequisite for joint-consensus membership changes.

The one-line fix patches a symptom. The real problem is structural.

### Three tallies, three policies on self

The codebase has three places that answer "did a majority agree?"

| Callsite | What it counts | How it handles self |
|---|---|---|
| `voteResult()` | `r.votes` map | Never included. Broken for single-node. |
| `hasQuorum(acks)` | ReadIndex ack map | Leader adds self explicitly at the callsite before calling. |
| `hasQuorumActive()` | `progress.RecentActive` | Hardcoded `active := 1` at the top of the function. |

Each reconstructs membership from `len(r.peers) + 1`. Each handles self as a special case — differently. The formula `(len(r.peers)+1)/2 + 1` is scattered across all three.

This is not a style problem. It is a correctness surface. Any new code that needs quorum — membership changes, commit-index advancement, configuration transitions — must re-derive the same formula and re-decide how to handle self. Joint consensus requires *two* majority configs that both must agree. If quorum is ad-hoc arithmetic, joint consensus is ad-hoc arithmetic squared.

### What the quorum primitive must own

The primitive needs to own two things: **who votes** and **the decision rule**.

"Who votes" is the voter set — a set of node IDs including self. No caller should compute `len(peers) + 1` or hardcode `active := 1`. The config knows every voter. Self is a voter like any other.

"The decision rule" is: given the voter set and a map of votes cast so far, return Won/Lost/Pending. The caller builds the vote map. The config decides.

etcd's `quorum.MajorityConfig` is exactly this: `map[uint64]struct{}` with a `VoteResult(votes map[uint64]bool) VoteResult` method. The caller doesn't know the quorum threshold. The caller doesn't special-case self. The caller just records votes — including its own — and asks the config.

The three callsites collapse:

**Vote counting** — `hup()` records `votes[r.id] = true` (self-vote), `bcastVote` sends to peers. Each `MsgVoteResp` records `votes[m.From] = !m.Reject`. After each response: `config.VoteResult(votes)`. Single-node: self-vote is the only entry, quorum = 1, `VoteWon` immediately.

**ReadIndex acks** — leader records `acks[r.id] = true` (self-ack), broadcasts heartbeat. Each `MsgHeartbeatResp` records `acks[m.From] = true`. After each ack: `config.VoteResult(acks)`. Single-node: self-ack, quorum = 1, respond immediately.

**CheckQuorum** — the tally builds a map from `progress`: `active[id] = pr.RecentActive`, plus `active[r.id] = true` (leader counts self). Then: `config.VoteResult(active)`. Single-node: self-active, quorum = 1, no-op step-down.

All three become: build `map[uint64]bool`, call `config.VoteResult(votes)`. No special cases at the callsite. Self is just another entry in the map.

### CommittedIndex

etcd's `MajorityConfig` also has `CommittedIndex(l AckedIndexer) Index` — given a lookup from voter ID to its highest acked log index, compute the committed index (the highest index acked by a majority). The current codebase advances `commitIndex` in `stepLeader` with inline arithmetic after every `MsgAppResp`. This is a natural fourth callsite: the config owns "what index has majority support" the same way it owns "what vote has majority support."

This is deferred. The commit-index path is performance-critical (runs on every MsgAppResp), and the current inline code is fast. Wrapping it in the quorum primitive is correct but should be profiled first. The three `VoteResult` callsites are the immediate scope.

### Package shape

A new package `raft/quorum` with three exports:

- `MajorityConfig` — `map[uint64]struct{}`
- `VoteResult` — enum: `VotePending`, `VoteLost`, `VoteWon`
- `MajorityConfig.VoteResult(votes map[uint64]bool) VoteResult` — the decision method

The package is a leaf — it depends on nothing in `raft`, `raftpb`, or `server`. The `raft` package imports it. This direction matters: the quorum primitive is pure arithmetic, no I/O, no state, no messages.

## Spec

### New package: `raft/quorum/quorum.go`

```go
package quorum

type VoteResult uint8

const (
    VotePending VoteResult = 1 + iota
    VoteLost
    VoteWon
)

type MajorityConfig map[uint64]struct{}

func (c MajorityConfig) VoteResult(votes map[uint64]bool) VoteResult {
    if len(c) == 0 {
        return VoteWon
    }
    var granted, missing int
    for id := range c {
        v, ok := votes[id]
        if !ok {
            missing++
            continue
        }
        if v {
            granted++
        }
    }
    q := len(c)/2 + 1
    if granted >= q {
        return VoteWon
    }
    if granted+missing >= q {
        return VotePending
    }
    return VoteLost
}
```

Empty config returns `VoteWon` — the joint-consensus convention. A half-populated joint quorum behaves like the populated half.

### Raft struct changes

Remove `VoteResult` type and constants from `raft.go`. Import from `quorum` package.

Add a `quorumConfig` field to `Raft`:

```go
type Raft struct {
    // ...existing fields...
    config quorum.MajorityConfig // voters = peers ∪ {self}
}
```

Populated during `NewRaft`: `config[r.id] = struct{}{}` plus `config[pid] = struct{}{}` for each peer.

### Callsite rewrites

**`voteResult()`** — delete the method. Replace all callsites with `r.config.VoteResult(r.votes)`.

**`hup()`** — after `bcastVote`, record self-vote:

```go
func (r *Raft) hup(t CampaignType) {
    // ...becomePreCandidate/becomeCandidate...
    r.bcastVote(voteMsgType, term)
    r.votes[r.id] = true  // self-vote
}
```

**`hasQuorum(acks)`** — delete the function. In ReadIndex handler, replace `r.hasQuorum(acks)` with `r.config.VoteResult(acks) == quorum.VoteWon`.

**`hasQuorumActive()`** — delete the function. In CheckQuorum handler, build a vote map from progress and call `r.config.VoteResult(active)`:

```go
case raftpb.MessageType_MsgCheckQuorum:
    active := make(map[uint64]bool)
    active[r.id] = true
    for id, pr := range r.progress {
        active[id] = pr.RecentActive
    }
    if r.config.VoteResult(active) != quorum.VoteWon {
        r.becomeFollower(r.term, None)
    }
```

### ReadIndex self-ack

In the `MsgReadIndex` handler (leader path), when the leader starts a ReadIndex round, record self-ack immediately:

```go
r.readOnly.addRequest(r.commitIndex, m)
r.readOnly.recvAck(r.id, m.Context)  // leader acks own heartbeat
r.bcastHeartbeatWithCtx(m.Context)
```

Single-node: self-ack reaches quorum, ReadIndex resolves immediately with no heartbeat round.

## Minimum tests

**Invariant:** all quorum decisions flow through a single primitive that treats every voter — including self — uniformly, and a single-node cluster reaches quorum on self-vote alone.

1. **Single-voter config wins on self-vote** — a `MajorityConfig{1}` with `votes{1: true}` returns `VoteWon`.
2. **Empty config wins unconditionally** — a `MajorityConfig{}` returns `VoteWon` regardless of the vote map.
3. **Three-voter majority** — config `{1,2,3}`, two grants → `VoteWon`, one grant + one reject → `VotePending`, one grant + two rejects → `VoteLost`.
4. **Missing voters are pending, not lost** — config `{1,2,3}`, only `{1: true}` cast, returns `VotePending` (not `VoteLost`).
5. **Single-node election completes** — a Raft node with no peers transitions from Follower → PreCandidate → Candidate → Leader after one election timeout, without any external messages.
6. **Single-node ReadIndex resolves without heartbeat** — a single-node leader handles `MsgReadIndex` and produces a `ReadState` in the next `Ready`, with no `MsgHeartbeat` in `msgs`.
7. **Single-node CheckQuorum is a no-op** — a single-node leader survives `MsgCheckQuorum` and remains Leader.
8. **Three-node election still works** — a Raft node with two peers becomes leader after receiving majority `MsgVoteResp` grants, confirming the refactor did not break the existing path.

## Open threads

1. **CommittedIndex via quorum primitive** — `stepLeader` computes commit advancement with inline arithmetic after each `MsgAppResp`. Wrapping it in `MajorityConfig.CommittedIndex()` would unify a fourth callsite but is performance-sensitive. Deferred until benchmark data shows whether the abstraction costs anything measurable.
2. **JointConfig** — joint consensus requires two `MajorityConfig`s that both must agree. The quorum package is shaped to support this (`JointConfig = [2]MajorityConfig`, `VoteResult` returns the minimum of the two), but joint consensus itself is deferred to the membership-change episode.

## Exposed bug: nil `readStatec` in `NewRaftHost`

The benchmark exposed a second failure, unrelated to the quorum primitive. After fixing the election, a 3-node integration test (`TestHTTPPutGetIntegration_037j`) still failed: HTTP PUT succeeded, but the subsequent HTTP GET timed out with 504.

**Symptom.** `proposeRead` submitted a `ReadIndex` request. The Raft leader completed the heartbeat quorum round, produced a `ReadState`, and `handleBatch` attempted to forward it to `readStatec`. The send blocked forever. The server's `run()` loop — sitting idle in `select` on the same channel — never received it.

**Root cause.** `NewRaftHost` (the public constructor, used by the real server) did not initialize `readStatec`. The internal constructor `newRaftHost` (used by unit test fakes) did. Sending to a nil channel in Go blocks forever without panic or error.

```go
// NewRaftHost — BEFORE fix
return &raftHost{
    ...
    applyc:     make(chan toApply, 1),
    // readStatec: missing — nil channel
    errc:       make(chan error, 1),
    ...
}
```

Every unit test passed because fakes went through `newRaftHost`. The integration test was the only path that exercised `NewRaftHost` → `handleBatch` → `readStatec` send. ReadIndex was added in 037l, but no integration test exercised the full ReadIndex → ReadState → `readStatec` → `s.run()` path through the public constructor until 037j's HTTP test.

**Fix.** One line: `readStatec: make(chan raft.ReadState, 1)` in `NewRaftHost`, matching `newRaftHost`.

## Flaky integration tests: orphan waiters under CheckQuorum

Running the full server test suite 10 times (`-count=10`) exposed intermittent failures in 4 cluster integration tests: `TestPutAppearsInFollowerSM_036u`, `TestPutAppearsInServerDB_036u`, `TestPutOnFollowerCommitsViaForwarding_037c`, `TestPutBeforeShutdownIsReadableAfterRestart_037e`. All pass consistently in isolation.

**Failure pattern.** The errors are `context deadline exceeded` on `proposePut` (5s `WriteTimeout`) and `follower did not apply entry` (5s polling deadline). Both are the same root cause: a leadership change mid-test causes the proposal to be lost.

**Mechanism.** Under CPU pressure from 10 concurrent test rounds, the 100ms tick interval becomes unreliable. Heartbeat responses arrive late. CheckQuorum (037m) demotes the leader after an election timeout (10 ticks = 1s nominal, longer under load). The cluster re-elects. A proposal that was in-flight during the demotion has no leader to commit it. The waiter — registered via `s.w.Register(id)` — is never triggered. It hangs until the handler's 5s `WriteTimeout` expires.

**This is the orphan-waiter problem** from 036q open thread #3: "If a leader proposes, then loses leadership before the entry commits, the waiter hangs until the handler's context times out." Not introduced by the 038 refactor. Pre-existing.
