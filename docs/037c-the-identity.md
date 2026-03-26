# 037c — The Identity

The server binary cannot start a Raft cluster. `kv-server` has no `--node-id`, no `--peers`, no `--raft-port`. The only way to run a cluster is through test code that manually wires listener objects.

And when a client sends a PUT to a follower, the proposal vanishes. `stepFollower` has no case for `MsgProp` — the message falls through the switch silently.

## Boundary

Give the server binary a Raft identity and teach the Raft core to forward proposals from followers to the leader.

In scope:
- Startup identity — the server binary knows its node ID, its peers, and its Raft port
- Proposal forwarding — a follower's `Propose()` reaches the leader through the Raft layer, not the application
- Centralized message exit — all outbound Raft messages flow through `r.send()`, which stamps `From` and `Term`

Deferred:
- Dynamic peer discovery (requires conf-change)
- Linearizable reads (ReadIndex)

Out of scope:
- Client-side retry or redirect logic
- New protocol status codes

## Design

### Redirect vs forwarding

My first instinct was application-layer redirect: follower checks `LeaderID()`, returns a `StatusNotLeader` with the leader's client address, and the client reconnects. That works — some systems do it — but it requires three things the server shouldn't need: a `peerMap` (node ID → client address), a new status code, and a leader-check branch in every write handler.

etcd does not redirect. The follower calls `raft.Propose()`, and the Raft library forwards `MsgProp` to the leader internally. The leader appends the entry, replicates it, and the follower eventually commits via normal log replication. The client never knows it talked to a follower.

Forwarding wins on two axes. Latency: redirect costs a full client round-trip (follower → client → leader), forwarding costs one internal hop (follower → leader). Simplicity: `handlePut` calls `Propose()` on every node — same code, same wait, same timeout. The Raft layer handles routing.

### `r.send()` — the single exit point

Implementing the forward exposed a structural gap. Our Raft core has seven `r.messages = append(...)` call sites, each manually stamping `From` and `Term`. The new `MsgProp` forward path was already missing `m.From`.

etcd centralizes this in `r.send()` — no code appends to `r.msgs` directly. `send()` enforces:

- **`m.From = r.id`** — always, unless already set.
- **`m.Term = r.term`** — for most message types. Skipped for `MsgProp` (proposals carry no term). Vote messages must have `Term` pre-set by the caller.

All seven existing call sites migrate to `r.send()` and drop their manual `From`/`Term` fields. The invariant becomes structural.

### The three Raft core changes

1. **`stepFollower` forwards `MsgProp`** — if the follower knows the leader (`r.lead != None`), set `m.To = r.lead` and call `r.send(m)`. If no leader is known, return `ErrProposalDropped`.

2. **`IsLocalMsg` no longer includes `MsgProp`** — `Node.Step()` rejects local messages to prevent the application from injecting election triggers over the network. But `MsgProp` must now travel over the network (follower → leader). Removing it from the local-message set lets `Node.Step()` accept it from the transport.

3. **`stepLeader` already handles `MsgProp`** — no change needed. The forwarded proposal is appended and replicated like any local proposal.

### The proposal round-trip on a follower

1. Client sends PUT to a follower.
2. `handlePut` calls `s.raftHost.Propose(data)` — same code path as on the leader.
3. `Node.Propose()` → `r.Propose(data)` → `r.Step(MsgProp)` → `stepFollower`.
4. `stepFollower` calls `r.send(m)` — `send` stamps `From`, queues the message.
5. Leader receives `MsgProp` via `Node.Step()` → `stepLeader` appends entry.
6. Leader replicates via `MsgApp`. Follower commits. Apply loop fires. Waiter triggers.
7. `handlePut` returns `StatusOK` to the client.

The follower's `handlePut` blocks on `w.Register(id)` until the entry with that `requestID` is applied locally. Same wait mechanism the leader uses — no special follower path.

### The peer format

`--peers` takes a comma-separated list: `id=host:raftPort`. Example:

```
--node-id 1 --port 4001 --raft-port 5001 \
  --peers 2=127.0.0.1:5002,3=127.0.0.1:5003
```

Each entry provides the remote node's ID and Raft address. The local node is not included in `--peers` — its identity comes from `--node-id` and `--raft-port`.

### Failure modes

Forwarding moves routing into the Raft layer, but it doesn't eliminate proposal loss. Three cases drop a proposal before it reaches the leader:

1. **No leader known.** A follower that has never received a `MsgApp` has `lead == None`. `stepFollower` returns `ErrProposalDropped`. The client's `handlePut` propagates the error immediately — no silent loss.

2. **Candidate state.** During an election, the node is a candidate. There is no leader to forward to, and a candidate shouldn't append to its own log (it might lose the election). `stepCandidate` returns `ErrProposalDropped`. etcd does the same — candidates are a transient black hole for proposals.

3. **In-flight leader change.** A follower forwards `MsgProp` to node X, but by the time it arrives, X has stepped down. If X is now a follower with a known new leader, it re-forwards — the proposal bounces once and lands. If X is a candidate or a follower with no leader, the proposal is dropped. The originating follower's `handlePut` blocks on `w.Register(id)` until `WriteTimeout` fires. The client gets a timeout, not a success — safe but not ideal.

In all three cases the safety property holds: no proposal is falsely acknowledged. The liveness gap — a proposal can be silently lost during transitions — is inherent in leader-based forwarding. etcd addresses this at the client layer: `clientv3` retries proposals on timeout. We defer client retry to a future episode.

## Minimum tests

**Invariant:** a proposal on any node reaches the leader and commits across the cluster; the proposing node's waiter fires.

1. **Follower forwards MsgProp to leader** — a follower with a known leader, upon receiving MsgProp, produces an outbound MsgProp addressed to the leader.
2. **Follower drops MsgProp when no leader known** — a follower with `lead == None` returns `ErrProposalDropped`.
3. **MsgProp accepted by Node.Step** — `Node.Step()` no longer rejects `MsgProp` as a local message. A leader receiving a forwarded MsgProp appends the entry.
4. **send() stamps From on all messages** — a message constructed without `From` and routed through `send()` arrives with `From == r.id`. Verifies the centralized exit point.
5. **PUT on follower commits via forwarding** — integration test: a 3-node cluster where PUT is sent to a follower. The entry commits and appears in the follower's state machine.
6. **PUT on leader still works** — existing behavior preserved. Leader propose-wait-apply returns StatusOK.
7. **Candidate drops MsgProp with ErrProposalDropped** — a candidate receiving MsgProp returns ErrProposalDropped and produces no outbound proposal message.
8. **Leader panics on empty MsgProp** — a MsgProp with no entries panics immediately; it indicates a programming error upstream (etcd does the same).
9. **Stale leader re-forwards MsgProp** — a node that stepped down from leader to follower (with a known new leader) re-forwards an arriving MsgProp to the new leader. `From` preserves the original proposer.

## Open threads

1. **Proposal dropped feedback** — if the follower has no leader, `Propose()` returns `ErrProposalDropped` and `handlePut` propagates it as an error. A richer error (e.g. "retry later") could help clients. Deferred.
2. **GET on follower consistency** — follower reads are not linearizable. A stale follower can serve stale data. ReadIndex is deferred.
3. **Batch proposals** — `stepLeader` processes only `m.Entries[0]`. Multi-entry proposals (batch append) would need all entries assigned index and term. Deferred as a future feature.
