# 036o - The Wait

We left 036 after [036n-the-progress](036n-the-progress.md) with a solid Raft model that guarantees entries not applied are invisible. Naturally, we want to rewrite the old write path so a client PUT flows through `RaftHost.Propose`. But `Propose` returning doesn't mean the write succeeded or failed — it only means the entry entered the log. The handler must wait for commit and apply before responding to the client.

## The problem

The missing piece is a notification mechanism. A handler proposes a write and blocks. Later, the apply path processes the committed entry. Something must connect the two — a waiter registry keyed by a request identity.

Two questions drive the design:

1. **What identity?** A Raft entry has `(index, term)`, but that's assigned inside the state machine. We can't get it at propose time without modifying the proved model, and it's an internal detail that shouldn't leak upward.

2. **Who owns the coordination?** The waiter map must be readable at apply time and writable at propose time. Both happen from different goroutines. The layer that owns this map must see both sides: request identity and apply outcome.

## What etcd does

`etcd` generates a unique request ID per proposal, embeds it in the data payload as structured bytes, and manages the full waiter lifecycle inside `EtcdServer` — not inside `raftNode`.

The flow:

1. `EtcdServer` generates an ID, marshals `(ID, request)` into an opaque byte array
2. `EtcdServer` registers a channel: `ch = s.w.Register(id)` — **before** proposing
3. `EtcdServer` calls `raftNode.Propose(ctx, bytes)` — fire and forget
4. `raftNode` carries the bytes through Raft, pushes committed entries to an `applyc` channel
5. `EtcdServer` reads from `applyc`, deserializes entry data, extracts the ID
6. `EtcdServer` calls `s.w.Trigger(id, result)` — wakes the handler

`raftNode` never touches the waiter map. It never deserializes entry data. It's pure plumbing — receives `Ready`, persists, sends, pushes committed entries outward, calls `Advance()`. All the intelligence (marshal, unmarshal, waiter signal) lives in the server layer.

The critical design decision: **the waiter registry and both sides of the waiter lifecycle sit in the server, not in the Raft runtime**. This follows the principle: coordination belongs at the layer that can see both the request identity and the apply outcome.

## The leak in 036m

Reading `etcd` exposed a problem in our current design. In [036m-the-raft-host](036m-the-raft-host.md), we introduced an injected `Applier` interface:

```go
type Applier interface {
    Apply(entries []raft.Entry) error
}
```

This has two issues:

1. **Raft detail leaks upward.** The server must import `raft.Entry` to implement `Applier`. But the server doesn't need `Index`, `Term`, or any Raft metadata — it only needs the opaque `Data` bytes it marshaled in the first place.

2. **Outside world controls inside behavior.** An injected interface lets the server's apply latency block `RaftHost`'s run loop. If apply is slow, the entire Raft coordination loop stalls. `etcd` avoids this by exposing a channel instead — the run loop pushes to a channel and moves on; the server pulls in its own goroutine at its own pace.

These aren't cosmetic concerns. They're signals that the boundary is drawn in the wrong place. The `Applier` was introduced two episodes ago as a reasonable first cut, but the wait mechanism forces the question of who owns what. The answer is clear: the server should own both the marshal and unmarshal sides of the application-level protocol, and `RaftHost` should be pure plumbing that never interprets the entry payload.

## The correction

Replace the injected `Applier` interface with an exposed apply channel, following `etcd`'s shape.

`RaftHost` extracts `entry.Data` from each committed entry, packages the byte slices into a `toApply` struct, and sends it on a channel. The server reads from that channel in its own loop. `RaftHost` never sees inside the bytes.

```go
// RaftHost exposes committed data
type toApply struct {
    Data [][]byte
}

func (r *raftHost) Apply() <-chan toApply
```

`RaftHost.handleBatch` changes from:

```go
if len(rd.CommittedEntries) > 0 {
    if err := r.applier.Apply(rd.CommittedEntries); err != nil {
        return err
    }
}
```

to:

```go
if len(rd.CommittedEntries) > 0 {
    data := make([][]byte, len(rd.CommittedEntries))
    for i, e := range rd.CommittedEntries {
        data[i] = e.Data
    }
    // unbuffered — blocks until the server consumes the batch
    r.applyc <- toApply{Data: data}
}
```

The `Applier` interface, the `Applier` field in `RaftHostConfig`, and related validation logic are removed.

## The server-side wait mechanism

With the apply channel in place, the server owns the full propose-wait-apply cycle:

```go
// Wait bridges the propose and apply paths
type Wait interface {
    Register(id uint64) <-chan any
    Trigger(id uint64, x any)
    IsRegistered(id uint64) bool
}

// Server propose path (handler goroutine)
id := nextID()
ch := w.Register(id)
data := marshal(id, payload)
raftHost.Propose(ctx, data)
select {
case x := <-ch:     // applied — return result
case <-ctx.Done():  // timeout — clean up waiter
    w.Trigger(id, nil)
}

// Server apply path (server main loop)
case ap := <-raftHost.Apply():
    for _, raw := range ap.Data {
        id, payload := unmarshal(raw)
        result := execute(payload)
        w.Trigger(id, result)
    }
```

The request ID serves dual purpose: waiter matching now, idempotent deduplication later. The buffered(1) channel ensures the `Trigger` call never blocks even if the handler already timed out and left.

## Impact on 036m

The following `RaftHost` changes affect existing 036m code and tests:

- Remove `Applier` interface and `Applier` field from `RaftHostConfig`
- Remove `Applier` validation from `validateRaftHostConfig` / `validatePublicRaftHostConfig`
- Remove `Applier` from `newRaftHost` / `NewRaftHost`
- Add `applyc chan toApply` to `raftHost`
- Add `Apply() <-chan toApply` method
- Update `handleBatch` to push to channel instead of calling `applier.Apply()`

Tests affected:
- `TestInternalRaftHostRequiresDependencies_036m` — remove "missing applier" case
- `TestNewRaftHostRequiresDependencies_036m` — remove "missing applier" case
- `TestHostDrainsReadyThenAdvance_036m` — replace `mockApplier` with channel read
- `TestHostReportsBatchFailureAndStops_036m` — apply errors no longer stop the host (the server handles them); may need rethinking
- `newRaftHostConfig()` / `newInternalRaftHostConfig_036m()` — remove `Applier` field

## Minimum tests

1. `TestWaitRegisterAndTrigger_036o` — `Register` returns a channel, `Trigger` sends the result, the channel receives it.

2. `TestWaitTriggerUnregisteredIdIsNoop_036o` — calling `Trigger` with an unknown ID doesn't panic.

3. `TestProposalSignaledAfterApply_036o` — server proposes through `RaftHost`, committed entry appears on `Apply()` channel, server triggers the waiter, handler receives the result.

4. `TestProposalTimesOutWithoutCommit_036o` — handler times out, waiter is cleaned up, no leaked channel.

5. `TestHostApplyChannelCarriesDataNotEntries_036o` — committed entries arrive on the `Apply()` channel as `[][]byte`, not as `[]raft.Entry`. The server never imports the raft package to read committed data.

## Bounded scope

036o is complete when:
- `RaftHost` no longer has an injected `Applier`
- `RaftHost` exposes committed entry data through a `toApply` channel
- the server reads from the apply channel and owns the full waiter lifecycle
- a `wait` struct backed by `map[uint64]chan any` + `sync.Mutex` supports `Register` and `Trigger`
- the request ID is embedded in the proposal data and extracted at apply time
- 036m tests are updated to reflect the removed `Applier` interface
- all tests pass

## Out of scope

- Idempotent deduplication (the ID supports it; the logic comes later)
- Full `Wait` package with sharding (etcd-style; defer until contention is measured)
- Orphan waiter cleanup on leadership change
- Concrete marshal/unmarshal format (a simple `[8-byte ID][payload]` prefix is enough)

## Open threads

1. **Orphan waiters:** if a proposal never commits (e.g., leader loses leadership mid-flight), the waiter map entry lingers. Leadership change may be the natural cleanup point.
2. **Apply back-pressure:** the apply channel is unbuffered or small-buffered. If the server is slow to consume, the `RaftHost` run loop blocks on the channel send. This is the right default (back-pressure), but may need tuning under load.
3. **Sharded wait map:** a single mutex may become a bottleneck under high concurrency. Sharding (like etcd's `Wait`) is the known fix when the measurement demands it.
