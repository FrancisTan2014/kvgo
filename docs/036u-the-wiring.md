# 036u - The Wiring

The Raft model from 036t is fully tested in isolation — deterministic ticks, channel-based transport, in-memory storage. None of it touches a real network or a real server. A proposal on the leader never reaches followers over TCP. Without wiring, the model is a simulation, not a consensus layer.

## Boundary

036u wires `RaftHost` and `RaftTransport` to the server, bringing the Raft model across the in-process boundary to the real network.

In scope:
- Tick mechanism — the cluster elects a leader without outside trigger
- Transport — messages reach followers through the network
- Quorum — proposals achieve majority consensus through Raft

Deferred:
- Old path cleanup — future episodes rewrite GET, election, etc., then remove the Redis-like code paths
- GET through Raft

Out of scope:
- Graceful shutdown ordering (open thread)
- TLS, snapshot routing, peer validation on the transport accept path

## Design

### Why `sm` and `db` coexist

It is attractive to fold `server.sm` and `server.db` into one. But the old Redis-like paths need `engine.DB` APIs like `Clear` and `Range` that `StateMachine` does not provide. Adding them to `StateMachine` would blur a proved boundary for no gain — `Clear` is a full-resync concern, not a consensus concern.

So in 036u we keep both: `server.sm` is wired as `server.db` (same engine underneath), the Raft apply loop writes through `sm`, and the old path writes through `db`. `server.db` gets removed only when all old paths have been rewritten through Raft.

### The node ID

`NewRaftHost` needs a node ID. Generating one locally is rejected immediately — without coordination, nothing guarantees cluster-wide uniqueness. This is a coordination concern: the bootstrap layer sees both the cluster topology and the node identity, so it owns ID assignment (the same principle from 036o — coordination belongs where both ends are visible). The ID comes from whoever creates `Server`, passed through `Options`.

### The context chain

Wiring `RaftHost` into the server exposed a `context.Context` misuse that runs through two layers of the 036 series:

- **`raft.node`** — `NewNode` launches `go n.run(ctx)`, but `ctx` is never defined. It compiles only because the call site has not been exercised. The `run` loop selects on `ctx.Done()` for shutdown.
- **`raftHost`** — stores `ctx` and `cancel` as struct fields, but neither constructor initializes them. `start()` selects on `r.ctx.Done()`; `Stop()` calls `r.cancel()`. Both are `nil` at runtime.

The surface bug is timing: `server.ctx` does not exist until `Start()` is called, so constructors cannot receive it. The `context` package docs confirm this is correct:

> Do not store Contexts inside a struct type; instead, pass a Context explicitly to each function that needs it.

But the deeper issue is ownership. Both `node` and `RaftHost` already name their lifecycle authority through `Start`/`Stop`. A parent context adds a second, hidden shutdown path for the same thing. Canceling a parent context assumes all children can stop atomically and immediately — that is `os.Exit(1)` with extra steps. Real shutdown is ordered: drain in-flight proposals, flush the apply loop, close storage.

Context and `CancellationToken` (C#) share the same philosophy: they are per-request and per-operation tools. Using them as lifecycle controllers conflates two concerns — "cancel this request" and "shut down this component."

In 036u we fix both layers: remove `ctx` from `node.run` and `raftHost` struct, replace `ctx.Done()` with a `stopc chan struct{}` closed by `Stop()`. Per-operation context on `Propose`, `Step`, and `Campaign` stays — that is the correct use.

### The listener ownership

`initializeRaftHost` hardcodes `ListenAddr` from `Options.Host` and `Options.RaftPort`. The test cluster has pre-bound raft listeners (`:0`) but no way to pass them through to the transport.

The fix is listener injection: `Options` gains a `RaftListener net.Listener` field. When set, `initializeRaftHost` passes it as `RaftTransportConfig.Listener` — the transport already respects this and skips `net.Listen`. The transport keeps full ownership of accept, connection tracking, and shutdown. No ownership boundary moves.

The deeper question — whether the server should own the accept loop and hand connections via `HandleConn` (the etcd pattern) — is deferred. That split involves connection lifecycle concerns (TLS, snapshot routing, peer validation, shutdown ordering) that 036u does not need to answer.

### The follower commit-index

`stepFollower` returned early on heartbeats (MsgApp with no entries) before checking `m.Commit`. The leader updates its commit index after quorum, then carries it in every subsequent MsgApp — including heartbeats. But the early return discarded it, so followers never advanced their commit index and never applied entries.

The fix: move the commit-index update before the empty-entries check. Every MsgApp — with or without entries — advances the follower's commit index if the leader's is higher.

### The leader identity

The server needs to know who the current leader is — write routing depends on it. The `lead` field lives on the `Raft` struct, which is owned by the node's goroutine. Reading it directly from outside is a data race.

The first attempt added a `LeaderID()` method to the `Node` interface, backed by a channel in the select loop (same pattern as `Propose`/`Step`/`Campaign`). But a dedicated always-sending channel for one field fires every select iteration even when nobody asks. That's the wrong abstraction.

etcd solves this by piggybacking on `Ready`: `SoftState` carries the current leader, and the consumer caches it. We follow the same pattern: `Ready` gains a `Lead` field populated from `r.lead` on every ready cycle, `raftHost` stores it in an `atomic.Uint64` when processing each batch, and `raftHost.LeaderID()` reads the atomic. No `LeaderID()` on the `Node` interface — the node already delivers the answer through `Ready`.

### Bugs surfaced by wiring

Wiring also exposed mechanical plumbing gaps from earlier episodes that compiled but never ran end-to-end:

- **Shutdown drain on `applyc`.** `handleBatch` sends on the unbuffered `applyc`. If `Shutdown` cancels the server context before the apply loop reads, the send blocks forever. Selecting on `stopc` alongside the `applyc` send prevents the deadlock.
- **Inner node teardown.** `raftHost.Stop()` must call `r.n.Stop()` after the host goroutine exits, otherwise `node.run` leaks.
- **Storage cleanup.** The server must track `raftStorage` and close it in `Shutdown` to release file handles.

### The constructor injection seam

Writing the integration tests forced a question: how does etcd let unit tests replace the real `raft.Node` with a fake?

etcd never conditionally constructs raft. In production, `NewServer` always builds the full chain internally. In unit tests, code bypasses `NewServer` entirely and constructs `&EtcdServer{}` directly, injecting mocks through the same interface fields that production uses (`raft.Node`, `Wait`, storage). The seam is the interface, not a conditional construction path.

We follow the same pattern: `NewServer` unconditionally calls `initializeRaftHost()`. Unit tests construct `&Server{}` directly with fakes — no `NewServer` call, no field patching.

## Minimum tests

**Invariant:** a PUT proposed on the leader reaches committed state through Raft consensus.

1. **Create Server with zero-ID is rejected** — proves invalid ID fails at construction time.
2. **A leader gets elected without manual campaigning** — proves the tick-driven mechanism works with a real time ticker.
3. **PUT proposal appears in leader's state machine** — proves a quorum write flows through network transport, Raft consensus, and the apply loop into `Server.sm`.
4. **PUT proposal appears in followers' state machines** — proves committed entries propagate to followers via commit-index advancement in heartbeats.
5. **handleBatch unblocks on stopc when applyc has no reader** — proves the shutdown-drain path: if the apply loop is gone, committed entries don't deadlock the host.
6. **Stop calls inner node Stop** — proves `raftHost.Stop()` tears down the inner `node.run` goroutine.
7. **Double Start is a no-op** — proves `Start()` idempotency via `started` atomic guard.
8. **Cluster shutdown completes** — proves the full lifecycle (start → elect → stop) returns cleanly with no deadlock or leaked resources.
9. **PUT then shutdown does not deadlock** — proves the end-to-end drain: propose, commit, then immediately shut down.
10. **All 036 tests pass** — proves the `stopc` refactoring on both `node` and `raftHost` preserves existing behavior.

## Open threads

1. **Inbound/outbound split.** 036u injects a pre-bound listener but the transport still owns accept, connection tracking, and shutdown. A future episode should evaluate whether the server should own the accept loop and hand connections via `HandleConn` — that decision depends on TLS, snapshot routing, peer validation, and shutdown ordering concerns that don't exist yet.
2. **Shutdown ordering.** The immediate deadlock is fixed (`stopc` select in `handleBatch`), but `Shutdown` still cancels the server context before stopping `raftHost`. The broader question — when to stop the transport vs. the host vs. drain the apply loop — needs a proper design when graceful shutdown matters.