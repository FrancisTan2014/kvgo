# 036j - The Higher-Term Authority

We left 036 after [036i-the-freshness](036i-the-freshness.md) with freshness-qualified voting and the first local proofs that a newer term should revoke stale ownership.

`MsgVote`, `MsgVoteResp`, `MsgApp`, and `MsgAppResp` are only the current message surfaces where the same authority rule appears.

The real truth of 036j is:

**A message from a higher term revokes stale local authority before message-specific logic continues.**

That is the right extraction because it answers one question only:

**Is my current local authority already obsolete?**

That question is broader than vote grant, vote rejection, append acceptance, or append-response bookkeeping. Those are protocol-specific questions. The higher-term guard must run first, then the message-specific logic may continue inside the new epoch.

So the shape we want is not four parallel handlers with repeated term-reset code. It is one shared guard near the top of `Step()`:

```go
func (r *Raft) Step(m Message) error {
    if m.Term > r.term {
        r.term = m.Term
        r.state = Follower
        r.votedFor = 0
        r.votes = nil
    }

    switch m.Type {
    case MsgVote:
        ...
    case MsgVoteResp:
        ...
    case MsgApp:
        ...
    case MsgAppResp:
        ...
    }
}
```

The important point is the ownership boundary the guard captures. Placing it at the top of `Step()` keeps the full protocol picture visible while still expressing one invariant that cuts across the current message surface.

This also makes the append side less dramatic. If an old leader receives a higher-term `MsgAppResp`, it should yield first. After that, the leader-only ack and commit bookkeeping naturally stops because the node is no longer a leader. The same applies to `MsgApp`: higher-term observation is the epoch change; append handling is a separate question after the epoch is settled.

So 036j proves:

**A message from a higher term revokes stale local authority before message-specific logic continues.**

## Minimum tests

#1 `TestRejectingHigherTermStaleVoteUpdatesTermAndClearsOldVote_036j`
#2 `TestHigherTermRejectionClearsVoteForLaterSameTermGrant_036j`
#3 `TestHigherTermGrantedVoteResponseMakesCandidateYield_036j`
#4 `TestHigherTermVoteResponseClearsSelfVoteForLaterGrant_036j`
#5 `TestHigherTermAppendRevokesStaleAuthorityBeforeAppendLogic_036j`
#6 `TestHigherTermAppendRespRevokesStaleLeaderAuthorityBeforeAckTracking_036j`

## Bounded scope

036j is complete when:
- one shared higher-term guard runs before message-specific logic in `Step()`
- `MsgVote` and `MsgVoteResp` stay honest through that shared guard
- `MsgApp` and `MsgAppResp` also revoke stale local authority before append-specific logic continues
- tests pass