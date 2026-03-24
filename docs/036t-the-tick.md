# 036t — The Tick

We tried to wire the Raft stack into the server. The first test: start three nodes, wait for a leader. It can't pass. The Raft core has no clock — `Campaign()` exists, but nothing calls it. A follower waits forever because no timeout fires. Leaders never send heartbeats because there's no heartbeat interval. The algorithm is correct but inert.

The Raft paper requires two timing behaviors: followers start elections if they hear no heartbeat within an **election timeout**, and leaders send periodic **heartbeats** to prevent unnecessary elections (§5.2). It specifies these as real-time durations but says nothing about how to track them.

etcd's insight is to replace wall-clock timers with a logical counter: the application calls `Tick()` at a fixed interval, the algorithm counts ticks, and fires elections or heartbeats when counters expire. This keeps the algorithm free of `time.After`, goroutines, and real clocks — a pure state machine. kv-go adopts the same approach. Without it, the cluster is a set of nodes that know the protocol but never act on it.

## Boundary

Add a tick-driven clock to the Raft core so elections and heartbeats fire automatically. Refactor the existing `Step` into per-role dispatch to support both ticking and stepping by role.

In scope:
- Tick mechanism — followers/candidates detect election timeout, leaders send periodic heartbeats
- Per-role dispatch — `Step` dispatch refactored by role, `Tick` dispatch introduced by role, without per-message state checks
- Propose refactoring — introduce `MsgProp` and move proposal logic from `Propose()` into `stepLeader`; `Propose()` becomes a thin wrapper that feeds `MsgProp` through `Step`, making `Step` the single entry point for all state transitions
- State transition functions — explicit `becomeFollower`, `becomeCandidate`, `becomeLeader`
- Internal message types — `MsgHup`, `MsgBeat`, `MsgProp` — local signals that never cross the wire
- Local message safety — prevent injection of internal signals from the network
- Node-level tick channel — application pushes ticks into the event loop
- Timeout configuration — election and heartbeat timeouts in logical tick units
- Constructor state restore — `newRaft` calls `becomeFollower` as its final step; loading persisted `HardState` from storage is causal — the restored term feeds the transition

Deferred:
- Server-level ticker goroutine (the application provides the clock)
- Pre-vote (the current election path is direct vote, no pre-vote in the Raft core)

Out of scope:
- Wiring the full stack into the server
- Check quorum / leader lease

## Design

### The clock problem

The current `Step()` is a monolithic switch over message types. Every case checks `r.state` to decide whether it applies. That works when messages arrive from outside, but a clock is different — the *behavior* of a tick depends on the role. A follower tick checks "has it been too long since I heard from a leader?" A leader tick checks "is it time to remind followers I'm alive?" These are not two branches of one function; they are two different functions that happen to share a name.

etcd solves this with a function pointer: `tick func()`. A follower stores `tickElection`; a leader stores `tickHeartbeat`. Calling `r.tick()` does the right thing without checking state. Reading etcd's source showed the same pattern on `step`: a per-role function pointer (`stepFollower`, `stepCandidate`, `stepLeader`) called after a shared preamble. Both pointers swap at the same moments — state transitions. The function pointer *is* the role.

kv-go adopts the same two fields on `Raft`:

- `tick func()` — `tickElection` for followers/candidates, `tickHeartbeat` for leaders
- `step func(m *raftpb.Message) error` — `stepFollower`, `stepCandidate`, `stepLeader`

`Step()` remains the public entry point. It runs the shared preamble — higher-term authority (if `m.Term > r.term`, step down) and vote requests (`MsgVote`) — then dispatches to `r.step(m)`. The per-role functions handle only their messages: `stepFollower` handles `MsgApp` and `MsgSnap`, `stepLeader` handles `MsgAppResp`, `stepCandidate` handles `MsgVoteResp`. (The internal message types introduced later add to `stepLeader`'s responsibilities.)

### Making state transitions explicit

Today, state transitions scatter assignments across several call sites. With two function pointers that must swap together, inline assignment becomes dangerous — forget one pointer and the node ticks as a follower while stepping as a candidate.

The fix is explicit transition functions:

- `becomeFollower(term, lead)` — sets state, assigns both pointers (`stepFollower`, `tickElection`), resets `electionElapsed`. The `lead` parameter is new: currently nothing tracks who the leader is, but followers need it to know whose `MsgApp` resets their election timer vs. a stale leader's.
- `becomeCandidate()` — sets state, increments term, assigns both pointers (`stepCandidate`, `tickElection`), resets `electionElapsed`, votes for self.
- `becomeLeader()` — sets state, assigns both pointers (`stepLeader`, `tickHeartbeat`), resets `heartbeatElapsed`, initializes progress.

`Campaign()` sends `MsgHup` through `Step()`. The higher-term preamble in `Step()` calls `becomeFollower()`. Every state change flows through `Step()` — if the pointers are correct in the transition functions, they're correct everywhere.

This also introduces the `lead` field on `Raft`. It doesn't exist today. Followers set it when they accept a leader via `becomeFollower(term, lead)` or when they receive a valid `MsgApp`. Leaders set it to their own ID. Candidates clear it.

### The internal message loop

The tick functions need to trigger behavior: `tickElection` must start a campaign, `tickHeartbeat` must broadcast to peers. The naive path is to call `Campaign()` or build messages directly. Reading etcd showed why that's wrong.

etcd's tick functions generate a *message* and feed it back through `Step()`:

- `tickElection` → sends `MsgHup` to `r.Step()`
- `tickHeartbeat` → sends `MsgBeat` to `r.Step()`

This preserves a single entry point for all state mutations. Every transition, whether triggered by a peer message or a local timer, flows through `Step()`. The tick function is a message *generator*, not a state *mutator*. This matters for testing — you can test the campaign path by injecting `MsgHup` directly without needing a clock, and the behavior is identical.

Three new message types are added to the proto enum: `MsgHup`, `MsgBeat`, and `MsgProp`. None cross the wire. They exist only as internal signals — `MsgHup` and `MsgBeat` from the tick layer, `MsgProp` from the propose path.

`tickElection` increments `electionElapsed`. When it reaches `randomizedElectionTimeout`, it sends `MsgHup` — `Step()` then runs the campaign logic. The randomized timeout is chosen from `[electionTimeout, 2 * electionTimeout)` at each reset to prevent split-vote loops (§5.2).

`tickHeartbeat` increments `heartbeatElapsed`. When it reaches `heartbeatTimeout`, it sends `MsgBeat` — `stepLeader` then broadcasts `MsgApp` to all peers and resets the counter.

Counter resets:
- `electionElapsed` → zero on valid `MsgApp` from current leader, on becoming candidate, on becoming follower
- `heartbeatElapsed` → zero on becoming leader, on sending heartbeat

### Guarding the boundary

`MsgHup`, `MsgBeat`, and `MsgProp` must never arrive from the network. A remote peer sending `MsgHup` could force an election on another node — that's a safety violation. etcd guards this with a type-based classifier: `IsLocalMsg(m.Type)` checks the message type against a known local set.

Two layers enforce locality:

1. **By construction** — tick functions call `r.Step()` directly. The message never enters the outbound `messages` slice that `Ready` exposes. No transport ever sees it.
2. **By type guard** — `Node.Step()` (the network-facing entry point) rejects local message types. If `IsLocalMsg(m.Type)` is true and the message came from outside, it returns an error.

kv-go adopts both layers. Currently `messages []*raftpb.Message` is the outbound slice. The guard goes on `Node.Step()`, not on `Raft.Step()`, because `Raft.Step()` must accept `MsgHup` from the tick layer.

### Crossing the goroutine boundary

The raft struct is pure — no goroutines, no locks. 036b established that all mutations happen inside the single-goroutine `run()` loop, and external callers serialize through channels. `Tick()` follows the same pattern: `Node` gains a `tickc` channel and a public `Tick()` method.

But there's a pressure that proposals don't have. A proposal failing to send means the caller retries or surfaces an error. A tick failing to send is different — the application's `time.Ticker` fires regardless, and there's no meaningful retry. If the `run()` loop is busy processing a large Ready and the tick channel fills up, blocking the caller would stall the application's event loop.

etcd's solution: buffered channel (`make(chan struct{}, 128)`), non-blocking send. If the buffer is full, the tick is dropped with a log warning. Dropped ticks are safe — election and heartbeat timeouts shift slightly, but the algorithm is designed to tolerate timing variation. This is why randomized timeouts exist.

`run()` selects on `tickc` alongside the existing channels and calls `r.Tick()`. A tick is just another event.

### Propose through Step

`Propose()` currently owns the proposal logic directly: it appends an entry, builds `MsgApp` messages, and writes them to the outbound slice. This bypasses `Step()`, breaking the single-entry-point invariant. The fix is a new local message type, `MsgProp`. `Propose()` becomes a thin wrapper that feeds a `MsgProp` into `Step()`. The shared preamble passes it to `r.step(m)`, and `stepLeader` handles the actual append and broadcast. Non-leaders ignore `MsgProp`. This completes the pattern: `Campaign()` sends `MsgHup`, `tickElection` sends `MsgHup`, `tickHeartbeat` sends `MsgBeat`, `Propose()` sends `MsgProp` — all through `Step()`.

### Configuration

`Config` gains two fields:
- `ElectionTick int` — base election timeout in ticks (e.g. 10)
- `HeartbeatTick int` — heartbeat interval in ticks (e.g. 1)

The actual tick duration (e.g. 100ms) is the application's choice, not the algorithm's. The paper specifies real-time durations; the logical counter translates them into testable units.

### Constructor state restore

`newRaft` must initialize from persisted state before starting the first term. etcd's `newRaft` calls `c.Storage.InitialState()` to read the last durable `HardState`, sets `r.Term`, `r.Vote`, and `r.raftLog.committed` from it, then calls `becomeFollower(r.Term, None)`. Because `reset(term)` only clears `Vote` when the term changes, the restored vote survives — the node remembers who it voted for.

kv-go mirrors this: `newRaft` calls `cfg.Storage.InitialState()`, loads `term`, `votedFor`, and `commitIndex` from the returned `HardState` (if non-empty), then calls `becomeFollower(r.term, None)`. A fresh node has an empty `HardState`, so the term stays 0 and the follower starts clean.

## Minimum tests

**Invariant:** a Raft cluster with ticking nodes elects a leader without any external trigger.

1. **tickElection produces MsgHup** — tick a follower past `randomizedElectionTimeout`; it must generate a `MsgHup` that flows through `Step()` and triggers a campaign. Proves the internal message loop: tick generates, Step mutates.
2. **tickHeartbeat produces MsgBeat** — tick a leader past `heartbeatTimeout`; `stepLeader` must convert the `MsgBeat` into `MsgApp` messages to all peers. Proves the same loop from the leader side.
3. **Candidate re-elects at higher term on timeout** — a candidate whose election times out must re-campaign at a higher term so peers who already voted can vote again. Proves `becomeCandidate()` always runs on `MsgHup`, incrementing the term.
4. **Node.Step rejects local messages** — call `Node.Step()` with a `MsgHup` from an external source; it must return an error. Proves the type guard boundary.
5. **Follower resets timer on leader contact** — a follower that receives `MsgApp` from the current leader must not campaign even after many ticks, as long as heartbeats keep arriving. Proves counter reset correctness.
6. **Three-node cluster self-elects** — create three `Raft` nodes, tick them repeatedly, deliver messages in-memory; one must reach leader state within a bounded number of ticks. Proves the full loop end-to-end: tick → MsgHup → campaign → votes → leader → MsgBeat → heartbeats.
7. **Split-vote recovery** — if two candidates split the vote, randomized timeouts must break the tie within a bounded number of rounds. Proves the randomized timeout prevents livelock.
8. **Constructor restores persisted state** — create a `Raft` with a `Storage` that returns a non-empty `HardState`; the raft must start with the restored term, votedFor, and commitIndex. Proves the `InitialState` loading path.
9. **Step refactor preserves existing behavior** — all existing 036b–036s tests must pass unchanged. The existing suite is the regression test for the step split.

## Open threads

1. **Pre-vote** — the Raft core doesn't have pre-vote. A partitioned node that campaigns will increment its term and disrupt the cluster on rejoin. Not blocking for 036t but important for correctness.
2. **Check quorum** — a leader that can't reach a majority should step down. Currently not implemented.
