# 036q - The Propose Path

We left 036p with a working apply side: committed entries flow from the apply channel through `run()` into `StateMachine.Put` and trigger waiters. But nothing *proposes* entries into Raft yet. The old `handlePut` writes directly to `engine.DB` and replicates via the Phase 1 quorum path. 036q closes the loop by wiring the handler to `RaftHost.Propose`.

## The problem

`handlePut` → `processPrimaryPut` does three things the Raft architecture replaces:

1. Writes to `s.db.Put` directly (bypasses Raft log ownership)
2. Generates a sequence number via `s.seq.Add(1)` (Raft owns ordering)
3. Forwards to replicas via `s.forwardToReplicas` (Raft replication replaces this)

The Raft write path should be: handler → propose → Raft commits → apply loop → state machine → respond. The handler must block until the apply loop triggers its waiter — the same async handshake `Wait` was built for.

## The design

A new method `raftPut` sits alongside the old `processPrimaryPut`. The old path stays untouched — both coexist until all paths are proven through Raft.

### The propose flow

```
handlePut (leader)
  └─ raftPut(ctx, req)
       1. id := s.nextRequestID()          // atomic uint64
       2. ch := s.w.Register(id)           // waiter before propose
       3. data := marshalEnvelope(id, encoded)
       4. err := s.raftHost.Propose(ctx, data)
       5. if err != nil → s.w.Trigger(id, err)
       6. select:
            case result := <-ch   → return result
            case <-ctx.Done()     → return timeout error
```

### ID generation

The envelope needs a `uint64` request ID. A simple `atomic.Uint64` counter on `Server` is sufficient for now — IDs only need to be unique within a single process lifetime. See open thread #4 for the collision risk across leader changes and etcd's `(memberID << 32) | counter` solution.

```go
func (s *Server) nextRequestID() uint64 {
    return s.reqIDGen.Add(1)
}
```

## Minimum tests

Same pattern as 036p — `fakeRaftHost` with a controlled `applyc` channel. The fake's `Propose` captures proposed data so the test can feed it back through apply, simulating the full Raft round-trip.

1. **`TestRaftPutRoundTrip_036q`** — propose a PUT, simulate commit via `applyc`, assert handler returns success and state machine has the value.

2. **`TestRaftPutProposeError_036q`** — `fakeRaftHost.Propose` returns an error. Assert handler receives the error (self-triggered through the waiter channel).

3. **`TestRaftPutTimeout_036q`** — propose succeeds but no commit arrives. Handler's context expires. Assert handler returns a timeout/context error.

4. **`TestRaftPutApplyError_036q`** — propose succeeds, commit arrives, but `sm.Put` returns an error. Assert handler receives the error through the waiter.

## Bounded scope

036q is complete when:
- `Server` has a `reqIDGen atomic.Uint64` for Raft request IDs
- `raftPut` implements the full propose-wait cycle: register → marshal → propose → wait on channel or context (propose failure self-triggers the waiter with the error)
- The old `handlePut` path is unchanged; `raftPut` is gated on `s.raftHost != nil`
- All tests pass with `fakeRaftHost` capturing proposed data and feeding it through `applyc`

## Out of scope

- Real transport between nodes
- Real `RaftHost.Start()` wiring
- GET through Raft (reads bypass Raft)
- Removing old Phase 1 replication code
- Leader detection / not-leader error forwarding through Raft (old `isLeader()` gate stays)

## Open threads

1. **Leader check.** `handlePut` uses `s.isLeader()` which is Phase 1 role state. Eventually this should come from Raft (`raftHost` knows if the node is leader). For now, the old check coexists.
2. **Propose to non-leader.** If the node thinks it's leader but Raft disagrees, `Propose` returns an error. The handler surfaces this to the client. Clean, but the client sees a generic error rather than a redirect. Proper not-leader handling with leader hint is deferred.
3. **Orphan waiters on leadership loss.** If a leader proposes, then loses leadership before the entry commits, the waiter hangs until the handler's context times out. Bulk cleanup on step-down is deferred (noted in 036p open threads).
4. **Request ID collision across leaders.** A plain counter resets on restart or re-election. If old waiters are still draining, the new leader's IDs can collide with the previous term's. etcd solves this by encoding the member ID in the high 32 bits: `ID = (memberID << 32) | counter`. This makes IDs globally unique per node with no coordination. Adopt this when real leadership transitions are possible.
5. **Phase 1 code removal.** The old write path (`processPrimaryPut`, `forwardToReplicas`, `quorumWrites`, `seq`, backlog, replication loop) coexists with the Raft path behind the `s.raftHost != nil` gate. Delete it in a dedicated cleanup episode after every client-visible operation (PUT, GET) is proven through Raft with passing tests.
