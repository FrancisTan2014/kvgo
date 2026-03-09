# 036d — The Apply

The Raft series continues. 036b proved the execution boundary, and 036c proved the persistence boundary. From the input-output perspective, the input side is now visible. What is the output boundary?

Visibility is the next match. One of the problems Raft intends to solve is apply-before-commit. 036d intends to prove that only committed entries become visible, and that failed apply does not advance progress. Raft itself stays pure, so I need a higher layer that exposes the seam without dragging in the whole server.

It is a dangerously attractive option to connect Raft directly to `kv-go`. But that would pull in networking, future message flow, and a lot of unfinished consensus machinery. It breaks the bounded-scope methodology. So 036d will prove the invariant with one small subject instead: an `Applier` that consumes `Ready`, applies committed entries, and calls `Advance()` only after success.

This is still a concrete artifact. The concrete thing is just a boundary, not a final subsystem.

Forget the real shape of `kv-go` for now. The throughline here is visibility.

## Minimum interface
My first instinct was to invent a full `StateMachine` shape, but that is already too much architecture. 036d does not need reads. It only needs to answer one question: did committed entries become visible?

So the minimum interface is only:

```go
type ApplyTarget interface {
	Apply(entries []Entry) error
}
```

The `Applier` itself is the smallest controllable subject that lets a unit test drive:

`Propose -> Consume -> CommitTo -> Apply -> Advance`

It is built around `Raft`, not `Node`, because 036b intentionally deferred `Step`. Without `Step`, `Node` has no path from uncommitted to committed. The temporary path already exists at the `Raft` level: `CommitTo`.

A simple architecture is:

```
Applier
  |
Raft
```

The important point is that `CommitTo` only advances the index. It does not apply anything. `Ready()` is the computed view that exposes what the outside world may now do. That is why 036d lives exactly at this seam.

## Minimum failures
The first failure is straightforward: if an entry is merely proposed, it must not be applied. The first `Ready` may contain unstable `Entries`, but `CommittedEntries` must still be empty.

The second failure is the core of the episode: once `CommitTo` advances the commit index, `Ready.CommittedEntries` may be applied. If apply succeeds, `Advance()` may move progress forward. If apply fails, progress must stop there. The committed entry must remain pending instead of being silently forgotten.

036d is complete when:

- uncommitted entries are not applied
- committed entries are applied through `Ready.CommittedEntries`
- failed apply blocks `Advance()`