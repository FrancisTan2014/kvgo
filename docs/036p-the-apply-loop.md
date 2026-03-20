# 036p - The Apply Loop

We left 036 after [036o-the-wait](036o-the-wait.md) with two halves that don't touch yet. `RaftHost` pushes committed entries as opaque `[][]byte` onto an apply channel. `Wait` bridges a propose call to an apply result. What's missing is the loop that connects them — reading committed bytes, executing them against state, and triggering waiters.

## The problem

`RaftHost.Apply()` produces `toApply{data [][]byte}`. Nobody consumes this channel yet. Without a consumer, `RaftHost.handleBatch` blocks forever once entries commit.

The server needs a `run()` loop that:

1. Reads from the apply channel
2. Decodes each entry (unmarshal envelope → decode protocol request)
3. Executes the KV operation against a `StateMachine`
4. Triggers the waiter so the handler can respond to the client

This is the equivalent of etcd's `EtcdServer.run()` → `applyAll` → `applyEntryNormal` → `s.w.Trigger(id, result)`.

## The design

The `Server` gains three fields:

```go
type Server struct {
    ...
    raftHost RaftHost       // Raft plumbing (apply channel + propose)
    sm       StateMachine   // Raft-owned state
    w        Wait           // waiter registry
    ...
}
```

`RaftHost` is already an interface — the propose path will use the same field in the next sub-episode. `StateMachine` is new:

```go
type StateMachine interface {
    Get(key string) ([]byte, bool)
    Put(key string, value []byte) error
}
```

`engine.DB` satisfies this directly. Tests use a `map`-backed fake — no filesystem.

### The envelope

Format established in 036o:

```
[8-byte request ID (uint64 little-endian)][protocol-encoded request]
```

Decoded by `unmarshalEnvelope` (ID extraction) then `protocol.DecodeRequest` (command, key, value). For 036p, only `CmdPut` is executed. Unknown commands are logged, not fatal.

### The loop

`Server.run()` is a single goroutine:

1. Cache `s.raftHost.Apply()` as a local channel
2. Select on context cancellation or incoming `toApply`
3. For each entry in the batch: `unmarshalEnvelope` → `protocol.DecodeRequest` → dispatch by command
4. `CmdPut` → `s.sm.Put(key, value)` → `s.w.Trigger(id, err)`
5. Unknown commands → log + trigger waiter with error
6. Malformed/corrupt entries → log and skip (don't crash the loop)

## Testing without Start

The apply loop needs `s.ctx`, `s.raftHost`, `s.w`, and `s.sm`. We test it by:

1. Creating a `Server` via `NewServer`
2. Setting `s.raftHost` to a mock whose `Apply()` returns a controlled channel
3. Setting `s.sm` to a `map`-backed fake
4. Calling `s.run()` in a goroutine
5. Sending `toApply` on the channel, asserting `sm.Get()` returns the value

No `Start`, no filesystem, no network.

## Minimum tests

1. **`TestApplyLoopExecutesPut_036p`** — send one encoded PUT, assert `sm.Get(key)` returns the value.

2. **`TestApplyLoopTriggersWaiter_036p`** — register a waiter, send the entry, assert the waiter channel receives `nil`.

3. **`TestApplyLoopBatchMultipleEntries_036p`** — send three PUTs in one batch, assert all three keys visible via `sm.Get()`.

4. **`TestApplyLoopSurvivesMalformedEntry_036p`** — send a too-short entry followed by a valid entry. The valid entry must still apply (the loop doesn't crash on garbage).

5. **`TestApplyLoopStopsOnContextCancel_036p`** — cancel the server context, assert the `run()` goroutine exits.

## Bounded scope

036p is complete when:
- `StateMachine` interface has `Get` and `Put`; `engine.DB` satisfies it directly
- `Server` has `raftHost`, `sm`, and `w` fields
- `Server.run()` reads from `raftHost.Apply()`, decodes entries, executes PUTs against `sm`, and triggers waiters
- all tests pass with mock `RaftHost` + fake `StateMachine` (no disk, no network)

## Out of scope

- Wiring `handlePut` to propose through `RaftHost`
- GET through Raft (reads bypass Raft, same as etcd)
- Removing old replication/quorum code
- Commands other than `CmdPut`
- Error recovery or retry in the apply loop
- Snapshot apply / restore

## Open threads

1. **`NewServer` wiring.** Currently `NewServer` doesn't accept `RaftHost`, `StateMachine`, or `Wait`. For 036p, fields are set directly in tests. Wiring through construction comes when `Start` launches the loop.
2. **Dead waiter on leader loss.** If a proposal never commits, the waiter hangs until the handler's context timeout fires. Orphan cleanup on leadership change is deferred.
3. **Apply ordering.** Entries within a batch apply in order. Across batches, the unbuffered channel enforces one-at-a-time delivery.
4. **Batched engine writes.** Currently one `sm.Put` per entry. If per-entry overhead becomes measurable, the optimization is grouping a batch into a single engine transaction. Concurrent application is not safe — Raft's total order must be preserved. Measure first.
