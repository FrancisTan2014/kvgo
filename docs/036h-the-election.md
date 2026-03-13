# 036h - The Election

We're getting close to the migration stage. We left 036 after [036g-the-response](036g-the-response.md): commit now depends on follower evidence, but leadership is still not earned. Thus, 036h intends to prove:

**A node becomes leader only after majority vote responses in one term.**

By far, our Raft leader is assigned manually. After 036h, the leader is born from an election. But I'll keep those unit tests that set `state=Leader` explicitly, since they still prove the truths they had intended to.

As the election flow we have built in [031-the-election](031-the-election.md), we need an entry point to `runElection`. The difference is that 031 is an ad hoc intention, while 036h intends to let Raft control the order. `Campaign` from `etcd/raft` now surfaces. Its meaning — "start a series of actions for a specified purpose" — is a good fit for starting an election locally without pretending that election timeout is a network message.

With `Campaign` the entry point, we have the bounded shape of 036h:
- Campaign() starts a local election
- candidate increments term
- candidate becomes `Candidate`
- candidate votes for itself
- candidate records self-vote
- candidate emits MsgVote to peers
- followers handle Step(MsgVote) and reply with term-scoped MsgVoteResp
- granting a higher-term vote steps the local node down to `Follower`
- only a `Candidate` counts Step(MsgVoteResp)
- candidate handles Step(MsgVoteResp) and becomes Leader only on majority in the current term

For a node that triggers `Campaign`, it needs an evidence collector to determine whether quorum is reached. As with `peers` in 036g, a map is enough for now: `map[uint64]bool`. The important point is not the container shape, but the boundary: this evidence is term-scoped election evidence, not a replication tracker. It must be reset when a new election starts.

`MsgVoteResp` must also carry the response result. I keep `Reject` instead of `Granted`, because the rejection branch is the one that naturally widens later if the protocol needs to carry hints or richer refusal reasons.

Once 036h needs to ignore stale vote responses, `MsgVoteResp` also needs to carry the election term it belongs to. That is enough to reject older responses honestly. Higher-term response handling is acceptable to leave incomplete here and can be tightened later when term ownership grows beyond this bounded election seam.

How does an election proposal get granted by a voter? From the ad hoc version we have implemented, a voter grants an election when:
- the term is not older
- and it has not voted yet in the term
- and the candidate's log is at least as up-to-date as the voter's log

The third condition is out of scope here. It is another seam for handling stale-leader issues. So I defer log freshness to the next sub-episode and keep 036h focused on majority-earned leadership.

Within that bounded scope, the voter-side rule becomes:
- reject if the candidate term is older than the voter's term
- reject if this node has already voted for a different candidate in the same term
- otherwise grant, adopt the candidate term, step down to `Follower`, and record `votedFor`, with log freshness deferred

Fundamentals are ready, let's shape our design:
```go
const (
    ...
	MsgVote     MessageType = 3
	MsgVoteResp MessageType = 4
)

type Message struct {
    ...
    Term   uint64
    Reject bool
}

type Raft struct {
    ...
    votedFor uint64
    votes    map[uint64]bool
}

func (r *Raft) Campaign() error {
    if r.state == Leader {
        // guard
        return
    }
    // term++
    // state = Candidate
    // votedFor = self
    // reset votes and record self-vote
    // produce and expose MsgVote
}

func (r *Raft) Step(m Message) error {
    switch m.Type {
    ...
    case MsgVote:
        rejected := m.Term < r.term ||
            (m.Term == r.term && r.votedFor != 0 && r.votedFor != m.From)
        // when granted: term = m.Term, state = Follower, votedFor = m.From
        // produce and expose MsgVoteResp
    case MsgVoteResp:
        // ignore when state != Candidate
        // ignore when m.Term < current election term
        // update the evidence collector
        // become leader if quorum reached
    }
}
```

## Minimum tests

#1 `TestCandidateDoesNotBecomeLeaderWithOnlySelfVote_036h` - self-vote alone is insufficient
#2 `TestCandidateBecomesLeaderAfterMajorityVoteResponses_036h` - majority elects a leader
#3 `TestVoteRejectedForOlderTerm_036h` and `TestVoteRejectedWhenExistingVote_036h` - bounded vote rejection rules hold
#4 `TestVoteGrantUpdatesTermAndVotedFor_036h` - granting a vote adopts the election term locally
#5 `TestGrantingHigherTermVoteStepsDownToFollower_036h` - granting a higher-term vote gives up older local leadership or candidacy
#6 `TestStaleVoteRespIgnored_036h` - old-term vote responses must not count toward the current election

## Bounded scope

036h is complete when:
- `Campaign` starts a local election by incrementing `term`, becoming `Candidate`, voting for self, and exposing `MsgVote`
- `Step(MsgVote)` exposes `MsgVoteResp` and, when granting, updates `term`, steps down to `Follower`, and records `votedFor`
- `Step(MsgVoteResp)` is counted only by a `Candidate`, and older-term responses are ignored
- `Step(MsgVoteResp)` changes `state` to `Leader` when quorum is reached in one term
- log freshness stays deferred
- tests pass
