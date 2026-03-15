# 036j - The Higher Term

We left 036 after [036i-the-freshness](036i-the-freshness.md) with a cleaner vote rule, but one ownership knot still remained: observing a higher term and granting a vote were still partly coupled.

That coupling sounds harmless when the code is small. A node receives `MsgVote`, computes `rejected`, and only on the granted path updates `term`, steps down, and records the vote. But those actions do not answer the same question.

A vote answers: **does this candidate deserve authority in the current term?**

A higher term answers something broader: **is my current local authority already obsolete?**

Those questions can diverge. A candidate may carry a higher term and still deserve rejection because its log is stale. If local term adoption is tied to vote grant, then rejecting the stale candidate also preserves stale local authority. That is the hole 036j closes.

The bounded truth of this episode is:

**Observing a higher term through `MsgVote` revokes older local election ownership before vote grant or rejection is decided.**

This is still not the full protocol surface for higher-term handling. Later messages may widen the same truth again. But 036j makes it honest for the election message we already have.

Let me shape the design:

```go
type Raft struct {
    ...
    term     uint64
    state    State
    votedFor uint64
}

func (r *Raft) Step(m Message) error {
    switch m.Type {
    ...
    case MsgVote:
        if m.Term > r.term {
            r.term = m.Term
            r.state = Follower
            r.votedFor = 0
        }

        rejected := m.Term < r.term ||
            (m.Term == r.term && r.votedFor != 0 && r.votedFor != m.From) ||
            !r.isCandidateUpToDate(m)

        if !rejected {
            r.votedFor = m.From
        }
    }
}
```

The important change is not the line count. It is the separation of meanings. Higher-term observation now answers the epoch question first. Vote grant or rejection answers the candidate question after that.

That also explains why `votedFor` must be cleared when a higher term is observed. `votedFor` without a matching term is only meaningful inside the current epoch. Once the node learns that the epoch has changed, any carried-over vote ownership from the older term becomes stale state.

This may look like a tiny episode. It is tiny in code, but not in truth. 036h made leadership majority-earned. 036i made voting freshness-qualified. 036j makes local authority yield honestly when a newer epoch is observed, even if the candidate is still rejected.

So 036j proves:

**Observing a higher term through `MsgVote` revokes older local election ownership before vote grant or rejection is decided.**

## Minimum tests

#1 `TestRejectingHigherTermStaleVoteUpdatesTermAndClearsOldVote_036j`
#2 `TestHigherTermRejectionClearsVoteForLaterSameTermGrant_036j`

## Bounded scope

036j is complete when:
- `Step(MsgVote)` updates `term`, becomes `Follower`, and clears stale `votedFor` when `m.Term` is higher
- higher-term observation happens even when the vote is later rejected for freshness
- a later same-term fresh vote can still be granted after that reset
- tests pass
