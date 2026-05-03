# 039 — The Self-Ack

Continuing the benchmark sabotage from 038. Election now works — a single-node cluster self-elects via the unified quorum primitive. But `kv-bench` still produces 0 successful requests. The server accepts the PUT, proposes it to Raft, and the leader appends the entry. Then nothing. No commit, no apply, no response. The 5-second `WriteTimeout` expires on every request.

```
Benchmark: 1000 requests, 1 connections, 128 byte values, 0% GETs
Target: 127.0.0.1:4100 (tcp)

No successful requests.
Errors: 1000
```

Server log with `-debug`:

```
level=WARN msg="matchTerm: storage.Term failed" commitIndex=0 error="requested index is unavailable"
level=ERROR msg="failed to process the request" cmd=2 error="context deadline exceeded"
```

`commitIndex=0`. The leader appended a no-op at index 1 during `becomeLeader` and the PUT at index 2, but committed neither. `maybeCommit` is only called inside the `MsgAppResp` handler — and no `MsgAppResp` ever arrives because there are no peers.

## Reasoning

### Why no commit

The commit path in `stepLeader` is triggered by `MsgAppResp`. A peer receives `MsgApp`, persists the entries, and replies with `MsgAppResp{Index: lastIndex}`. The leader updates `progress[peer].MatchIndex` and calls `maybeCommit(getMedian())`. The median of all match indices (peers + leader's `lastLogIndex`) determines the commit point.

For a single-node cluster, `r.peers` is empty. `bcastAppend()` iterates peers — does nothing. No `MsgAppResp` ever arrives. `maybeCommit` is never called. The entry sits in the log forever.

The leader appends entries but never votes on its own log. It's like an election where the candidate never votes for itself.

### Naive fix #1: inline commit

The simplest patch: after `appendEntry` + `bcastAppend` in `stepLeader.MsgProp`, call `maybeCommit(getMedian())`. For a single node, `getMedian()` returns `lastLogIndex` (leader's own match, the only entry in the matches slice). Quorum of 1, entry commits immediately.

This fixes the symptom. But `maybeCommit(i)` calls `matchTerm(i)` which calls `r.storage.Term(i)`. The entry was just appended to `r.log` (in-memory) — it hasn't been persisted to storage yet. `storage.Term(i)` returns `ErrUnavailable`.

### Naive fix #2: scan in-memory log

To fix the `matchTerm` failure, make it check `r.log` before falling back to storage. The entry is in memory, the term matches, commit advances.

Now both fixes together work: the entry commits, appears in `CommittedEntries` in the next Ready, `handleBatch` persists and applies it, the client gets a response.

But something is wrong. The entry committed *before* it was persisted. The Ready contract is:

1. `Ready()` produces entries to persist, messages to send, committed entries to apply.
2. The caller persists, sends, applies.
3. `Advance()` tells Raft the batch is done. `stableIndex` advances.

With inline `maybeCommit`, the entry commits at step 0 — during `Step(MsgProp)` — before the caller has seen the Ready. The next `Ready()` includes the entry in *both* `Entries` (to persist) and `CommittedEntries` (to apply). The caller does persist before apply in `handleBatch`, so the entry reaches disk before the state machine. But Raft's internal state already marked it committed. If the process crashes between `Step(MsgProp)` and `handleBatch`, the entry is committed but lost.

The crash window is microseconds. The probability is near zero. But the invariant is real: **commit must not precede persist.** A peer's `MsgAppResp` is proof of durability — "I persisted your entries up to this index." The leader's own match should carry the same guarantee: "I persisted my entries up to this index." But `lastLogIndex` is the in-memory tip, not the durable tip. The leader claims durability it hasn't earned.

### The code smell

Two patches stacked on top of each other. The inline commit needed the `matchTerm` hack because it fired before persistence. The `matchTerm` hack exists only because commit fires too early.

A third problem surfaces once commit works: the no-op entry from `becomeLeader` has `nil` data, and `unmarshalEnvelope(nil)` fails. Skipping no-ops in `applyEntry` is correct regardless of how commit works — no-ops are Raft-internal, not client requests. It was just hidden until the naive fixes unblocked the commit path.

The root cause is one thing: **the leader never acks its own entries after persistence.** Every peer acks via `MsgAppResp` — proof of durability. The leader never sends this signal to itself. The three patches work around the absence of self-ack. The question is how a proper self-ack should work.

### What "ack" means

For a peer, `MsgAppResp{Index: i}` means: "I have durably persisted entries up to index `i`." The leader uses this to update `progress[peer].MatchIndex` and recompute the commit point.

The leader's own equivalent: "I have durably persisted entries up to `stableIndex`." `stableIndex` advances in `Advance()`, which is called after `handleBatch` persists entries. That's the persistence boundary. The self-ack should fire *at or after* `Advance`, not during `Step`.

### Where to deliver the self-ack

The self-ack is a `MsgAppResp` from the leader to itself. Two places we tried:

**Inside `appendEntry`.** The leader queues `MsgAppResp{To: r.id, Index: lastLogIndex}` immediately after appending. But this fires before persistence — the same premature-ack problem.

**After `Advance` in the node loop.** The node loop calls `r.Advance()`, then steps a self-ack message back into the raft state machine. Entries are now persisted. The self-ack goes through `stepLeader.MsgAppResp` — the normal handler. But the leader doesn't track itself in `r.progress`. The handler looks up `prs := r.progress[m.From]` and fails.

Both dead ends. Reading etcd's `send()` reveals a third path we hadn't considered.

### How etcd solves it: two message queues

etcd's `send()` doesn't put all messages in one outbox. It routes them into two queues based on what the message *promises*:

- `r.msgs` — sent immediately (before persistence). `MsgApp`, `MsgHeartbeat`, `MsgProp` — messages that don't claim durability.
- `r.msgsAfterAppend` — sent after persistence. `MsgAppResp`, `MsgVoteResp`, `MsgPreVoteResp` — any response that constitutes a vote on durability.

The split isn't about self vs peer. It's about **what the message promises**. A `MsgAppResp` says "I persisted this." Sending it before persistence is a lie — whether the sender is a peer or the leader itself. All ack/vote responses go to the deferred queue. The caller delivers them after `handleBatch` persists.

etcd's `appendEntry` sends `MsgAppResp{To: r.id}` — same as our first attempt. But it doesn't go to `r.msgs`. It goes to `r.msgsAfterAppend`, because `send()` routes all `MsgAppResp` there. The self-ack is queued, not delivered.

For self-addressed messages (`m.To == r.id`), the delivery doesn't go through the network. Instead, etcd's `acceptReady` collects them into a `stepsOnAdvance` list. When the caller calls `Advance()`, the node loop steps each deferred message back into the raft state machine. The self-`MsgAppResp` reaches `stepLeader`, updates progress, calls `maybeCommit`. For a single node, this is the only ack — quorum of 1, commit advances. For a multi-node cluster, the leader's ack adds one vote to the quorum alongside peer acks.

Deferring `MsgAppResp` also applies to followers. A follower's `handleAppendEntries` produces `MsgAppResp` which goes to `msgsAfterAppend`, then into `rd.Messages` for network delivery after the follower persists. This adds one Ready cycle to follower ack latency compared to sending immediately. The cost is real — commit latency becomes `persist_leader + network + persist_follower + ready_cycle_follower` — but correctness demands it: a follower's `MsgAppResp` is a durability promise, and sending it before persistence is the same lie the leader would tell with inline commit.

`MsgPreVoteResp` does not set `votedFor` (PreVote is non-binding by design, 037n), so it is strictly safe to send immediately. We defer it anyway, following etcd's conservative convention — the cost is negligible (one extra Ready cycle per election) and formal safety of the immediate path has not been verified.

### Self in progress and the ProgressTracker

But the self-ack path requires the leader to track itself in `r.progress`. The `MsgAppResp` handler looks up `r.progress[m.From]`. If the leader isn't tracked, the lookup fails. And if we just add self to the progress map, every loop over progress must decide: am I iterating voters or peers? The codebase already gets this wrong in three places.

`bcastAppend` and `bcastHeartbeat` iterate `r.peers` (peers to message). `getMedian` and `hasQuorumActive` iterate `r.progress` (voters). But `getMedian` hardcodes `r.lastLogIndex` as the leader's match instead of reading it from progress. `hasQuorumActive` hardcodes `active := 1` for self instead of reading from progress. Each function reimplements the voter-vs-peer distinction ad hoc.

etcd solves this with a `tracker` package that owns both the progress map and the quorum config. It exposes:

- `Committed()` — computes the commit point from match indices via `MajorityConfig.CommittedIndex()`. Replaces `getMedian`.
- `QuorumActive()` — checks if a majority of voters are recently active. Replaces `hasQuorumActive`.
- `IsSingleton()` — returns true if the progress map contains only self. Replaces `singletonCluster`.
- `Visit(func(id uint64, pr *Progress))` — iterates all progress entries in deterministic order. Replaces raw loops over `r.progress` and `r.peers`.

We already have all these paths scattered across the Raft struct. 039 is touching every one of them — adding self to progress, changing `getMedian` to iterate progress, changing `hasQuorumActive` to include self from progress. Doing this without the ProgressTracker means touching every callsite now and again later when the ProgressTracker arrives.

The ProgressTracker scope is bounded: a struct with `progress map[uint64]*Progress` + `config quorum.MajorityConfig` + `selfID uint64`, exposing the four methods above. No new behavior — just consolidating existing logic behind a named boundary. It also closes 038 open thread #1 (CommittedIndex via MajorityConfig) because `Committed()` uses `MajorityConfig.CommittedIndex()` directly, and prepares for 038 open thread #2 (JointConfig) because the ProgressTracker can hold `[2]MajorityConfig` when that episode arrives.

## Spec

Four layers: (1) new `raft/tracker` package consolidating progress + config + quorum logic, (2) Raft struct replaces `progress`/`config`/`peers` with `trk`, splits message queues, adds self-ack, (3) node loop defers self-addressed messages to post-Advance, (4) server skips no-ops in apply.

### Raft struct — new fields

```go
type Raft struct {
    // ...existing...
    msgsAfterAppend []*raftpb.Message // deferred until persistence
}
```

### `send()` — route ack/vote responses to deferred queue

```go
func (r *Raft) send(m *raftpb.Message) {
    // ...existing From/Term logic...
    switch m.Type {
    case raftpb.MessageType_MsgAppResp,
         raftpb.MessageType_MsgVoteResp,
         raftpb.MessageType_MsgPreVoteResp:
        r.msgsAfterAppend = append(r.msgsAfterAppend, m)
    default:
        r.messages = append(r.messages, m)
    }
}
```

### `appendEntry` — self-ack

```go
func (r *Raft) appendEntry(data []byte) {
    prev := r.lastLog()
    e := &raftpb.Entry{Index: prev.Index + 1, Term: r.term, Data: data}
    r.appendEntries([]*raftpb.Entry{e})
    r.send(&raftpb.Message{To: r.id, Type: raftpb.MessageType_MsgAppResp, Index: r.lastLogIndex})
}
```

### ProgressTracker package: `raft/tracker/tracker.go`

```go
package tracker

type ProgressTracker struct {
    selfID   uint64
    Voters   quorum.MajorityConfig
    progress map[uint64]*Progress
    votes    map[uint64]bool
}

func NewProgressTracker(selfID uint64, voters quorum.MajorityConfig) *ProgressTracker {
    t := &ProgressTracker{
        selfID:   selfID,
        Voters:   voters,
        progress: make(map[uint64]*Progress),
        votes:    make(map[uint64]bool),
    }
    t.progress[selfID] = &Progress{}
    return t
}

func (t *ProgressTracker) InitProgress(id uint64, match, next uint64) {
    t.progress[id] = &Progress{MatchIndex: match, NextIndex: next}
}

func (t *ProgressTracker) ResetVotes() {
    t.votes = make(map[uint64]bool)
}

func (t *ProgressTracker) RecordVote(id uint64, v bool) {
    t.votes[id] = v
}

func (t *ProgressTracker) TallyVotes() quorum.VoteResult {
    return t.Voters.VoteResult(t.votes)
}

func (t *ProgressTracker) Committed() uint64 {
    return t.Voters.CommittedIndex(matchAckIndexer(t.progress))
}

func (t *ProgressTracker) QuorumActive() bool {
    votes := make(map[uint64]bool)
    t.Visit(func(id uint64, pr *Progress) {
        votes[id] = pr.RecentActive
    })
    return t.Voters.VoteResult(votes) == quorum.VoteWon
}

func (t *ProgressTracker) IsSingleton() bool {
    return len(t.progress) == 1
}

func (t *ProgressTracker) Progress(id uint64) *Progress {
    return t.progress[id]
}

func (t *ProgressTracker) Visit(f func(id uint64, pr *Progress)) {
    for id, pr := range t.progress {
        f(id, pr)
    }
}
```

`matchAckIndexer` adapts the progress map for `MajorityConfig.CommittedIndex()`:

```go
type matchAckIndexer map[uint64]*Progress

func (m matchAckIndexer) AckedIndex(id uint64) (uint64, bool) {
    pr, ok := m[id]
    if !ok { return 0, false }
    return pr.MatchIndex, true
}
```

### `AckedIndexer` and `CommittedIndex` — quorum package

```go
type AckedIndexer interface {
    AckedIndex(voterID uint64) (idx uint64, found bool)
}
```

```go
func (c MajorityConfig) CommittedIndex(l AckedIndexer) uint64 {
    n := len(c)
    if n == 0 { return math.MaxUint64 }
    srt := make([]uint64, n)
    i := n - 1
    for id := range c {
        if idx, ok := l.AckedIndex(id); ok {
            srt[i] = idx
            i--
        }
    }
    sort.Slice(srt, func(a, b int) bool { return srt[a] < srt[b] })
    pos := n - (n/2 + 1)
    return srt[pos]
}
```

### Raft struct — replace progress/config/peers with ProgressTracker

Remove `progress map[uint64]*Progress`, `config quorum.MajorityConfig`, `peers []uint64`, and `votes map[uint64]bool` from the Raft struct.

```go
type Raft struct {
    // ...existing...
    trk *tracker.ProgressTracker
    msgsAfterAppend []*raftpb.Message
}
```

### `newRaft` — create ProgressTracker once

```go
r.trk = tracker.NewProgressTracker(r.id, buildMajorityConfig(cfg.ID, peers))
for _, pid := range peers {
    r.trk.InitProgress(pid, 0, 1)
}
```

### `reset()`

```go
func (r *Raft) reset(term uint64) {
    // ...existing term/votedFor/election logic...
    r.trk.ResetVotes()
	r.trk.Visit(func(id uint64, pr *tracker.Progress) {
		pr.MatchIndex = 0
		pr.NextIndex = r.lastLogIndex + 1
		pr.RecentActive = false
		if id == r.id {
			pr.MatchIndex = r.lastLogIndex
		}
	})
}
```

### `becomeLeader`

```go
func (r *Raft) becomeLeader() {
    r.state = Leader
    r.step = stepLeader
    r.tick = r.tickHeartbeat
    r.reset(r.term)
    r.lead = r.id
    r.trk.Progress(r.id).RecentActive = true
    r.appendEntry(nil)
    r.bcastAppend()
}
```

### `stepLeader` callsite changes

- `r.progress[m.From]` → `r.trk.Progress(m.From)`
- `r.getMedian()` → `r.trk.Committed()`
- `r.hasQuorumActive()` → `r.trk.QuorumActive()`
- `r.singletonCluster()` → `r.trk.IsSingleton()`
- `r.config.VoteResult(r.votes)` → `r.trk.TallyVotes()`
- `r.config.VoteResult(acks)` → `r.trk.Voters.VoteResult(acks)`
- `r.votes[id] = v` → `r.trk.RecordVote(id, v)`

### `stepLeader.MsgAppResp`

No special case needed. Self is in the ProgressTracker. The self-ack flows through the same handler as any peer ack.

### `bcastAppend`, `bcastHeartbeat`

Replace `r.peers` iteration with `r.trk.Visit()`, skipping self:

```go
func (r *Raft) bcastAppend() {
    r.trk.Visit(func(id uint64, pr *tracker.Progress) {
        if id == r.id { return }
        r.sendAppend(id)
    })
}
```

### Node loop — `stepsOnAdvance`

```go
type node struct {
    // ...existing...
    stepsOnAdvance []*raftpb.Message
}
```

In `acceptReady` (during `readyc <- rd`):

```go
case readyc <- rd:
    readyc = nil
    advancec = n.advancec
    for _, m := range n.r.msgsAfterAppend {
        if m.To == n.r.id {
            n.stepsOnAdvance = append(n.stepsOnAdvance, m)
        }
    }
```

In `Advance`:

```go
case <-advancec:
    if !IsEmptyHardState(rd.HardState) {
        n.prevHard = rd.HardState
    }
    n.r.Advance()
    for _, m := range n.stepsOnAdvance {
        _ = n.r.Step(m)
    }
    n.stepsOnAdvance = n.stepsOnAdvance[:0]
    rd = Ready{}
    advancec = nil
```

### `Advance()` — clear deferred queue

```go
func (r *Raft) Advance() {
    r.stableIndex = r.lastLogIndex
    r.appliedIndex = r.commitIndex
    r.messages = r.messages[:0]
    r.msgsAfterAppend = r.msgsAfterAppend[:0]
    r.readStates = r.readStates[:0]
}
```

### `applyEntry` — skip no-ops

```go
func (s *Server) applyEntry(raw []byte) {
    if len(raw) == 0 {
        return // no-op entry (Raft-internal, not a client request)
    }
    // ...existing envelope decode + dispatch...
}
```

## Minimum tests

**Invariant:** the leader commits entries only after persistence, all quorum computations flow through the ProgressTracker with self as a first-class voter, and durability-promising messages are deferred until after persistence.

1. **Single-node commits after Advance** — a single-node leader proposes an entry; `Ready()` includes it in `Entries` but not `CommittedEntries`. After `Advance()`, the next `Ready()` includes it in `CommittedEntries`.
2. **Single-node no-op commits after Advance** — the no-op from `becomeLeader` appears in `CommittedEntries` only after the first `Advance()` cycle.
3. **CommittedEntries empty before Advance** — after proposing, `Ready().CommittedEntries` is empty. Commit has not fired yet.
4. **Multi-node commit requires peer ack** — a 3-node leader proposes; after `Advance()` alone, commit does not advance (self-ack is 1 of 3). Commit advances only after a peer's `MsgAppResp`.
5. **Self in ProgressTracker** — after `becomeLeader`, `trk.Progress(r.id)` exists with `MatchIndex` at the leader's last log index.
6. **Self-ack updates ProgressTracker** — after `Advance()`, `trk.Progress(r.id).MatchIndex` advances to `stableIndex`.
7. **ProgressTracker.Committed** — `trk.Committed()` returns the commit point from the progress map via `MajorityConfig.CommittedIndex()`, with no hardcoded leader match.
8. **No-op skip in apply** — an entry with empty data does not reach `unmarshalEnvelope`.
9. **MsgAppResp routed to msgsAfterAppend** — a follower's `handleAppendEntries` produces `MsgAppResp`; it appears in `msgsAfterAppend`, not `messages`.
10. **bcastAppend skips self** — `MsgApp` messages are sent only to non-self entries via `trk.Visit()`.
11. **ProgressTracker.QuorumActive** — CheckQuorum uses `trk.QuorumActive()` which reads `RecentActive` from the progress map including self.

## Open threads

1. **Concurrent connections** — `kv-bench -c 5` causes EOF on all workers after the first request. Discovered while validating the self-ack fix. Root cause unknown. Deferred to next episode.
2. **JointConfig** — the ProgressTracker holds a single `MajorityConfig`. Joint consensus requires `[2]MajorityConfig` where both must agree. The ProgressTracker is shaped to support this. Deferred to the membership-change episode.
3. **Deferred vote-response timing** — `MsgVoteResp` and `MsgPreVoteResp` now go to the deferred queue. Existing tests may need adjustment if they observe vote responses in `Ready.Messages` before `Advance`.
