package raft

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func newLeaderRaftWithOnePeer() Raft {
	r := *NewRaft(0)
	r.id = 1
	r.term = 1
	r.state = Leader
	r.peers = append(r.peers, 2)
	return r
}

// Historical: pre-036f, Propose returned the created Entry directly.
// Obsoleted in 036f when Propose began exposing work through Ready instead.
//
// func TestProposeIncreasesLastLogIndex(t *testing.T) {
// 	r := Raft{}
// 	e1 := r.Propose([]byte("foo"))
// 	require.Equal(t, uint64(1), e1.Index)
// 	e2 := r.Propose([]byte("foo"))
// 	require.Equal(t, uint64(2), e2.Index)
// }

func TestProposeNotChangeCommitIndex_036b(t *testing.T) {
	r := newLeaderRaftWithOnePeer()
	require.NoError(t, r.Propose([]byte("")))
	require.Zero(t, r.commitIndex)
}

func TestReadyReturnsProposedEntry_036b(t *testing.T) {
	r := newLeaderRaftWithOnePeer()
	require.NoError(t, r.Propose([]byte("")))
	ready := r.Ready()
	require.Len(t, ready.Entries, 1)
	require.Len(t, ready.CommittedEntries, 0)
}

func TestAdvanceClearReadyEntries_036b(t *testing.T) {
	r := newLeaderRaftWithOnePeer()
	require.NoError(t, r.Propose([]byte("foo")))
	r.CommitTo(1)

	ready := r.Ready()
	require.Len(t, ready.Entries, 1)
	require.Len(t, ready.CommittedEntries, 1)
	require.Len(t, ready.Messages, 1)

	r.Advance()
	ready = r.Ready()
	require.Len(t, ready.Entries, 0)
	require.Len(t, ready.CommittedEntries, 0)
	require.Len(t, ready.Messages, 0)
}

func TestCommitToIncreasesCommitIndex_036b(t *testing.T) {
	r := newLeaderRaftWithOnePeer()
	require.NoError(t, r.Propose([]byte("foo")))

	r.CommitTo(1)
	require.Equal(t, uint64(1), r.commitIndex)
}

func TestCommitToNeverDecreasesCommitIndex_036b(t *testing.T) {
	r := newLeaderRaftWithOnePeer()
	require.NoError(t, r.Propose([]byte("foo")))

	r.CommitTo(1)
	require.Equal(t, uint64(1), r.commitIndex)

	r.CommitTo(0)
	require.Equal(t, uint64(1), r.commitIndex)
}

func TestReadyExposesCommittedEntsOnlyAfterCommitTo_036b(t *testing.T) {
	r := newLeaderRaftWithOnePeer()
	require.NoError(t, r.Propose([]byte("foo")))

	ready := r.Ready()
	require.Len(t, ready.CommittedEntries, 0)

	r.CommitTo(1)
	ready = r.Ready()
	require.Len(t, ready.CommittedEntries, 1)
}

func TestFullLifecycle_036b(t *testing.T) {
	r := newLeaderRaftWithOnePeer()
	require.NoError(t, r.Propose([]byte("x")))

	// Phase 1: unstable only
	rd := r.Ready()
	require.Len(t, rd.Entries, 1)
	require.Len(t, rd.CommittedEntries, 0) // ← invariant: not applied yet
	r.Advance()

	// Phase 2: committed
	r.CommitTo(1)
	rd = r.Ready()
	require.Len(t, rd.Entries, 0)
	require.Len(t, rd.CommittedEntries, 1) // ← now safe to apply
	r.Advance()

	// Phase 3: idle
	require.False(t, r.HasReady())
}

func TestAppendMessageIsReadyAfterLeaderPropose_036f(t *testing.T) {
	r := newLeaderRaftWithOnePeer()
	require.NoError(t, r.Propose([]byte("foo")))

	rd := r.Ready()
	require.Len(t, rd.Messages, 1)
	require.Len(t, rd.CommittedEntries, 0)

	msg := rd.Messages[0]
	require.Equal(t, MsgApp, msg.Type)
	require.Len(t, msg.Entries, 1)
	require.Equal(t, uint64(1), msg.Entries[0].Index)
	require.Equal(t, []byte("foo"), msg.Entries[0].Data)
}

func TestNewEntryIsReadyAfterFollowerStepMsgApp_036f(t *testing.T) {
	leader := newLeaderRaftWithOnePeer()
	require.NoError(t, leader.Propose([]byte("foo")))

	msg := leader.Ready().Messages[0]
	r := Raft{state: Follower}
	require.NoError(t, r.Step(msg))

	rd := r.Ready()
	require.Len(t, rd.Entries, 1)
	require.Equal(t, msg.Entries[0], rd.Entries[0])

	// follower doesn't get entry through Propose()
	r2 := Raft{state: Follower}
	require.NoError(t, r2.Propose([]byte("foo")))

	rd = r2.Ready()
	require.Len(t, rd.Messages, 0)
	require.Len(t, rd.Entries, 0)
}

func TestTrackerCreatedOnPropose_036g(t *testing.T) {
	leader := newLeaderRaftWithOnePeer()
	require.NoError(t, leader.Propose([]byte("foo")))

	id := entryID{index: 1, term: 1}
	require.Contains(t, leader.acks, id)
	require.True(t, leader.acks[id][leader.id])
}

func TestTrackerUpdatedOnStepMsgAppResp_036g(t *testing.T) {
	leader := newLeaderRaftWithOnePeer()
	require.NoError(t, leader.Propose([]byte("foo")))

	msg := leader.Ready().Messages[0]
	follower := *NewRaft(2)
	follower.state = Follower
	require.NoError(t, follower.Step(msg))

	resp := follower.Ready().Messages[0]
	require.NoError(t, leader.Step(resp))

	id := entryID{index: 1, term: 1}
	require.Contains(t, leader.acks, id)
	require.True(t, leader.acks[id][2])
}

func TestCommittedEntriesReadyAfterAppendQuorumReached_036g(t *testing.T) {
	leader := newLeaderRaftWithOnePeer()
	require.NoError(t, leader.Propose([]byte("foo")))

	msg := leader.Ready().Messages[0]
	follower := *NewRaft(2)
	follower.state = Follower
	require.NoError(t, follower.Step(msg))

	resp := follower.Ready().Messages[0]
	require.NoError(t, leader.Step(resp))

	rd := leader.Ready()
	require.Len(t, rd.CommittedEntries, 1)
	require.Equal(t, []byte("foo"), rd.CommittedEntries[0].Data)
}

func TestCandidateDoesNotBecomeLeaderWithOnlySelfVote_036h(t *testing.T) {
	voterId := uint64(2)

	n := NewRaft(1)
	n.peers = append(n.peers, voterId)

	require.NoError(t, n.Campaign())
	rd := n.Ready()
	require.Len(t, rd.Messages, 1)
	require.Equal(t, MsgVote, rd.Messages[0].Type)
	require.Equal(t, voterId, rd.Messages[0].To)
	require.Equal(t, Candidate, n.state)

	require.NoError(t, n.Step(Message{
		Type:   MsgVoteResp,
		From:   voterId,
		To:     n.id,
		Term:   n.term,
		Reject: true,
	}))
	require.Equal(t, Candidate, n.state)
}

func TestCandidateBecomesLeaderAfterMajorityVoteResponses_036h(t *testing.T) {
	voterId := uint64(2)

	n := NewRaft(1)
	n.peers = append(n.peers, voterId)

	require.NoError(t, n.Campaign())
	require.Equal(t, Candidate, n.state)

	require.NoError(t, n.Step(Message{
		Type:   MsgVoteResp,
		From:   voterId,
		To:     n.id,
		Term:   1,
		Reject: false,
	}))
	require.Equal(t, Leader, n.state)
}

func TestVoteRejectedForOlderTerm_036h(t *testing.T) {
	n := NewRaft(1)
	n.term = 2

	require.NoError(t, n.Step(Message{
		Type: MsgVote,
		Term: 1,
	}))
	rd := n.Ready()
	require.Len(t, rd.Messages, 1)
	require.Equal(t, MsgVoteResp, rd.Messages[0].Type)
	require.True(t, rd.Messages[0].Reject)
}

func TestVoteRejectedWhenExistingVote_036h(t *testing.T) {
	n := NewRaft(2)
	n.term = 1
	n.votedFor = 3

	require.NoError(t, n.Step(Message{
		Type: MsgVote,
		From: 1,
		Term: 1,
	}))
	rd := n.Ready()
	require.Len(t, rd.Messages, 1)
	require.Equal(t, MsgVoteResp, rd.Messages[0].Type)
	require.True(t, rd.Messages[0].Reject)
}

func TestVoteGrantUpdatesTermAndVotedFor_036h(t *testing.T) {
	n := NewRaft(2)
	n.term = 1

	require.NoError(t, n.Step(Message{
		Type: MsgVote,
		From: 1,
		Term: 2,
	}))

	require.Equal(t, uint64(2), n.term)
	require.Equal(t, uint64(1), n.votedFor)
}

func TestGrantingHigherTermVoteStepsDownToFollower_036h(t *testing.T) {
	n := NewRaft(2)
	n.term = 1
	n.state = Candidate

	require.NoError(t, n.Step(Message{
		Type: MsgVote,
		From: 1,
		Term: 2,
	}))
	require.Equal(t, Follower, n.state)
}

func TestStaleVoteRespIgnored_036h(t *testing.T) {
	voterId := uint64(2)

	n := NewRaft(1)
	n.term = 1
	n.peers = append(n.peers, voterId)

	require.NoError(t, n.Campaign())
	require.NoError(t, n.Step(Message{
		Type:   MsgVoteResp,
		From:   voterId,
		To:     n.id,
		Term:   1,
		Reject: false,
	}))
	require.Equal(t, Candidate, n.state)
}

func TestVoteRejectedWhenCandidateLastLogTermIsOlder_036i(t *testing.T) {
	n := NewRaft(1)
	n.term = 2
	n.log = append(n.log, Entry{Term: 2, Index: 1})

	require.NoError(t, n.Step(Message{
		Type:    MsgVote,
		Term:    3,
		LogTerm: 1,
		Index:   1,
	}))

	rd := n.Ready()
	require.Len(t, rd.Messages, 1)
	require.True(t, rd.Messages[0].Reject)
}

func TestVoteRejectedWhenCandidateLastLogIndexIsLowerInSameLastTerm_036i(t *testing.T) {
	n := NewRaft(1)
	n.term = 2
	n.log = append(n.log, Entry{Term: 2, Index: 2})

	require.NoError(t, n.Step(Message{
		Type:    MsgVote,
		Term:    3,
		LogTerm: 2,
		Index:   1,
	}))

	rd := n.Ready()
	require.Len(t, rd.Messages, 1)
	require.True(t, rd.Messages[0].Reject)
}

func TestCampaignIncludesLastLogPositionInVoteRequest_036i(t *testing.T) {
	n := NewRaft(1)
	n.term = 2
	n.log = append(n.log, Entry{Term: 2, Index: 2})
	n.peers = append(n.peers, 2)

	require.NoError(t, n.Campaign())

	rd := n.Ready()
	require.Len(t, rd.Messages, 1)
	require.Equal(t, uint64(2), rd.Messages[0].LogTerm)
	require.Equal(t, uint64(2), rd.Messages[0].Index)
}

func TestRejectingHigherTermStaleVoteUpdatesTermAndClearsOldVote_036j(t *testing.T) {
	n := NewRaft(2)
	n.term = 1
	n.state = Candidate
	n.votedFor = n.id
	n.log = append(n.log, Entry{Term: 1, Index: 2})

	require.NoError(t, n.Step(Message{
		Type:    MsgVote,
		From:    1,
		Term:    2,
		LogTerm: 0,
		Index:   0,
	}))

	rd := n.Ready()
	require.Len(t, rd.Messages, 1)
	require.True(t, rd.Messages[0].Reject)
	require.Equal(t, uint64(2), n.term)
	require.Equal(t, Follower, n.state)
	require.Zero(t, n.votedFor)
}

func TestHigherTermRejectionClearsVoteForLaterSameTermGrant_036j(t *testing.T) {
	n := NewRaft(2)
	n.term = 1
	n.state = Candidate
	n.votedFor = n.id
	n.log = append(n.log, Entry{Term: 1, Index: 2})

	require.NoError(t, n.Step(Message{
		Type:    MsgVote,
		From:    1,
		Term:    2,
		LogTerm: 0,
		Index:   0,
	}))
	n.messages = nil

	require.NoError(t, n.Step(Message{
		Type:    MsgVote,
		From:    3,
		Term:    2,
		LogTerm: 1,
		Index:   2,
	}))

	rd := n.Ready()
	require.Len(t, rd.Messages, 1)
	require.False(t, rd.Messages[0].Reject)
	require.Equal(t, uint64(3), n.votedFor)
}

func TestHigherTermGrantedVoteResponseMakesCandidateYield_036j(t *testing.T) {
	n := NewRaft(1)
	n.peers = append(n.peers, 2)

	require.NoError(t, n.Campaign())
	require.Equal(t, Candidate, n.state)
	require.Equal(t, uint64(1), n.term)
	require.Equal(t, n.id, n.votedFor)

	require.NoError(t, n.Step(Message{
		Type:   MsgVoteResp,
		From:   2,
		To:     n.id,
		Term:   2,
		Reject: false,
	}))

	require.Equal(t, Follower, n.state)
	require.Equal(t, uint64(2), n.term)
	require.Zero(t, n.votedFor)
	require.Nil(t, n.votes)
}

func TestHigherTermVoteResponseClearsSelfVoteForLaterGrant_036j(t *testing.T) {
	n := NewRaft(2)
	n.peers = append(n.peers, 1, 3)

	require.NoError(t, n.Campaign())
	require.Equal(t, Candidate, n.state)

	require.NoError(t, n.Step(Message{
		Type:   MsgVoteResp,
		From:   1,
		To:     n.id,
		Term:   2,
		Reject: true,
	}))
	require.Equal(t, Follower, n.state)
	n.messages = nil

	require.NoError(t, n.Step(Message{
		Type:    MsgVote,
		From:    3,
		Term:    2,
		LogTerm: 0,
		Index:   0,
	}))

	rd := n.Ready()
	require.Len(t, rd.Messages, 1)
	require.False(t, rd.Messages[0].Reject)
	require.Equal(t, uint64(3), n.votedFor)
}

func TestHigherTermAppendRevokesStaleAuthorityBeforeAppendLogic_036j(t *testing.T) {
	n := NewRaft(2)
	n.term = 1
	n.state = Candidate
	n.votedFor = n.id
	n.votes = map[uint64]bool{n.id: true, 1: false}

	require.NoError(t, n.Step(Message{
		Type:    MsgApp,
		From:    1,
		To:      n.id,
		Term:    2,
		Entries: []Entry{{Index: 1, Term: 2, Data: []byte("x")}},
	}))

	require.Equal(t, uint64(2), n.term)
	require.Equal(t, Follower, n.state)
	require.Zero(t, n.votedFor)
	require.Nil(t, n.votes)

	rd := n.Ready()
	require.Len(t, rd.Entries, 1)
	require.Equal(t, Entry{Index: 1, Term: 2, Data: []byte("x")}, rd.Entries[0])
	require.Len(t, rd.Messages, 1)
	require.Equal(t, MsgAppResp, rd.Messages[0].Type)
	require.Equal(t, uint64(2), rd.Messages[0].Term)
	require.Equal(t, uint64(2), rd.Messages[0].LogTerm)
}

func TestHigherTermAppendRespRevokesStaleLeaderAuthorityBeforeAckTracking_036j(t *testing.T) {
	leader := newLeaderRaftWithOnePeer()
	leader.votedFor = leader.id
	leader.votes = map[uint64]bool{leader.id: true, 2: false}
	require.NoError(t, leader.Propose([]byte("foo")))

	id := entryID{index: 1, term: 1}
	require.False(t, leader.acks[id][2])

	require.NoError(t, leader.Step(Message{
		Type:    MsgAppResp,
		From:    2,
		To:      leader.id,
		Term:    2,
		Index:   1,
		LogTerm: 1,
	}))

	require.Equal(t, uint64(2), leader.term)
	require.Equal(t, Follower, leader.state)
	require.Zero(t, leader.votedFor)
	require.Nil(t, leader.votes)
	require.False(t, leader.acks[id][2])
	require.Zero(t, leader.commitIndex)
}
