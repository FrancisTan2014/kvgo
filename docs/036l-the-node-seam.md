# 036l - The Node Seam

We left 036 after [036k-the-snapshot-transfer](036k-the-snapshot-transfer.md) with a thin Raft core that can now survive the compaction boundary honestly. But the server still does not host Raft through a stable runtime seam.

That gap matters more than it first appears. The next target of 036 is to rewrite one real server write path so that the log owns the write. But today the application layer still has no complete, honest contract for talking to Raft at runtime.

`Raft` already knows how to do proposal, voting, append response handling, and snapshot transfer. What it does not yet have is a clean boundary for the outside world to drive it.

That is the real subject of 036l.

## The problem

The server must do three different kinds of work around Raft:

1. inject client proposals
2. inject inbound Raft messages from peers
3. consume outbound work from Raft and perform the side effects outside the algorithm

Right now only part of that shape exists. `Ready` already exposes outbound work, and `Propose()` already injects client intent. But the runtime seam is still incomplete:

- the application layer does not yet own a complete ingress path for Raft messages
- peer membership is hidden inside `Raft` instead of being supplied by the host
- Raft node IDs and server peer IDs live in different identity spaces

If we replace the server write path before those seams are stabilized, we will wire the new architecture through temporary glue and then rewrite it again.

## What real systems do

`etcd` does not let the application reach into the Raft state machine directly. The host runtime drives it through a very small surface:

- proposals go in
- received messages go in
- `Ready` comes out
- the host persists, sends, and applies that work
- then the host tells Raft it may advance

That split is the reason the algorithm stays testable. Transport, storage, and application visibility remain outside the pure state machine.

`kv-go` should follow the same shape here, even though the surrounding server is not a copy of `etcd`.

## The decision

036l introduces one stable runtime seam:

**the application layer talks to Raft only through `Node`, and all Raft side effects leave through `Ready`.**

That means `Node` becomes the real host-facing API, not a thin temporary wrapper.

The minimum honest `Node` surface is:

```go
type Node interface {
    Propose(ctx context.Context, data []byte) error
    Step(ctx context.Context, m Message) error
    Campaign(ctx context.Context) error
    Ready() <-chan Ready
    Advance()
}
```

This is the whole runtime picture of 036l:

- `Propose()` lets the application inject client write intent
- `Step()` lets the network/runtime inject inbound Raft messages
- `Campaign()` lets the runtime trigger candidacy without reaching into `Raft`
- `Ready()` is the only outward path for persistence, transport, and apply work
- `Advance()` confirms that the host finished handling the current batch

The application layer should not call `Raft` directly.

## Membership ownership

Peer membership should not remain a hidden field inside `Raft`.

Membership is part of the node's runtime identity, so it should be supplied at construction through a config-owned seam rather than patched in later through ad-hoc mutation.

A small configuration shape is enough:

```go
type Config struct {
    ID      uint64
    Peers   []uint64
    Storage Storage
}
```

This makes one fact explicit: the host owns the cluster view that Raft starts from.

036l does not need to solve dynamic membership changes. It only needs to stop hiding static membership inside the pure state machine.

## Identity translation

There is also an unavoidable integration fact in `kv-go`: Raft node identity and server peer identity are not the same thing.

- Raft uses `uint64`
- the existing `PeerManager` is keyed by server-facing `string` node IDs

That mismatch should not be pushed down into `Raft`.

036l only makes that boundary visible. The actual server-side adapter that translates between:

- Raft IDs used by the algorithm
- peer keys used by `PeerManager`

is still the next integration step, after the `Node` seam itself is real.

## Why not `ProgressTracker` yet

It is tempting to introduce follower progress tracking first, because real replication eventually needs per-follower resend state.

But that is a different problem.

`ProgressTracker` answers questions like:

- what does this follower likely have?
- which index should we retry next?
- when does append repair yield to snapshot?

Those are replication-thickening concerns above the runtime seam. They matter after the host contract is stable.

036l is earlier than that.

The missing truth right now is not follower progress. The missing truth is:

**who is allowed to talk to Raft, and through which boundary do Raft side effects leave the algorithm?**

So `ProgressTracker` should wait for the next step.

## Reusing existing server pieces

036l should reuse `PeerManager` as transport and peer lookup infrastructure, but not as the meaning of replication.

The old local-write-first quorum path remains the legacy architecture. Reusing `PeerManager` is acceptable because it solves dialing and peer-address ownership, not because it preserves the old write semantics.

So the host shape becomes:

1. server receives or creates an event
2. server calls `Node.Propose()`, `Node.Step()`, or `Node.Campaign()`
3. server consumes `Ready`
4. server persists entries, sends outbound messages, applies committed entries
5. server calls `Advance()`

That is the seam 036l exists to make explicit.

## What 036l proves

036l proves one invariant only:

**the server can host Raft through one clear runtime seam: ingress through `Node`, egress through `Ready`.**

That is enough to make the next episode safe. Once this seam is real, one actual server write path can be rewritten onto the Raft-owned log without building on unstable glue.

## Minimum tests

#1 `TestNodeStepFeedsInboundMessageIntoRaft_036l`
A host can inject an inbound Raft message through `Node.Step()` without reaching into `Raft` directly.

#2 `TestNodeReadyIsTheOnlyOutboundMessagePath_036l`
Outbound protocol work becomes visible to the host only through `Ready.Messages`.

#3 `TestNodeConfigOwnsInitialMembership_036l`
Peer membership is supplied by node configuration rather than hidden mutation after construction.

## Bounded scope

036l is complete when:
- `Node` is the real host-facing Raft interface
- inbound Raft messages enter through `Node.Step()`
- runtime-triggered candidacy enters through `Node.Campaign()`
- outbound persistence / transport / apply work leaves only through `Ready`
- initial peer membership is supplied by config instead of hidden inside `Raft`
- tests pass
