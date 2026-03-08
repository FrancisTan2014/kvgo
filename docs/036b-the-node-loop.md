# 036b — The Node Loop

## Inherit from 036a

036a already made the decision: `kv-go` pivots from a Redis-like primary-replica system to a Raft-based consensus system.

So 036b does not introduce a new sabotage. It starts construction.

The question here is not "why Raft." The question is: **what is the smallest living subject of Raft?**

## Boundary

036b only builds the Raft core loop in memory.

In scope:
- Raft node state
- Local proposals (`Propose`)
- Output channel (`Ready`)
- Acknowledge processed output (`Advance`)

Deferred:
- `Step` (inter-node messages) — needs elections and replication
- `Tick` (timer driver) — needs election timeout and heartbeat

Out of scope:
- Persistent storage implementation
- Real network transport
- Applying committed entries to `kvgo/engine`
- Membership changes and snapshots

If 036b tries to solve all of these, the entry point disappears again.

## Why the loop first

In 036a we listed many state fields and got stuck.

That happened because state alone is dead.
A state machine is not a struct; it is **state + transitions + observable output**.

So the first object to make real is the loop:
- input comes in (local proposal)
- state changes
- output comes out (`Ready`)

Without this loop, every field is just static data.

## The core model

A single Raft node owns:
- persistent safety-related state (`Term`, `Vote`, `Log`)
- volatile progress state (`CommitIndex`, `LastApplied`)
- role-related replication progress (leader-only)

But 036b does not aim to complete every branch of the algorithm.
It aims to establish one stable execution model:

`input -> transition -> Ready -> Advance`

This is the seam that lets storage, transport, and state machine apply plug in later without rewriting consensus logic.

## `Ready` as the contract

`Ready` is the boundary between pure Raft logic and side effects.

Raft produces:
- entries that must be persisted
- messages that should be sent to peers
- committed entries that may be applied by the state machine
- hard-state updates that must be durable

Raft itself does none of the side effects.

This is the main architecture claim of 036b: consensus logic is testable as pure transitions, while I/O is orchestrated outside.

## What 036b proves

036b is complete when the following is true:
- the node can consume input deterministically
- the node can emit `Ready` deterministically
- `Advance` moves the loop forward without hidden side effects
- uncommitted entries are not exposed as applyable output

At this point, the subject is alive.

036c can then add persistence against this boundary, instead of mixing persistence into algorithm logic.

## What I learned

The `run()` goroutine is a single-thread event loop. All mutations to Raft state happen inside it; external callers communicate only through channels. A nil channel never fires in `select`, so the Ready→Advance protocol is enforced structurally — the wrong call order is unrepresentable, not just undocumented.

I kept trying to make 036b bigger than it needed to be. The final implementation is ~80 lines of code and ~80 lines of tests. That felt too small, but every line is proven and every boundary is respected. CommitTo exists at the `Raft` level as a test shortcut; it does not appear in the `Node` channel loop because in real Raft, commit index advances as a side effect of `Step` processing messages — not as a separate external event.

When reading etcd's source, I kept wandering into beautiful code that had nothing to do with my question. The lesson: enter with a concrete purpose, find the evidence, stop, and return to your own workspace. A giant codebase is a trap if you browse without intent.

Ten years of shipping code taught me to implement. This episode taught me to stop before the boundary and prove what I have.

## References

1. [The Raft paper](https://raft.github.io/raft.pdf)
2. [etcd raft library](https://github.com/etcd-io/etcd/tree/main/raft)
3. [raftexample](https://github.com/etcd-io/etcd/tree/main/contrib/raftexample)
