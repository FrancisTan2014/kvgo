package raft

import (
	"context"
	"log/slog"
	"testing"

	"kvgo/raftpb"

	"github.com/stretchr/testify/require"
)

type mockStorage struct {
	firstIndex uint64
	snap       *raftpb.SnapshotMeta
	applied    []*raftpb.SnapshotMeta
	hardState  *raftpb.HardState
}

func (m *mockStorage) InitialState() (*raftpb.HardState, error) {
	if m.hardState != nil {
		return m.hardState, nil
	}
	return &raftpb.HardState{}, nil
}

func (m *mockStorage) Save(entries []*raftpb.Entry, hard *raftpb.HardState) error {
	return nil
}

func (m *mockStorage) Entries(lo, hi uint64) ([]*raftpb.Entry, error) {
	return nil, nil
}

func (m *mockStorage) FirstIndex() uint64 {
	return m.firstIndex
}

func (m *mockStorage) LastIndex() uint64 {
	if m.snap != nil && m.snap.LastIncludedIndex > 0 {
		return m.snap.LastIncludedIndex
	}
	if m.firstIndex > 0 {
		return m.firstIndex - 1
	}
	return 0
}

func (m *mockStorage) Compact(index uint64) error {
	return nil
}

func (m *mockStorage) Close() error {
	return nil
}

func (m *mockStorage) Snapshot() (*raftpb.SnapshotMeta, error) {
	if m.snap == nil {
		return &raftpb.SnapshotMeta{}, nil
	}
	return m.snap, nil
}

func (m *mockStorage) ApplySnapshot(snap *raftpb.SnapshotMeta) error {
	m.snap = snap
	m.applied = append(m.applied, snap)
	return nil
}

func newTestRaft(id uint64) *Raft {
	return newRaft(Config{
		ID:            id,
		Storage:       &mockStorage{},
		ElectionTick:  10,
		HeartbeatTick: 1,
		Logger:        slog.Default(),
	})
}

func newLeaderRaftWithOnePeer() *Raft {
	r := newTestRaft(1)
	r.peers = []uint64{2}
	r.term = 1
	r.becomeLeader()
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

func TestReadyExposesHardStateAfterCampaign_036m(t *testing.T) {
	r := newTestRaft(1)
	r.peers = []uint64{2}

	require.NoError(t, r.Campaign())

	rd := r.Ready()
	require.Equal(t, &raftpb.HardState{Term: 1, VotedFor: 1, CommittedIndex: 0}, rd.HardState)
}

func TestReadyExposesHardStateAfterGrantingVote_036m(t *testing.T) {
	r := newTestRaft(2)

	require.NoError(t, r.Step(&raftpb.Message{Type: raftpb.MessageType_MsgVote, From: 1, To: 2, Term: 3}))

	rd := r.Ready()
	require.Equal(t, &raftpb.HardState{Term: 3, VotedFor: 1, CommittedIndex: 0}, rd.HardState)
}

func TestReadyExposesHardStateAfterCommit_036m(t *testing.T) {
	r := newLeaderRaftWithOnePeer()
	require.NoError(t, r.Propose([]byte("foo")))
	r.CommitTo(1)

	rd := r.Ready()
	require.Equal(t, &raftpb.HardState{Term: 1, VotedFor: 0, CommittedIndex: 1}, rd.HardState)
}

func TestHardStateChangesWithoutMessagesOrEntries_036m(t *testing.T) {
	r := newLeaderRaftWithOnePeer()

	require.NoError(t, r.Step(&raftpb.Message{Type: raftpb.MessageType_MsgApp, From: 2, To: 1, Term: 2}))
	require.Equal(t, &raftpb.HardState{Term: 2, VotedFor: 0, CommittedIndex: 0}, r.HardState())
}

func TestAdvanceDoesNotChangeHardState_036m(t *testing.T) {
	r := newLeaderRaftWithOnePeer()

	require.NoError(t, r.Step(&raftpb.Message{Type: raftpb.MessageType_MsgApp, From: 2, To: 1, Term: 2}))
	require.Equal(t, &raftpb.HardState{Term: 2, VotedFor: 0, CommittedIndex: 0}, r.HardState())

	r.Advance()
	require.Equal(t, &raftpb.HardState{Term: 2, VotedFor: 0, CommittedIndex: 0}, r.HardState())
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
	require.Equal(t, raftpb.MessageType_MsgApp, msg.Type)
	require.Len(t, msg.Entries, 1)
	require.Equal(t, uint64(1), msg.Entries[0].Index)
	require.Equal(t, []byte("foo"), msg.Entries[0].Data)
}

func TestNewEntryIsReadyAfterFollowerStepMsgApp_036f(t *testing.T) {
	leader := newLeaderRaftWithOnePeer()
	require.NoError(t, leader.Propose([]byte("foo")))

	msg := leader.Ready().Messages[0]
	follower := newTestRaft(2)
	require.NoError(t, follower.Step(msg))

	rd := follower.Ready()
	require.Len(t, rd.Entries, 1)
	require.Equal(t, msg.Entries[0], rd.Entries[0])

	// follower without a leader drops proposal with ErrProposalDropped
	follower2 := newTestRaft(3)
	require.ErrorIs(t, follower2.Propose([]byte("foo")), ErrProposalDropped)

	rd = follower2.Ready()
	require.Len(t, rd.Messages, 0)
	require.Len(t, rd.Entries, 0)
}

// Superseded by 036n: per-follower progress replaces per-entry ack tracking
//
// func TestTrackerCreatedOnPropose_036g(t *testing.T) {
// 	leader := newLeaderRaftWithOnePeer()
// 	require.NoError(t, leader.Propose([]byte("foo")))

// 	id := entryID{index: 1, term: 1}
// 	require.Contains(t, leader.acks, id)
// 	require.True(t, leader.acks[id][leader.id])
// }

// Superseded by 036n: per-follower progress replaces per-entry ack tracking
//
// func TestTrackerUpdatedOnStepMsgAppResp_036g(t *testing.T) {
// 	leader := newLeaderRaftWithOnePeer()
// 	require.NoError(t, leader.Propose([]byte("foo")))

// 	msg := leader.Ready().Messages[0]
// 	follower := *newTestRaft(2)
// 	follower.state = Follower
// 	require.NoError(t, follower.Step(msg))

// 	resp := follower.Ready().Messages[0]
// 	require.NoError(t, leader.Step(resp))

// 	id := entryID{index: 1, term: 1}
// 	require.Contains(t, leader.acks, id)
// 	require.True(t, leader.acks[id][2])
// }

// Superseded by 036n: per-follower progress replaces per-entry ack tracking
//
// func TestCommittedEntriesReadyAfterAppendQuorumReached_036g(t *testing.T) {
// 	leader := newLeaderRaftWithOnePeer()
// 	require.NoError(t, leader.Propose([]byte("foo")))

// 	msg := leader.Ready().Messages[0]
// 	follower := *newTestRaft(2)
// 	follower.state = Follower
// 	require.NoError(t, follower.Step(msg))

// 	resp := follower.Ready().Messages[0]
// 	require.NoError(t, leader.Step(resp))

// 	rd := leader.Ready()
// 	require.Len(t, rd.CommittedEntries, 1)
// 	require.Equal(t, []byte("foo"), rd.CommittedEntries[0].Data)
// }

func TestCandidateDoesNotBecomeLeaderWithOnlySelfVote_036h(t *testing.T) {
	voterId := uint64(2)

	n := newTestRaft(1)
	n.peers = append(n.peers, voterId)

	require.NoError(t, n.Campaign())
	rd := n.Ready()
	require.Len(t, rd.Messages, 1)
	require.Equal(t, raftpb.MessageType_MsgVote, rd.Messages[0].Type)
	require.Equal(t, voterId, rd.Messages[0].To)
	require.Equal(t, Candidate, n.state)

	require.NoError(t, n.Step(&raftpb.Message{
		Type:   raftpb.MessageType_MsgVoteResp,
		From:   voterId,
		To:     n.id,
		Term:   n.term,
		Reject: true,
	}))
	require.Equal(t, Candidate, n.state)
}

func TestCandidateBecomesLeaderAfterMajorityVoteResponses_036h(t *testing.T) {
	voterId := uint64(2)

	n := newTestRaft(1)
	n.peers = append(n.peers, voterId)

	require.NoError(t, n.Campaign())
	require.Equal(t, Candidate, n.state)

	require.NoError(t, n.Step(&raftpb.Message{
		Type:   raftpb.MessageType_MsgVoteResp,
		From:   voterId,
		To:     n.id,
		Term:   1,
		Reject: false,
	}))
	require.Equal(t, Leader, n.state)
}

func TestVoteRejectedForOlderTerm_036h(t *testing.T) {
	n := newTestRaft(1)
	n.term = 2

	require.NoError(t, n.Step(&raftpb.Message{
		Type: raftpb.MessageType_MsgVote,
		Term: 1,
	}))
	rd := n.Ready()
	require.Len(t, rd.Messages, 1)
	require.Equal(t, raftpb.MessageType_MsgVoteResp, rd.Messages[0].Type)
	require.True(t, rd.Messages[0].Reject)
}

func TestVoteRejectedWhenExistingVote_036h(t *testing.T) {
	n := newTestRaft(2)
	n.term = 1
	n.votedFor = 3

	require.NoError(t, n.Step(&raftpb.Message{
		Type: raftpb.MessageType_MsgVote,
		From: 1,
		Term: 1,
	}))
	rd := n.Ready()
	require.Len(t, rd.Messages, 1)
	require.Equal(t, raftpb.MessageType_MsgVoteResp, rd.Messages[0].Type)
	require.True(t, rd.Messages[0].Reject)
}

func TestVoteGrantUpdatesTermAndVotedFor_036h(t *testing.T) {
	n := newTestRaft(2)
	n.term = 1

	require.NoError(t, n.Step(&raftpb.Message{
		Type: raftpb.MessageType_MsgVote,
		From: 1,
		Term: 2,
	}))

	require.Equal(t, uint64(2), n.term)
	require.Equal(t, uint64(1), n.votedFor)
}

func TestGrantingHigherTermVoteStepsDownToFollower_036h(t *testing.T) {
	n := newTestRaft(2)
	n.term = 1
	n.state = Candidate

	require.NoError(t, n.Step(&raftpb.Message{
		Type: raftpb.MessageType_MsgVote,
		From: 1,
		Term: 2,
	}))
	require.Equal(t, Follower, n.state)
}

func TestStaleVoteRespIgnored_036h(t *testing.T) {
	voterId := uint64(2)

	n := newTestRaft(1)
	n.term = 1
	n.peers = append(n.peers, voterId)

	require.NoError(t, n.Campaign())
	require.NoError(t, n.Step(&raftpb.Message{
		Type:   raftpb.MessageType_MsgVoteResp,
		From:   voterId,
		To:     n.id,
		Term:   1,
		Reject: false,
	}))
	require.Equal(t, Candidate, n.state)
}

func TestVoteRejectedWhenCandidateLastLogTermIsOlder_036i(t *testing.T) {
	n := newTestRaft(1)
	n.term = 2
	n.log = append(n.log, &raftpb.Entry{Term: 2, Index: 1})

	require.NoError(t, n.Step(&raftpb.Message{
		Type:    raftpb.MessageType_MsgVote,
		Term:    3,
		LogTerm: 1,
		Index:   1,
	}))

	rd := n.Ready()
	require.Len(t, rd.Messages, 1)
	require.True(t, rd.Messages[0].Reject)
}

func TestVoteRejectedWhenCandidateLastLogIndexIsLowerInSameLastTerm_036i(t *testing.T) {
	n := newTestRaft(1)
	n.term = 2
	n.log = append(n.log, &raftpb.Entry{Term: 2, Index: 2})

	require.NoError(t, n.Step(&raftpb.Message{
		Type:    raftpb.MessageType_MsgVote,
		Term:    3,
		LogTerm: 2,
		Index:   1,
	}))

	rd := n.Ready()
	require.Len(t, rd.Messages, 1)
	require.True(t, rd.Messages[0].Reject)
}

func TestCampaignIncludesLastLogPositionInVoteRequest_036i(t *testing.T) {
	n := newTestRaft(1)
	n.term = 2
	n.log = append(n.log, &raftpb.Entry{Term: 2, Index: 2})
	n.peers = append(n.peers, 2)

	require.NoError(t, n.Campaign())

	rd := n.Ready()
	require.Len(t, rd.Messages, 1)
	require.Equal(t, uint64(2), rd.Messages[0].LogTerm)
	require.Equal(t, uint64(2), rd.Messages[0].Index)
}

func TestRejectingHigherTermStaleVoteUpdatesTermAndClearsOldVote_036j(t *testing.T) {
	n := newTestRaft(2)
	n.term = 1
	n.state = Candidate
	n.votedFor = n.id
	n.log = append(n.log, &raftpb.Entry{Term: 1, Index: 2})

	require.NoError(t, n.Step(&raftpb.Message{
		Type:    raftpb.MessageType_MsgVote,
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
	n := newTestRaft(2)
	n.term = 1
	n.state = Candidate
	n.votedFor = n.id
	n.log = append(n.log, &raftpb.Entry{Term: 1, Index: 2})

	require.NoError(t, n.Step(&raftpb.Message{
		Type:    raftpb.MessageType_MsgVote,
		From:    1,
		Term:    2,
		LogTerm: 0,
		Index:   0,
	}))
	n.messages = nil

	require.NoError(t, n.Step(&raftpb.Message{
		Type:    raftpb.MessageType_MsgVote,
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
	n := newTestRaft(1)
	n.peers = append(n.peers, 2)

	require.NoError(t, n.Campaign())
	require.Equal(t, Candidate, n.state)
	require.Equal(t, uint64(1), n.term)
	require.Equal(t, n.id, n.votedFor)

	require.NoError(t, n.Step(&raftpb.Message{
		Type:   raftpb.MessageType_MsgVoteResp,
		From:   2,
		To:     n.id,
		Term:   2,
		Reject: false,
	}))

	require.Equal(t, Follower, n.state)
	require.Equal(t, uint64(2), n.term)
	require.Zero(t, n.votedFor)
	require.Empty(t, n.votes)
}

func TestHigherTermVoteResponseClearsSelfVoteForLaterGrant_036j(t *testing.T) {
	n := newTestRaft(2)
	n.peers = append(n.peers, 1, 3)

	require.NoError(t, n.Campaign())
	require.Equal(t, Candidate, n.state)

	require.NoError(t, n.Step(&raftpb.Message{
		Type:   raftpb.MessageType_MsgVoteResp,
		From:   1,
		To:     n.id,
		Term:   2,
		Reject: true,
	}))
	require.Equal(t, Follower, n.state)
	n.messages = nil

	require.NoError(t, n.Step(&raftpb.Message{
		Type:    raftpb.MessageType_MsgVote,
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
	n := newTestRaft(2)
	n.term = 1
	n.state = Candidate
	n.votedFor = n.id
	n.votes = map[uint64]bool{n.id: true, 1: false}

	require.NoError(t, n.Step(&raftpb.Message{
		Type:    raftpb.MessageType_MsgApp,
		From:    1,
		To:      n.id,
		Term:    2,
		Entries: []*raftpb.Entry{{Index: 1, Term: 2, Data: []byte("x")}},
	}))

	require.Equal(t, uint64(2), n.term)
	require.Equal(t, Follower, n.state)
	require.Zero(t, n.votedFor)
	require.Empty(t, n.votes)

	rd := n.Ready()
	require.Len(t, rd.Entries, 1)
	require.Equal(t, &raftpb.Entry{Index: 1, Term: 2, Data: []byte("x")}, rd.Entries[0])
	require.Len(t, rd.Messages, 1)
	require.Equal(t, raftpb.MessageType_MsgAppResp, rd.Messages[0].Type)
	require.Equal(t, uint64(2), rd.Messages[0].Term)
	require.Equal(t, uint64(2), rd.Messages[0].LogTerm)
}

// Superseded by 036n: per-follower progress replaces per-entry ack tracking
//
// func TestHigherTermAppendRespRevokesStaleLeaderAuthorityBeforeAckTracking_036j(t *testing.T) {
// 	leader := newLeaderRaftWithOnePeer()
// 	leader.votedFor = leader.id
// 	leader.votes = map[uint64]bool{leader.id: true, 2: false}
// 	require.NoError(t, leader.Propose([]byte("foo")))

// 	id := entryID{index: 1, term: 1}
// 	require.False(t, leader.acks[id][2])

// 	require.NoError(t, leader.Step(&raftpb.Message{
// 		Type:    raftpb.MessageType_MsgAppResp,
// 		From:    2,
// 		To:      leader.id,
// 		Term:    2,
// 		Index:   1,
// 		LogTerm: 1,
// 	}))

// 	require.Equal(t, uint64(2), leader.term)
// 	require.Equal(t, Follower, leader.state)
// 	require.Zero(t, leader.votedFor)
// 	require.Nil(t, leader.votes)
// 	require.False(t, leader.acks[id][2])
// 	require.Zero(t, leader.commitIndex)
// }

func TestMsgAppIsRejectedWhenAnchorCannotBeProved_036k(t *testing.T) {
	follower := newTestRaft(2)

	require.NoError(t, follower.Step(&raftpb.Message{
		Type:    raftpb.MessageType_MsgApp,
		Term:    2,
		Index:   1,
		LogTerm: 2,
		Entries: []*raftpb.Entry{{Index: 2, Term: 2, Data: []byte("x")}},
	}))

	rd := follower.Ready()
	require.Len(t, rd.Messages, 1)
	require.True(t, rd.Messages[0].Reject)
}

func TestMsgSnapIsReadyWhenRepairCrossesCompactionBoundary_036k(t *testing.T) {
	leader := newLeaderRaftWithOnePeer()
	leader.storage = &mockStorage{
		firstIndex: 5,
		snap: &raftpb.SnapshotMeta{
			LastIncludedIndex: 4,
			LastIncludedTerm:  1,
		},
	}

	require.NoError(t, leader.Step(&raftpb.Message{
		Type:       raftpb.MessageType_MsgAppResp,
		From:       2,
		To:         leader.id,
		Term:       leader.term,
		Reject:     true,
		RejectHint: 3,
	}))

	rd := leader.Ready()
	require.Len(t, rd.Messages, 1)
	require.Equal(t, raftpb.MessageType_MsgSnap, rd.Messages[0].Type)
	require.Equal(t, leader.id, rd.Messages[0].From)
	require.Equal(t, uint64(2), rd.Messages[0].To)
	require.Equal(t, &raftpb.SnapshotMeta{LastIncludedIndex: 4, LastIncludedTerm: 1}, rd.Messages[0].Snapshot)
}

func TestFollowerInstallsSnapshotBoundaryFromMsgSnap_036k(t *testing.T) {
	storage := &mockStorage{}
	follower := newTestRaft(2)
	follower.storage = storage
	follower.log = []*raftpb.Entry{{Index: 1, Term: 1, Data: []byte("old")}, {Index: 2, Term: 1, Data: []byte("older")}}
	follower.lastLogIndex = 2

	snap := &raftpb.SnapshotMeta{LastIncludedIndex: 4, LastIncludedTerm: 2}
	require.NoError(t, follower.Step(&raftpb.Message{
		Type:     raftpb.MessageType_MsgSnap,
		From:     1,
		To:       follower.id,
		Term:     3,
		Snapshot: snap,
	}))

	require.Equal(t, uint64(3), follower.term)
	require.Equal(t, Follower, follower.state)
	require.Equal(t, []*raftpb.SnapshotMeta{snap}, storage.applied)
	require.Equal(t, snap, storage.snap)
	require.Empty(t, follower.log)
	require.Equal(t, uint64(4), follower.lastLogIndex)
	require.Equal(t, uint64(4), follower.commitIndex)
	require.Equal(t, uint64(4), follower.appliedIndex)
	require.False(t, follower.anchorExists(2, 1))
	require.True(t, follower.anchorExists(4, 2))
}

func TestAppendAfterInstalledSnapshotIsReady_036k(t *testing.T) {
	storage := &mockStorage{}
	follower := newTestRaft(2)
	follower.storage = storage

	snap := &raftpb.SnapshotMeta{LastIncludedIndex: 4, LastIncludedTerm: 2}
	require.NoError(t, follower.Step(&raftpb.Message{
		Type:     raftpb.MessageType_MsgSnap,
		From:     1,
		To:       follower.id,
		Term:     3,
		Snapshot: snap,
	}))

	require.NoError(t, follower.Step(&raftpb.Message{
		Type:    raftpb.MessageType_MsgApp,
		From:    1,
		To:      follower.id,
		Term:    3,
		Index:   4,
		LogTerm: 2,
		Entries: []*raftpb.Entry{{Index: 5, Term: 3, Data: []byte("x")}},
	}))

	rd := follower.Ready()
	require.Len(t, rd.Entries, 1)
	require.Equal(t, &raftpb.Entry{Index: 5, Term: 3, Data: []byte("x")}, rd.Entries[0])
}

func TestProgressInitializedOnElection_036n(t *testing.T) {
	candidate := newTestRaft(1)
	candidate.peers = []uint64{2}
	candidate.becomeCandidate()
	candidate.votes[2] = false

	require.NoError(t, candidate.Step(&raftpb.Message{
		Type:   raftpb.MessageType_MsgVoteResp,
		From:   2,
		Term:   candidate.term,
		Reject: false,
	}))

	require.Len(t, candidate.progress, 1)
	require.Equal(t, Progress{MatchIndex: 0, NextIndex: 1}, *candidate.progress[2])
}

func TestProposeSendsEntriesFromNextIndex_036n(t *testing.T) {
	leader := newLeaderRaftWithOnePeer()
	leader.log = []*raftpb.Entry{
		{Index: 1, Term: 1},
	}
	leader.progress = make(map[uint64]*Progress)
	leader.progress[2] = &Progress{
		MatchIndex: 0,
		NextIndex:  1,
	}

	require.NoError(t, leader.Propose([]byte("foo")))

	rd := leader.Ready()
	require.Equal(t, []*raftpb.Entry{
		{Index: 1, Term: 1},
		{Index: 2, Term: 1, Data: []byte("foo")},
	}, rd.Messages[0].Entries)
}

func TestCommitAdvancesOnMajorityMatch_036n(t *testing.T) {
	leader := newLeaderRaftWithOnePeer()
	leader.peers = append(leader.peers, 3)

	leader.lastLogIndex = 3
	leader.log = []*raftpb.Entry{
		{Index: 1, Term: 1},
		{Index: 2, Term: 1},
		{Index: 3, Term: 1},
	}

	leader.progress = make(map[uint64]*Progress)
	leader.progress[2] = &Progress{MatchIndex: 0, NextIndex: 1}
	leader.progress[3] = &Progress{MatchIndex: 1, NextIndex: 2}

	require.NoError(t, leader.Step(&raftpb.Message{
		Type:  raftpb.MessageType_MsgAppResp,
		From:  2,
		Index: 2,
	}))
	// matches: [leader=3, peer2=2, peer3=1] → median=2, not max=3
	require.Equal(t, uint64(2), leader.commitIndex)
}

func TestFollowerAdvancesCommitFromMsgApp_036n(t *testing.T) {
	follower := newTestRaft(2)
	follower.term = 1
	follower.lastLogIndex = 1

	require.Equal(t, uint64(0), follower.commitIndex)
	require.NoError(t, follower.Step(&raftpb.Message{
		Type:    raftpb.MessageType_MsgApp,
		Term:    1,
		Commit:  1,
		Entries: []*raftpb.Entry{{Index: 1, Term: 1, Data: []byte("foo")}},
	}))

	require.Equal(t, uint64(1), follower.commitIndex)
}

func TestNextIndexBacksUpOnRejection_036n(t *testing.T) {
	leader := newLeaderRaftWithOnePeer()
	leader.progress = make(map[uint64]*Progress)
	leader.progress[2] = &Progress{MatchIndex: 2, NextIndex: 4}

	require.NoError(t, leader.Step(&raftpb.Message{
		Type:   raftpb.MessageType_MsgAppResp,
		From:   2,
		Reject: true,
	}))

	// conservative backup: decrement by 1. Update this assertion
	// when the RejectHint fast path is introduced.
	require.Equal(t, uint64(3), leader.progress[2].NextIndex)
}

// --- 036t: The Tick ---

func TestTickElectionProducesMsgHup_036t(t *testing.T) {
	r := newTestRaft(1)
	r.peers = []uint64{2}

	// Tick past the randomized election timeout.
	// randomizedElectionTimeout ∈ [electionTimeout, 2*electionTimeout).
	for i := 0; i < 2*r.electionTimeout; i++ {
		r.tick()
	}

	// The follower should have become a candidate via MsgHup → Step → becomeCandidate.
	require.Equal(t, Candidate, r.state)

	// MsgVote should be ready for the peer.
	rd := r.Ready()
	require.Len(t, rd.Messages, 1)
	require.Equal(t, raftpb.MessageType_MsgVote, rd.Messages[0].Type)
	require.Equal(t, uint64(2), rd.Messages[0].To)
}

func TestTickHeartbeatProducesMsgApp_036t(t *testing.T) {
	leader := newLeaderRaftWithOnePeer()
	leader.Advance()

	// Tick past heartbeatTimeout.
	for i := 0; i < leader.heartbeatTimeout; i++ {
		leader.tick()
	}

	rd := leader.Ready()
	require.Len(t, rd.Messages, 1)
	require.Equal(t, raftpb.MessageType_MsgApp, rd.Messages[0].Type)
	require.Equal(t, uint64(2), rd.Messages[0].To)
	require.Equal(t, leader.term, rd.Messages[0].Term)
	// Heartbeat carries no entries, just commit index.
	require.Empty(t, rd.Messages[0].Entries)
}

func TestCandidateReElectsAtHigherTermOnTimeout_036t(t *testing.T) {
	r := newTestRaft(1)
	r.peers = []uint64{2, 3}

	// First campaign — becomes candidate at term 1.
	require.NoError(t, r.Campaign())
	require.Equal(t, Candidate, r.state)
	require.Equal(t, uint64(1), r.term)
	r.Advance()

	// Election times out — tick past randomizedElectionTimeout.
	for i := 0; i < 2*r.electionTimeout; i++ {
		r.tick()
	}

	// Must re-campaign at a higher term.
	require.Equal(t, Candidate, r.state)
	require.Equal(t, uint64(2), r.term)
	require.Equal(t, r.id, r.votedFor)

	rd := r.Ready()
	// MsgVote sent to both peers at the new term.
	require.Len(t, rd.Messages, 2)
	for _, msg := range rd.Messages {
		require.Equal(t, raftpb.MessageType_MsgVote, msg.Type)
		require.Equal(t, uint64(2), msg.Term)
	}
}

func TestNodeStepRejectsLocalMessages_036t(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	n := newStartedNode(1, 2)

	localTypes := []raftpb.MessageType{
		raftpb.MessageType_MsgHup,
		raftpb.MessageType_MsgBeat,
	}
	for _, mt := range localTypes {
		err := n.Step(ctx, &raftpb.Message{Type: mt, From: 99})
		require.Error(t, err, "Node.Step should reject local message type %v", mt)
	}
}

func TestNodeStepAcceptsMsgProp_037c(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Set up a leader node so MsgProp is actually processed.
	n := setupNode(Config{ID: 1, Peers: []uint64{2}, Storage: &mockStorage{}, ElectionTick: 10, HeartbeatTick: 1})
	defer n.Stop()
	n.r.becomeLeader()
	go n.run()

	// MsgProp arriving over "the network" must be accepted by Node.Step.
	err := n.Step(ctx, &raftpb.Message{
		Type:    raftpb.MessageType_MsgProp,
		From:    2,
		Entries: []*raftpb.Entry{{Data: []byte("hello")}},
	})
	require.NoError(t, err)

	// The leader should have appended the entry.
	rd := <-n.Ready()
	require.True(t, len(rd.Entries) > 0, "leader should have appended forwarded proposal")
}

func TestFollowerResetsTimerOnLeaderContact_036t(t *testing.T) {
	r := newTestRaft(2)
	r.peers = []uint64{1}
	r.becomeFollower(1, 1)

	// Tick close to election timeout, but send MsgApp from leader each time
	// to reset the counter.
	for round := 0; round < 5; round++ {
		for i := 0; i < r.electionTimeout-1; i++ {
			r.tick()
		}
		// Leader heartbeat resets electionElapsed.
		require.NoError(t, r.Step(&raftpb.Message{
			Type: raftpb.MessageType_MsgApp,
			From: 1,
			To:   2,
			Term: 1,
		}))
	}

	// Despite many ticks, still a follower because heartbeats kept arriving.
	require.Equal(t, Follower, r.state)
}

func TestThreeNodeClusterSelfElects_036t(t *testing.T) {
	nodes := make([]*Raft, 3)
	for i := range nodes {
		id := uint64(i + 1)
		peers := make([]uint64, 0, 2)
		for j := range nodes {
			if uint64(j+1) != id {
				peers = append(peers, uint64(j+1))
			}
		}
		nodes[i] = newTestRaft(id)
		nodes[i].peers = peers
	}

	// Deliver messages between nodes for up to 100 ticks.
	for tick := 0; tick < 100; tick++ {
		for _, n := range nodes {
			n.tick()
		}
		// Collect and deliver all messages.
		for _, src := range nodes {
			rd := src.Ready()
			for _, msg := range rd.Messages {
				for _, dst := range nodes {
					if dst.id == msg.To {
						_ = dst.Step(msg)
					}
				}
			}
			src.Advance()
		}

		// Check if any node became leader.
		for _, n := range nodes {
			if n.state == Leader {
				return // success
			}
		}
	}
	t.Fatal("no leader elected within 100 ticks")
}

func TestSplitVoteRecovery_036t(t *testing.T) {
	// 3-node cluster: if two of three campaign simultaneously,
	// randomized timeouts must break the tie within bounded rounds.
	nodes := make([]*Raft, 3)
	for i := range nodes {
		id := uint64(i + 1)
		peers := make([]uint64, 0, 2)
		for j := range nodes {
			if uint64(j+1) != id {
				peers = append(peers, uint64(j+1))
			}
		}
		nodes[i] = newTestRaft(id)
		nodes[i].peers = peers
	}

	// Force two candidates simultaneously by calling Campaign.
	require.NoError(t, nodes[0].Campaign())
	require.NoError(t, nodes[1].Campaign())

	// Deliver messages and tick for up to 200 ticks.
	for tick := 0; tick < 200; tick++ {
		for _, n := range nodes {
			n.tick()
		}
		for _, src := range nodes {
			rd := src.Ready()
			for _, msg := range rd.Messages {
				for _, dst := range nodes {
					if dst.id == msg.To {
						_ = dst.Step(msg)
					}
				}
			}
			src.Advance()
		}

		for _, n := range nodes {
			if n.state == Leader {
				return // success — split vote resolved
			}
		}
	}
	t.Fatal("split vote not resolved within 200 ticks")
}

func TestNewRaftRestoresPersistedState_036t(t *testing.T) {
	storage := &mockStorage{}
	storage.hardState = &raftpb.HardState{Term: 5, VotedFor: 2, CommittedIndex: 3}

	r := newRaft(Config{
		ID:            1,
		Storage:       storage,
		ElectionTick:  10,
		HeartbeatTick: 1,
		Logger:        slog.Default(),
	})

	require.Equal(t, uint64(5), r.term)
	require.Equal(t, uint64(2), r.votedFor)
	require.Equal(t, uint64(3), r.commitIndex)
	require.Equal(t, Follower, r.state)
}

// ---------------------------------------------------------------------------
// 037c — MsgProp Forwarding
// ---------------------------------------------------------------------------

func TestFollowerForwardsMsgPropToLeader_037c(t *testing.T) {
	r := newTestRaft(2)
	r.peers = []uint64{1}
	r.becomeFollower(1, 1) // term=1, leader=1

	err := r.Step(&raftpb.Message{
		Type:    raftpb.MessageType_MsgProp,
		Entries: []*raftpb.Entry{{Data: []byte("hello")}},
	})
	require.NoError(t, err)

	rd := r.Ready()
	require.Len(t, rd.Messages, 1)
	msg := rd.Messages[0]
	require.Equal(t, raftpb.MessageType_MsgProp, msg.Type)
	require.Equal(t, uint64(1), msg.To) // forwarded to leader
	require.Equal(t, []byte("hello"), msg.Entries[0].Data)
}

func TestFollowerDropsMsgPropWhenNoLeader_037c(t *testing.T) {
	r := newTestRaft(2)
	r.peers = []uint64{1}
	r.becomeFollower(1, None) // term=1, no leader known

	err := r.Step(&raftpb.Message{
		Type:    raftpb.MessageType_MsgProp,
		Entries: []*raftpb.Entry{{Data: []byte("hello")}},
	})
	require.ErrorIs(t, err, ErrProposalDropped)

	rd := r.Ready()
	require.Empty(t, rd.Messages)
}

func TestSendStampsFromOnAllMessages_037c(t *testing.T) {
	r := newTestRaft(1)
	r.peers = []uint64{2}
	r.becomeLeader()
	r.Advance()

	// Propose produces MsgApp to peer — verify From is stamped.
	require.NoError(t, r.Propose([]byte("data")))
	rd := r.Ready()
	require.True(t, len(rd.Messages) > 0)
	for _, msg := range rd.Messages {
		require.Equal(t, uint64(1), msg.From, "send should stamp From on %s", msg.Type)
		require.Equal(t, r.term, msg.Term, "send should stamp Term on %s", msg.Type)
	}
}

func TestSendStampsFromOnForwardedProp_037c(t *testing.T) {
	r := newTestRaft(2)
	r.peers = []uint64{1}
	r.becomeFollower(1, 1)

	// MsgProp forwarded from follower — From was unset, send should fill it.
	require.NoError(t, r.Step(&raftpb.Message{
		Type:    raftpb.MessageType_MsgProp,
		Entries: []*raftpb.Entry{{Data: []byte("hello")}},
	}))
	rd := r.Ready()
	require.Len(t, rd.Messages, 1)
	require.Equal(t, uint64(2), rd.Messages[0].From, "send should stamp From on forwarded MsgProp")
	require.Equal(t, uint64(0), rd.Messages[0].Term, "MsgProp should carry no term")
}

func TestCandidateDropsMsgProp_037c(t *testing.T) {
	r := newTestRaft(1)
	r.peers = []uint64{2, 3}
	r.becomeCandidate()

	err := r.Step(&raftpb.Message{
		Type:    raftpb.MessageType_MsgProp,
		Entries: []*raftpb.Entry{{Data: []byte("hello")}},
	})
	require.ErrorIs(t, err, ErrProposalDropped)

	rd := r.Ready()
	// No outbound messages from the proposal — only the vote requests from becomeCandidate.
	for _, msg := range rd.Messages {
		require.Equal(t, raftpb.MessageType_MsgVote, msg.Type, "candidate should not forward MsgProp")
	}
}

func TestLeaderPanicsOnEmptyMsgProp_037c(t *testing.T) {
	r := newTestRaft(1)
	r.peers = []uint64{2}
	r.becomeLeader()
	r.Advance()

	require.Panics(t, func() {
		_ = r.Step(&raftpb.Message{
			Type:    raftpb.MessageType_MsgProp,
			Entries: []*raftpb.Entry{},
		})
	}, "empty MsgProp should panic — it indicates a programming error")
}

func TestStaleLeaderReforwardsMsgProp_037c(t *testing.T) {
	// Node 2 was leader, stepped down to follower in term 2 with leader=3.
	r := newTestRaft(2)
	r.peers = []uint64{1, 3}
	r.becomeFollower(2, 3) // term=2, leader=3

	// A MsgProp arrives from node 1 (forwarded during old term when node 2 was leader).
	// Term=0 (proposals carry no term), so the m.Term > r.term guard doesn't fire.
	err := r.Step(&raftpb.Message{
		Type:    raftpb.MessageType_MsgProp,
		From:    1,
		Entries: []*raftpb.Entry{{Data: []byte("re-forward")}},
	})
	require.NoError(t, err)

	rd := r.Ready()
	require.Len(t, rd.Messages, 1)
	msg := rd.Messages[0]
	require.Equal(t, raftpb.MessageType_MsgProp, msg.Type)
	require.Equal(t, uint64(3), msg.To, "should re-forward to the new leader")
	require.Equal(t, uint64(1), msg.From, "From preserves original proposer — send() only fills None")
	require.Equal(t, []byte("re-forward"), msg.Entries[0].Data)
}
