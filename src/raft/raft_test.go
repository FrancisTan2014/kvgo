package raft

import (
	"context"
	"fmt"
	"log/slog"
	"testing"

	"kvgo/raftpb"

	"github.com/stretchr/testify/require"
)

type mockStorage struct {
	firstIndex uint64
	entries    []*raftpb.Entry
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
	if len(m.entries) == 0 {
		return nil, nil
	}
	first := m.entries[0].Index
	start := int(lo - first)
	end := int(hi - first)
	if start < 0 || end > len(m.entries) {
		return nil, fmt.Errorf("out of range [%d,%d) in entries [%d,%d]",
			lo, hi, first, m.entries[len(m.entries)-1].Index)
	}
	result := make([]*raftpb.Entry, end-start)
	copy(result, m.entries[start:end])
	return result, nil
}

func (m *mockStorage) FirstIndex() uint64 {
	if len(m.entries) > 0 {
		return m.entries[0].Index
	}
	return m.firstIndex
}

func (m *mockStorage) LastIndex() uint64 {
	if len(m.entries) > 0 {
		return m.entries[len(m.entries)-1].Index
	}
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

func (m *mockStorage) Term(i uint64) (uint64, error) {
	if m.snap != nil && i == m.snap.LastIncludedIndex {
		return m.snap.LastIncludedTerm, nil
	}
	if len(m.entries) > 0 {
		first := m.entries[0].Index
		if i >= first && i <= m.entries[len(m.entries)-1].Index {
			return m.entries[int(i-first)].Term, nil
		}
	}
	return 0, ErrUnavailable
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

// newLeaderRaftWithOnePeer returns a leader (id=1) in term 1 with one peer.
// 037m: commits and drains the no-op entry appended by becomeLeader (added
// in 037l) and advances peer progress past it, so tests start with a clean
// Ready slate where the next Propose produces exactly one new entry.
func newLeaderRaftWithOnePeer() *Raft {
	r := newTestRaft(1)
	r.peers = []uint64{2}
	r.term = 1
	r.becomeLeader()
	// Commit the no-op (simulates quorum ack) so appliedIndex advances past it.
	r.CommitTo(r.lastLogIndex)
	r.Advance()
	// Simulate the peer acking the no-op so NextIndex starts past it.
	for _, pr := range r.progress {
		pr.NextIndex = r.lastLogIndex + 1
		pr.MatchIndex = r.lastLogIndex
	}
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
	// 037m: commitIndex is already 1 from the no-op committed in the helper.
	initialCommit := r.commitIndex
	require.NoError(t, r.Propose([]byte("")))
	require.Equal(t, initialCommit, r.commitIndex, "Propose must not advance commitIndex")
}

func TestReadyReturnsProposedEntry_036b(t *testing.T) {
	r := newLeaderRaftWithOnePeer()
	require.NoError(t, r.Propose([]byte("")))
	ready := r.Ready()
	require.Len(t, ready.Entries, 1, "only the proposed entry; no-op drained by helper (037m)")
	require.Len(t, ready.CommittedEntries, 0)
}

func TestAdvanceClearReadyEntries_036b(t *testing.T) {
	r := newLeaderRaftWithOnePeer()
	require.NoError(t, r.Propose([]byte("foo")))
	// 037m: no-op is at index 1, proposed entry at index 2.
	r.CommitTo(2)

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

	// 037m: proposed entry is at index 2 (no-op at 1).
	r.CommitTo(2)
	require.Equal(t, uint64(2), r.commitIndex)
}

func TestCommitToNeverDecreasesCommitIndex_036b(t *testing.T) {
	r := newLeaderRaftWithOnePeer()
	require.NoError(t, r.Propose([]byte("foo")))

	// 037m: proposed entry is at index 2.
	r.CommitTo(2)
	require.Equal(t, uint64(2), r.commitIndex)

	r.CommitTo(0)
	require.Equal(t, uint64(2), r.commitIndex)
}

func TestReadyExposesCommittedEntsOnlyAfterCommitTo_036b(t *testing.T) {
	r := newLeaderRaftWithOnePeer()
	require.NoError(t, r.Propose([]byte("foo")))

	ready := r.Ready()
	require.Len(t, ready.CommittedEntries, 0)

	// 037m: proposed entry is at index 2.
	r.CommitTo(2)
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
	// 037m: proposed entry is at index 2.
	r.CommitTo(2)

	rd := r.Ready()
	require.Equal(t, &raftpb.HardState{Term: 1, VotedFor: 0, CommittedIndex: 2}, rd.HardState)
}

func TestHardStateChangesWithoutMessagesOrEntries_036m(t *testing.T) {
	r := newLeaderRaftWithOnePeer()

	require.NoError(t, r.Step(&raftpb.Message{Type: raftpb.MessageType_MsgApp, From: 2, To: 1, Term: 2}))
	// 037m: after higher-term MsgApp, commitIndex resets to 0 via becomeFollower→reset.
	// But reset only resets commitIndex if term changes. CommitTo never decreases.
	// The MsgApp at term 2 triggers becomeFollower(2, 2). reset(2) clears lead but
	// does NOT reset commitIndex. So commitIndex stays at 1 from the helper.
	require.Equal(t, &raftpb.HardState{Term: 2, VotedFor: 0, CommittedIndex: 1}, r.HardState())
}

func TestAdvanceDoesNotChangeHardState_036m(t *testing.T) {
	r := newLeaderRaftWithOnePeer()

	require.NoError(t, r.Step(&raftpb.Message{Type: raftpb.MessageType_MsgApp, From: 2, To: 1, Term: 2}))
	// 037m: commitIndex is preserved at 1 from the helper (CommitTo never decreases).
	require.Equal(t, &raftpb.HardState{Term: 2, VotedFor: 0, CommittedIndex: 1}, r.HardState())

	r.Advance()
	require.Equal(t, &raftpb.HardState{Term: 2, VotedFor: 0, CommittedIndex: 1}, r.HardState())
}

func TestFullLifecycle_036b(t *testing.T) {
	r := newLeaderRaftWithOnePeer()
	require.NoError(t, r.Propose([]byte("x")))

	// Phase 1: unstable only (no-op already drained by helper — 037m)
	rd := r.Ready()
	require.Len(t, rd.Entries, 1)
	require.Len(t, rd.CommittedEntries, 0) // ← invariant: not applied yet
	r.Advance()

	// Phase 2: committed (037m: proposed entry is at index 2)
	r.CommitTo(2)
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
	require.Len(t, rd.Messages, 1, "no-op MsgApp drained by helper (037m)")
	require.Len(t, rd.CommittedEntries, 0)

	msg := rd.Messages[0]
	require.Equal(t, raftpb.MessageType_MsgApp, msg.Type)
	require.Len(t, msg.Entries, 1)
	// 037m: proposed entry is at index 2 (no-op at 1).
	require.Equal(t, uint64(2), msg.Entries[0].Index)
	require.Equal(t, []byte("foo"), msg.Entries[0].Data)
}

func TestNewEntryIsReadyAfterFollowerStepMsgApp_036f(t *testing.T) {
	// 037m: don't use the helper — we need NextIndex=1 so the MsgApp
	// carries entries from the beginning, including the no-op, so a
	// fresh follower (empty log) can accept them (prevIndex=0).
	leader := newTestRaft(1)
	leader.peers = []uint64{2}
	leader.term = 1
	leader.becomeLeader()
	leader.CommitTo(leader.lastLogIndex)
	leader.Advance()
	// Keep progress[2].NextIndex at 1 (not yet acked).

	require.NoError(t, leader.Propose([]byte("foo")))

	rd := leader.Ready()
	require.Len(t, rd.Messages, 1)
	msg := rd.Messages[0]
	// MsgApp carries all entries from NextIndex=1: [no-op, "foo"].
	require.Equal(t, raftpb.MessageType_MsgApp, msg.Type)

	follower := newTestRaft(2)
	require.NoError(t, follower.Step(msg))

	frd := follower.Ready()
	require.GreaterOrEqual(t, len(frd.Entries), 1)
	last := frd.Entries[len(frd.Entries)-1]
	require.Equal(t, []byte("foo"), last.Data)

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
	// 037m: filter out any non-snapshot messages (MsgApp for no-op may be pending).
	var snaps []*raftpb.Message
	for _, m := range rd.Messages {
		if m.Type == raftpb.MessageType_MsgSnap {
			snaps = append(snaps, m)
		}
	}
	require.Len(t, snaps, 1)
	require.Equal(t, leader.id, snaps[0].From)
	require.Equal(t, uint64(2), snaps[0].To)
	require.Equal(t, &raftpb.SnapshotMeta{LastIncludedIndex: 4, LastIncludedTerm: 1}, snaps[0].Snapshot)
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
	// 037m: becomeLeader appended a no-op at index 1. Rebuild log and
	// progress to test NextIndex-based entry slicing independently.
	leader.log = []*raftpb.Entry{
		{Index: 1, Term: 1},
	}
	leader.lastLogIndex = 1
	leader.progress = make(map[uint64]*Progress)
	leader.progress[2] = &Progress{
		MatchIndex: 0,
		NextIndex:  1,
	}

	require.NoError(t, leader.Propose([]byte("foo")))

	rd := leader.Ready()
	// Find the MsgApp carrying the proposed entry.
	var appMsg *raftpb.Message
	for _, m := range rd.Messages {
		if m.Type == raftpb.MessageType_MsgApp {
			appMsg = m
		}
	}
	require.NotNil(t, appMsg)
	require.Equal(t, []*raftpb.Entry{
		{Index: 1, Term: 1},
		{Index: 2, Term: 1, Data: []byte("foo")},
	}, appMsg.Entries)
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
	// 037m: matchTerm needs storage to verify the entry's term.
	leader.storage = &mockStorage{
		entries: []*raftpb.Entry{
			{Index: 1, Term: 1},
			{Index: 2, Term: 1},
			{Index: 3, Term: 1},
		},
	}

	leader.progress = make(map[uint64]*Progress)
	leader.progress[2] = &Progress{MatchIndex: 0, NextIndex: 1}
	leader.progress[3] = &Progress{MatchIndex: 1, NextIndex: 2}

	require.NoError(t, leader.Step(&raftpb.Message{
		Type:  raftpb.MessageType_MsgAppResp,
		From:  2,
		Term:  leader.term,
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

// Superseded by 037h: heartbeats now use dedicated MsgHeartbeat instead of empty MsgApp
//
// func TestTickHeartbeatProducesMsgApp_036t(t *testing.T) {
// 	leader := newLeaderRaftWithOnePeer()
// 	leader.Advance()
//
// 	// Tick past heartbeatTimeout.
// 	for i := 0; i < leader.heartbeatTimeout; i++ {
// 		leader.tick()
// 	}
//
// 	rd := leader.Ready()
// 	require.Len(t, rd.Messages, 1)
// 	require.Equal(t, raftpb.MessageType_MsgApp, rd.Messages[0].Type)
// 	require.Equal(t, uint64(2), rd.Messages[0].To)
// 	require.Equal(t, leader.term, rd.Messages[0].Term)
// 	// Heartbeat carries no entries, just commit index.
// 	require.Empty(t, rd.Messages[0].Entries)
// }

func TestTickHeartbeatProducesMsgHeartbeat_037h(t *testing.T) {
	leader := newLeaderRaftWithOnePeer()
	leader.Advance()

	// Tick past heartbeatTimeout.
	for i := 0; i < leader.heartbeatTimeout; i++ {
		leader.tick()
	}

	rd := leader.Ready()
	require.Len(t, rd.Messages, 1)
	require.Equal(t, raftpb.MessageType_MsgHeartbeat, rd.Messages[0].Type)
	require.Equal(t, uint64(2), rd.Messages[0].To)
	require.Equal(t, leader.term, rd.Messages[0].Term)
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

// ---------------------------------------------------------------------------
// 037e — The Bootstrap
// ---------------------------------------------------------------------------

func storageWithEntries(entries []*raftpb.Entry, hs *raftpb.HardState) *mockStorage {
	return &mockStorage{
		entries:   entries,
		hardState: hs,
	}
}

func TestRestartedRaftLoadsLogFromStorage_037e(t *testing.T) {
	entries := []*raftpb.Entry{
		{Index: 1, Term: 1, Data: []byte("a")},
		{Index: 2, Term: 1, Data: []byte("b")},
		{Index: 3, Term: 2, Data: []byte("c")},
	}
	hs := &raftpb.HardState{Term: 2, VotedFor: 1, CommittedIndex: 2}

	r := newRaft(Config{
		ID:            1,
		Storage:       storageWithEntries(entries, hs),
		Applied:       2,
		ElectionTick:  10,
		HeartbeatTick: 1,
		Logger:        slog.Default(),
	})

	require.Equal(t, uint64(3), r.lastLogIndex)
	require.Len(t, r.log, 3)
	require.Equal(t, uint64(1), r.log[0].Index)
	require.Equal(t, uint64(3), r.log[2].Index)
	require.Equal(t, []byte("c"), r.log[2].Data)
	require.Equal(t, uint64(3), r.stableIndex)
	require.Equal(t, uint64(2), r.appliedIndex)
}

func TestRestartedRaftDoesNotReEmitStableEntries_037e(t *testing.T) {
	entries := []*raftpb.Entry{
		{Index: 1, Term: 1, Data: []byte("a")},
		{Index: 2, Term: 1, Data: []byte("b")},
	}
	hs := &raftpb.HardState{Term: 1, CommittedIndex: 2}

	r := newRaft(Config{
		ID:            1,
		Storage:       storageWithEntries(entries, hs),
		Applied:       2,
		ElectionTick:  10,
		HeartbeatTick: 1,
		Logger:        slog.Default(),
	})

	rd := r.Ready()
	require.Empty(t, rd.Entries, "stable entries should not be re-emitted")
}

func TestRestartedRaftPreservesVoteAndTerm_037e(t *testing.T) {
	entries := []*raftpb.Entry{
		{Index: 1, Term: 5, Data: []byte("x")},
	}
	hs := &raftpb.HardState{Term: 5, VotedFor: 2, CommittedIndex: 1}

	r := newRaft(Config{
		ID:            1,
		Storage:       storageWithEntries(entries, hs),
		Applied:       1,
		ElectionTick:  10,
		HeartbeatTick: 1,
		Logger:        slog.Default(),
	})

	require.Equal(t, uint64(5), r.term)
	require.Equal(t, uint64(2), r.votedFor)

	// Vote request in same term from a different node should be rejected.
	err := r.Step(&raftpb.Message{
		Type:    raftpb.MessageType_MsgVote,
		From:    3,
		Term:    5,
		Index:   1,
		LogTerm: 5,
	})
	require.NoError(t, err)

	rd := r.Ready()
	require.Len(t, rd.Messages, 1)
	require.True(t, rd.Messages[0].Reject, "should reject vote: already voted for 2 in term 5")
}

func TestRestartedFollowerReceivesNewEntries_037e(t *testing.T) {
	entries := []*raftpb.Entry{
		{Index: 1, Term: 1, Data: []byte("a")},
		{Index: 2, Term: 1, Data: []byte("b")},
		{Index: 3, Term: 1, Data: []byte("c")},
		{Index: 4, Term: 2, Data: []byte("d")},
		{Index: 5, Term: 2, Data: []byte("e")},
	}
	hs := &raftpb.HardState{Term: 2, CommittedIndex: 5}

	r := newRaft(Config{
		ID:            1,
		Peers:         []uint64{2, 3},
		Storage:       storageWithEntries(entries, hs),
		Applied:       5,
		ElectionTick:  10,
		HeartbeatTick: 1,
		Logger:        slog.Default(),
	})

	// Leader sends MsgApp with new entries 6–7.
	err := r.Step(&raftpb.Message{
		Type:    raftpb.MessageType_MsgApp,
		From:    2,
		Term:    2,
		Index:   5,
		LogTerm: 2,
		Entries: []*raftpb.Entry{
			{Index: 6, Term: 2, Data: []byte("f")},
			{Index: 7, Term: 2, Data: []byte("g")},
		},
	})
	require.NoError(t, err)

	require.Equal(t, uint64(7), r.lastLogIndex)
	require.Len(t, r.log, 7)
	require.Equal(t, []byte("g"), r.log[6].Data)
}

func TestRestartedNodeDoesNotDisruptLeader_037e(t *testing.T) {
	entries := []*raftpb.Entry{
		{Index: 1, Term: 3, Data: []byte("x")},
	}
	hs := &raftpb.HardState{Term: 3, CommittedIndex: 1}

	r := newRaft(Config{
		ID:            2,
		Peers:         []uint64{1, 3},
		Storage:       storageWithEntries(entries, hs),
		Applied:       1,
		ElectionTick:  10,
		HeartbeatTick: 1,
		Logger:        slog.Default(),
	})

	require.Equal(t, Follower, r.state)
	require.Equal(t, uint64(3), r.term)

	// Leader heartbeat suppresses election timer.
	r.Step(&raftpb.Message{
		Type: raftpb.MessageType_MsgApp,
		From: 1,
		Term: 3,
	})
	require.Equal(t, Follower, r.state)
	require.Equal(t, uint64(1), r.lead)
}

func TestEmptyStorageStartsFresh_037e(t *testing.T) {
	r := newRaft(Config{
		ID:            1,
		Peers:         []uint64{2, 3},
		Storage:       &mockStorage{},
		ElectionTick:  10,
		HeartbeatTick: 1,
		Logger:        slog.Default(),
	})

	require.Equal(t, uint64(0), r.lastLogIndex)
	require.Empty(t, r.log)
	require.Equal(t, uint64(0), r.stableIndex)
	require.Equal(t, uint64(0), r.appliedIndex)
	require.Equal(t, Follower, r.state)
}

func TestAppendEntriesSkipsDuplicates_037e(t *testing.T) {
	r := newTestRaft(1)
	r.appendEntries([]*raftpb.Entry{
		{Index: 1, Term: 1, Data: []byte("a")},
		{Index: 2, Term: 1, Data: []byte("b")},
		{Index: 3, Term: 1, Data: []byte("c")},
	})

	// Overlapping append with same terms — should be a no-op.
	r.appendEntries([]*raftpb.Entry{
		{Index: 2, Term: 1, Data: []byte("b")},
		{Index: 3, Term: 1, Data: []byte("c")},
	})

	require.Len(t, r.log, 3)
	require.Equal(t, uint64(3), r.lastLogIndex)
}

func TestAppendEntriesTruncatesOnConflict_037e(t *testing.T) {
	r := newTestRaft(1)
	r.appendEntries([]*raftpb.Entry{
		{Index: 1, Term: 1, Data: []byte("a")},
		{Index: 2, Term: 1, Data: []byte("b")},
		{Index: 3, Term: 1, Data: []byte("c")},
	})

	// Leader sends entries 2–4 with a new term at index 2.
	// Entries 2–3 (term 1) should be truncated, replaced by term 2.
	r.appendEntries([]*raftpb.Entry{
		{Index: 2, Term: 2, Data: []byte("B")},
		{Index: 3, Term: 2, Data: []byte("C")},
		{Index: 4, Term: 2, Data: []byte("D")},
	})

	require.Len(t, r.log, 4)
	require.Equal(t, uint64(4), r.lastLogIndex)
	require.Equal(t, uint64(2), r.log[1].Term, "entry 2 should have new term")
	require.Equal(t, []byte("B"), r.log[1].Data)
	require.Equal(t, []byte("D"), r.log[3].Data)
}

func TestAppendEntriesExtendsLog_037e(t *testing.T) {
	r := newTestRaft(1)
	r.appendEntries([]*raftpb.Entry{
		{Index: 1, Term: 1, Data: []byte("a")},
		{Index: 2, Term: 1, Data: []byte("b")},
	})

	// Overlapping with match, then extending.
	r.appendEntries([]*raftpb.Entry{
		{Index: 2, Term: 1, Data: []byte("b")},
		{Index: 3, Term: 1, Data: []byte("c")},
	})

	require.Len(t, r.log, 3)
	require.Equal(t, uint64(3), r.lastLogIndex)
	require.Equal(t, []byte("c"), r.log[2].Data)
}

func TestConflictingAppendEmitsReplacementEntriesInReady_037e(t *testing.T) {
	r := newTestRaft(1)
	r.appendEntries([]*raftpb.Entry{
		{Index: 1, Term: 1, Data: []byte("a")},
		{Index: 2, Term: 1, Data: []byte("b")},
		{Index: 3, Term: 1, Data: []byte("c")},
	})
	// Simulate persisting all entries and advancing.
	r.stableIndex = 3

	// Conflicting append at index 2: truncates [2,3] and appends [2,3,4] with term 2.
	r.appendEntries([]*raftpb.Entry{
		{Index: 2, Term: 2, Data: []byte("B")},
		{Index: 3, Term: 2, Data: []byte("C")},
		{Index: 4, Term: 2, Data: []byte("D")},
	})

	// stableIndex must be reset to the truncation point (index 1).
	require.Equal(t, uint64(1), r.stableIndex)

	// Ready().Entries must contain the replacement entries for persistence.
	rd := r.Ready()
	require.Len(t, rd.Entries, 3)
	require.Equal(t, uint64(2), rd.Entries[0].Index)
	require.Equal(t, uint64(2), rd.Entries[0].Term)
}

func TestRestartWithSnapshotAndEntriesLoadsCorrectly_037e(t *testing.T) {
	storage := &mockStorage{
		snap: &raftpb.SnapshotMeta{LastIncludedIndex: 10, LastIncludedTerm: 2},
		entries: []*raftpb.Entry{
			{Index: 11, Term: 2, Data: []byte("x")},
			{Index: 12, Term: 3, Data: []byte("y")},
			{Index: 13, Term: 3, Data: []byte("z")},
		},
		hardState: &raftpb.HardState{Term: 3, VotedFor: 2, CommittedIndex: 12},
	}

	r := newRaft(Config{
		ID:            1,
		Peers:         []uint64{1, 2, 3},
		Storage:       storage,
		Applied:       10,
		ElectionTick:  10,
		HeartbeatTick: 1,
	})

	require.Len(t, r.log, 3)
	require.Equal(t, uint64(13), r.lastLogIndex)
	require.Equal(t, uint64(13), r.stableIndex)
	require.Equal(t, uint64(10), r.appliedIndex)
	require.Equal(t, uint64(11), r.log[0].Index)
}

// 037g — The Replay
// ---------------------------------------------------------------------------

func TestConfigAppliedReplaysUnappliedEntries_037g(t *testing.T) {
	entries := []*raftpb.Entry{
		{Index: 1, Term: 1, Data: []byte("a")},
		{Index: 2, Term: 1, Data: []byte("b")},
		{Index: 3, Term: 1, Data: []byte("c")},
		{Index: 4, Term: 1, Data: []byte("d")},
		{Index: 5, Term: 1, Data: []byte("e")},
		{Index: 6, Term: 1, Data: []byte("f")},
		{Index: 7, Term: 1, Data: []byte("g")},
		{Index: 8, Term: 1, Data: []byte("h")},
		{Index: 9, Term: 1, Data: []byte("i")},
		{Index: 10, Term: 1, Data: []byte("j")},
	}
	hs := &raftpb.HardState{Term: 1, CommittedIndex: 10}

	r := newRaft(Config{
		ID:            1,
		Storage:       storageWithEntries(entries, hs),
		Applied:       0,
		ElectionTick:  10,
		HeartbeatTick: 1,
		Logger:        slog.Default(),
	})

	rd := r.Ready()
	require.Len(t, rd.CommittedEntries, 10)
	require.Equal(t, uint64(1), rd.CommittedEntries[0].Index)
	require.Equal(t, uint64(10), rd.CommittedEntries[9].Index)
}

func TestConfigAppliedSkipsAlreadyAppliedEntries_037g(t *testing.T) {
	entries := []*raftpb.Entry{
		{Index: 1, Term: 1, Data: []byte("a")},
		{Index: 2, Term: 1, Data: []byte("b")},
		{Index: 3, Term: 1, Data: []byte("c")},
		{Index: 4, Term: 1, Data: []byte("d")},
		{Index: 5, Term: 1, Data: []byte("e")},
		{Index: 6, Term: 1, Data: []byte("f")},
		{Index: 7, Term: 1, Data: []byte("g")},
		{Index: 8, Term: 1, Data: []byte("h")},
		{Index: 9, Term: 1, Data: []byte("i")},
		{Index: 10, Term: 1, Data: []byte("j")},
	}
	hs := &raftpb.HardState{Term: 1, CommittedIndex: 10}

	r := newRaft(Config{
		ID:            1,
		Storage:       storageWithEntries(entries, hs),
		Applied:       7,
		ElectionTick:  10,
		HeartbeatTick: 1,
		Logger:        slog.Default(),
	})

	rd := r.Ready()
	require.Len(t, rd.CommittedEntries, 3)
	require.Equal(t, uint64(8), rd.CommittedEntries[0].Index)
	require.Equal(t, uint64(10), rd.CommittedEntries[2].Index)
}

func TestConfigAppliedGreaterThanCommitIndexPanics_037g(t *testing.T) {
	entries := []*raftpb.Entry{
		{Index: 1, Term: 1, Data: []byte("a")},
		{Index: 2, Term: 1, Data: []byte("b")},
		{Index: 3, Term: 1, Data: []byte("c")},
		{Index: 4, Term: 1, Data: []byte("d")},
		{Index: 5, Term: 1, Data: []byte("e")},
		{Index: 6, Term: 1, Data: []byte("f")},
		{Index: 7, Term: 1, Data: []byte("g")},
		{Index: 8, Term: 1, Data: []byte("h")},
		{Index: 9, Term: 1, Data: []byte("i")},
		{Index: 10, Term: 1, Data: []byte("j")},
	}
	hs := &raftpb.HardState{Term: 1, CommittedIndex: 10}

	require.Panics(t, func() {
		newRaft(Config{
			ID:            1,
			Storage:       storageWithEntries(entries, hs),
			Applied:       15,
			ElectionTick:  10,
			HeartbeatTick: 1,
			Logger:        slog.Default(),
		})
	})
}

func TestConfigAppliedBelowFirstIndexPanics_037g(t *testing.T) {
	// Entries 1–10 compacted, entries 11–20 remain. Applied=5 claims a
	// position inside the compacted range — raft can't replay entries 6–10.
	storage := &mockStorage{
		snap: &raftpb.SnapshotMeta{LastIncludedIndex: 10, LastIncludedTerm: 1},
		entries: []*raftpb.Entry{
			{Index: 11, Term: 1}, {Index: 12, Term: 1},
			{Index: 13, Term: 1}, {Index: 14, Term: 1},
			{Index: 15, Term: 1},
		},
		hardState: &raftpb.HardState{Term: 1, CommittedIndex: 15},
	}

	require.Panics(t, func() {
		newRaft(Config{
			ID:            1,
			Storage:       storage,
			Applied:       5,
			ElectionTick:  10,
			HeartbeatTick: 1,
			Logger:        slog.Default(),
		})
	})
}

func TestFreshStartWithZeroApplied_037g(t *testing.T) {
	r := newRaft(Config{
		ID:            1,
		Peers:         []uint64{2, 3},
		Storage:       &mockStorage{},
		Applied:       0,
		ElectionTick:  10,
		HeartbeatTick: 1,
		Logger:        slog.Default(),
	})

	require.Equal(t, uint64(0), r.appliedIndex)
	require.Equal(t, uint64(0), r.commitIndex)
	require.False(t, r.HasReady())
}

func TestReplayAfterCompactionBoundedByLog_037g(t *testing.T) {
	// Entries 1–10 compacted, entries 11–20 remain in log.
	storage := &mockStorage{
		snap: &raftpb.SnapshotMeta{LastIncludedIndex: 10, LastIncludedTerm: 1},
		entries: []*raftpb.Entry{
			{Index: 11, Term: 1, Data: []byte("k")},
			{Index: 12, Term: 1, Data: []byte("l")},
			{Index: 13, Term: 1, Data: []byte("m")},
			{Index: 14, Term: 1, Data: []byte("n")},
			{Index: 15, Term: 1, Data: []byte("o")},
			{Index: 16, Term: 1, Data: []byte("p")},
			{Index: 17, Term: 1, Data: []byte("q")},
			{Index: 18, Term: 1, Data: []byte("r")},
			{Index: 19, Term: 1, Data: []byte("s")},
			{Index: 20, Term: 1, Data: []byte("t")},
		},
		hardState: &raftpb.HardState{Term: 1, CommittedIndex: 20},
	}

	// With Applied = snapshot boundary, replay entries 11–20.
	r1 := newRaft(Config{
		ID:            1,
		Peers:         []uint64{2, 3},
		Storage:       storage,
		Applied:       10,
		ElectionTick:  10,
		HeartbeatTick: 1,
		Logger:        slog.Default(),
	})
	rd1 := r1.Ready()
	require.Len(t, rd1.CommittedEntries, 10)
	require.Equal(t, uint64(11), rd1.CommittedEntries[0].Index)

	// With Applied = 0, still get entries 11–20 — compacted entries are gone.
	r2 := newRaft(Config{
		ID:            1,
		Peers:         []uint64{2, 3},
		Storage:       storage,
		Applied:       0,
		ElectionTick:  10,
		HeartbeatTick: 1,
		Logger:        slog.Default(),
	})
	rd2 := r2.Ready()
	require.Len(t, rd2.CommittedEntries, 10)
	require.Equal(t, uint64(11), rd2.CommittedEntries[0].Index)
	require.Equal(t, uint64(20), rd2.CommittedEntries[9].Index)
}

// ---------------------------------------------------------------------------
// 037h — The Heartbeat
// ---------------------------------------------------------------------------

func TestHeartbeatCarriesCappedCommitIndex_037h(t *testing.T) {
	r := newTestRaft(1)
	r.peers = []uint64{2, 3}
	r.term = 1
	r.becomeLeader()

	// Simulate different progress per peer.
	r.commitIndex = 10
	r.progress[2] = &Progress{MatchIndex: 7, NextIndex: 8}
	r.progress[3] = &Progress{MatchIndex: 10, NextIndex: 11}
	r.Advance()

	require.NoError(t, r.Step(&raftpb.Message{From: r.id, Type: raftpb.MessageType_MsgBeat}))

	rd := r.Ready()
	require.Len(t, rd.Messages, 2)

	for _, msg := range rd.Messages {
		require.Equal(t, raftpb.MessageType_MsgHeartbeat, msg.Type)
		if msg.To == 2 {
			require.Equal(t, uint64(7), msg.Commit, "commit to peer 2 should be capped at MatchIndex=7")
		} else {
			require.Equal(t, uint64(10), msg.Commit, "commit to peer 3 should be commitIndex=10")
		}
	}
}

func TestFollowerResetsElectionTimerOnHeartbeat_037h(t *testing.T) {
	r := newTestRaft(2)
	r.peers = []uint64{1}
	r.becomeFollower(1, 1)

	// Advance election timer partway.
	for i := 0; i < r.electionTimeout-2; i++ {
		r.tick()
	}
	require.True(t, r.electionElapsed > 0)

	require.NoError(t, r.Step(&raftpb.Message{
		Type: raftpb.MessageType_MsgHeartbeat,
		From: 1,
		Term: 1,
	}))
	require.Equal(t, 0, r.electionElapsed)
}

func TestFollowerAdvancesCommitOnHeartbeat_037h(t *testing.T) {
	r := newTestRaft(2)
	r.peers = []uint64{1}
	r.becomeFollower(1, 1)
	r.log = []*raftpb.Entry{
		{Index: 1, Term: 1}, {Index: 2, Term: 1}, {Index: 3, Term: 1},
		{Index: 4, Term: 1}, {Index: 5, Term: 1}, {Index: 6, Term: 1},
		{Index: 7, Term: 1}, {Index: 8, Term: 1}, {Index: 9, Term: 1},
		{Index: 10, Term: 1},
	}
	r.lastLogIndex = 10
	r.commitIndex = 5

	require.NoError(t, r.Step(&raftpb.Message{
		Type:   raftpb.MessageType_MsgHeartbeat,
		From:   1,
		Term:   1,
		Commit: 8,
	}))
	require.Equal(t, uint64(8), r.commitIndex)
}

func TestFollowerRespondsWithMsgHeartbeatResp_037h(t *testing.T) {
	r := newTestRaft(2)
	r.peers = []uint64{1}
	r.becomeFollower(1, 1)

	require.NoError(t, r.Step(&raftpb.Message{
		Type: raftpb.MessageType_MsgHeartbeat,
		From: 1,
		Term: 1,
	}))

	rd := r.Ready()
	require.Len(t, rd.Messages, 1)
	require.Equal(t, raftpb.MessageType_MsgHeartbeatResp, rd.Messages[0].Type)
	require.Equal(t, uint64(1), rd.Messages[0].To)
}

func TestLeaderSendsMsgAppAfterHeartbeatRespFromBehindPeer_037h(t *testing.T) {
	r := newTestRaft(1)
	r.peers = []uint64{2}
	r.term = 1
	r.becomeLeader()
	r.log = []*raftpb.Entry{
		{Index: 1, Term: 1, Data: []byte("a")},
		{Index: 2, Term: 1, Data: []byte("b")},
		{Index: 3, Term: 1, Data: []byte("c")},
	}
	r.lastLogIndex = 3
	r.progress[2] = &Progress{MatchIndex: 1, NextIndex: 2}
	r.Advance()

	require.NoError(t, r.Step(&raftpb.Message{
		Type: raftpb.MessageType_MsgHeartbeatResp,
		From: 2,
		Term: 1,
	}))

	rd := r.Ready()
	require.Len(t, rd.Messages, 1)
	require.Equal(t, raftpb.MessageType_MsgApp, rd.Messages[0].Type)
	require.Equal(t, uint64(2), rd.Messages[0].To)
	// Should send entries from NextIndex onward.
	require.True(t, len(rd.Messages[0].Entries) > 0, "should send entries to catch up behind peer")
}

func TestLeaderSkipsMsgAppAfterHeartbeatRespFromCaughtUpPeer_037h(t *testing.T) {
	r := newTestRaft(1)
	r.peers = []uint64{2}
	r.term = 1
	r.becomeLeader()
	r.log = []*raftpb.Entry{
		{Index: 1, Term: 1, Data: []byte("a")},
	}
	r.lastLogIndex = 1
	r.progress[2] = &Progress{MatchIndex: 1, NextIndex: 2}
	r.Advance()

	require.NoError(t, r.Step(&raftpb.Message{
		Type: raftpb.MessageType_MsgHeartbeatResp,
		From: 2,
		Term: 1,
	}))

	rd := r.Ready()
	require.Empty(t, rd.Messages, "caught-up peer should not receive MsgApp")
}

func TestHeartbeatDoesNotInterfereWithReplication_037h(t *testing.T) {
	// 037m: use helper that drains the no-op from becomeLeader.
	r := newLeaderRaftWithOnePeer()
	// 037m: matchTerm needs storage to verify entry terms for maybeCommit.
	r.storage = &mockStorage{
		entries: []*raftpb.Entry{
			{Index: 1, Term: 1},
		},
	}

	// Propose an entry — normal replication.
	require.NoError(t, r.Propose([]byte("x")))
	// 037m: update storage to include the proposed entry.
	r.storage = &mockStorage{
		entries: []*raftpb.Entry{
			{Index: 1, Term: 1},
			{Index: 2, Term: 1, Data: []byte("x")},
		},
	}
	rd := r.Ready()
	require.Len(t, rd.Messages, 1)
	require.Equal(t, raftpb.MessageType_MsgApp, rd.Messages[0].Type)
	appMsg := rd.Messages[0]
	r.Advance()

	// Follower acks via MsgAppResp — leader advances commit.
	// 037m: MsgApp carries entries from NextIndex; ack the last entry.
	lastEntry := appMsg.Entries[len(appMsg.Entries)-1]
	require.NoError(t, r.Step(&raftpb.Message{
		Type:  raftpb.MessageType_MsgAppResp,
		From:  2,
		Term:  1,
		Index: lastEntry.Index,
	}))
	require.Equal(t, lastEntry.Index, r.commitIndex)
	r.Advance()

	// Now tick heartbeat — should produce MsgHeartbeat, not interfere.
	for i := 0; i < r.heartbeatTimeout; i++ {
		r.tick()
	}
	rd = r.Ready()
	require.Len(t, rd.Messages, 1)
	require.Equal(t, raftpb.MessageType_MsgHeartbeat, rd.Messages[0].Type)
}

func TestHeartbeatRespSendsSnapshotWhenAnchorIsCompacted_037h(t *testing.T) {
	r := newTestRaft(1)
	r.peers = []uint64{2}
	r.term = 1
	r.becomeLeader()
	r.storage = &mockStorage{
		snap: &raftpb.SnapshotMeta{LastIncludedIndex: 10, LastIncludedTerm: 1},
		entries: []*raftpb.Entry{
			{Index: 11, Term: 1, Data: []byte("a")},
			{Index: 12, Term: 1, Data: []byte("b")},
		},
	}
	r.log = []*raftpb.Entry{
		{Index: 11, Term: 1, Data: []byte("a")},
		{Index: 12, Term: 1, Data: []byte("b")},
	}
	r.lastLogIndex = 12
	// Peer is far behind — NextIndex points into the compacted range.
	r.progress[2] = &Progress{MatchIndex: 0, NextIndex: 5}
	r.Advance()

	require.NoError(t, r.Step(&raftpb.Message{
		Type: raftpb.MessageType_MsgHeartbeatResp,
		From: 2,
		Term: 1,
	}))

	rd := r.Ready()
	require.Len(t, rd.Messages, 1)
	require.Equal(t, raftpb.MessageType_MsgSnap, rd.Messages[0].Type)
	require.Equal(t, uint64(10), rd.Messages[0].Snapshot.LastIncludedIndex)
}

// --- 037l ReadIndex tests ---

// #14 becomeLeader appends a no-op entry in the new term so that Raft §5.4.2
// can commit previous-term entries and so that `committedEntryInCurrentTerm`
// will eventually flip to true (unblocking ReadIndex).
func TestBecomeLeaderAppendsNoOpEntry_037l(t *testing.T) {
	r := newTestRaft(1)
	r.peers = []uint64{2}
	r.term = 1
	r.becomeLeader()

	require.Len(t, r.log, 1, "becomeLeader must append exactly one no-op entry")
	require.Equal(t, uint64(1), r.log[0].Index)
	require.Equal(t, uint64(1), r.log[0].Term)
	require.Nil(t, r.log[0].Data, "no-op entry must carry nil Data")
}

// #6 A single-node cluster has trivial quorum (itself). No heartbeat broadcast
// is needed — stepLeader short-circuits MsgReadIndex and immediately appends a
// ReadState.
func TestSingletonClusterReadIndexSkipsHeartbeat_037l(t *testing.T) {
	r := newTestRaft(1)
	r.peers = nil
	r.term = 1
	r.becomeLeader()
	r.Advance() // drain becomeLeader's Ready

	rctx := []byte("rctx-singleton")
	require.NoError(t, r.Step(&raftpb.Message{
		Type:    raftpb.MessageType_MsgReadIndex,
		From:    1,
		Context: rctx,
	}))

	rd := r.Ready()
	for _, m := range rd.Messages {
		require.NotEqual(t, raftpb.MessageType_MsgHeartbeat, m.Type,
			"singleton ReadIndex must not broadcast heartbeats")
	}
	require.Len(t, rd.ReadStates, 1)
	require.Equal(t, rctx, rd.ReadStates[0].RequestCtx)
}

// #15 A follower with no known leader, or a candidate mid-election, has no
// upstream to forward MsgReadIndex to. Both fast-fail with ErrProposalDropped
// so the caller sees the failure immediately instead of blocking to its ctx
// deadline.
func TestStepFollowerReadIndexWithoutLeaderIsDropped_037l(t *testing.T) {
	r := newTestRaft(1)
	r.becomeFollower(1, None)

	err := r.Step(&raftpb.Message{
		Type:    raftpb.MessageType_MsgReadIndex,
		Context: []byte("rctx"),
	})
	require.ErrorIs(t, err, ErrProposalDropped)
}

func TestStepCandidateReadIndexIsDropped_037l(t *testing.T) {
	r := newTestRaft(1)
	r.peers = []uint64{2}
	r.becomeCandidate()

	err := r.Step(&raftpb.Message{
		Type:    raftpb.MessageType_MsgReadIndex,
		Context: []byte("rctx"),
	})
	require.ErrorIs(t, err, ErrProposalDropped)
}

// #13 maybeCommit must refuse to advance commitIndex over a previous-term
// entry (Raft §5.4.2). It advances only after a current-term entry commits at
// a higher index, which then carries the prior-term entries with it.
func TestMaybeCommitSkipsPreviousTermMedian_037l(t *testing.T) {
	r := newTestRaft(1)
	r.peers = []uint64{2, 3}
	r.term = 2
	r.state = Leader
	r.step = stepLeader
	r.log = []*raftpb.Entry{
		{Index: 1, Term: 1},
		{Index: 2, Term: 1},
		{Index: 3, Term: 2}, // current-term no-op
	}
	r.lastLogIndex = 3
	// storage.Term must resolve each index so matchTerm works.
	r.storage = &mockStorage{
		entries: []*raftpb.Entry{
			{Index: 1, Term: 1},
			{Index: 2, Term: 1},
			{Index: 3, Term: 2},
		},
	}

	require.False(t, r.maybeCommit(2), "median at prior-term index must be rejected")
	require.Zero(t, r.commitIndex, "commitIndex must not advance on prior-term median")

	require.True(t, r.maybeCommit(3), "median at current-term index must commit")
	require.Equal(t, uint64(3), r.commitIndex, "commitIndex jumps past prior-term entries")
}

// #1 Leader ReadIndex with quorum — single reader. After quorum acks, Raft
// emits a ReadState carrying the recorded commitIndex and the original rctx.
// (Concurrent-reader correlation is #16.)
func TestLeaderReadIndexReturnsReadState_037l(t *testing.T) {
	r := newTestRaft(1)
	r.peers = []uint64{2, 3}
	r.term = 1
	r.becomeLeader()
	// Pre-populate storage so matchTerm(1) is true. Without this the
	// committedEntryInCurrentTerm guard would queue the request.
	r.storage = &mockStorage{
		entries: []*raftpb.Entry{{Index: 1, Term: 1}},
	}
	r.commitIndex = 1
	r.Advance() // drain becomeLeader's Ready

	rctx := []byte("rctx-solo")
	require.NoError(t, r.Step(&raftpb.Message{
		Type:    raftpb.MessageType_MsgReadIndex,
		From:    1,
		Context: rctx,
	}))

	// Leader broadcasts heartbeat carrying rctx; no ReadState yet.
	rd := r.Ready()
	var heartbeats int
	for _, m := range rd.Messages {
		if m.Type == raftpb.MessageType_MsgHeartbeat {
			heartbeats++
			require.Equal(t, rctx, m.Context)
		}
	}
	require.Equal(t, 2, heartbeats, "heartbeat to every peer")
	require.Empty(t, rd.ReadStates, "ReadState must wait for quorum ack")
	r.Advance()

	// One peer ack pushes the leader to quorum (self already acked).
	require.NoError(t, r.Step(&raftpb.Message{
		Type:    raftpb.MessageType_MsgHeartbeatResp,
		From:    2,
		Context: rctx,
	}))

	rd = r.Ready()
	require.Len(t, rd.ReadStates, 1)
	require.Equal(t, uint64(1), rd.ReadStates[0].Index)
	require.Equal(t, rctx, rd.ReadStates[0].RequestCtx)
}

// #16 Two concurrent ReadIndex requests produce two ReadStates, each keyed by
// its originating rctx. A later quorum proof subsumes all earlier ones —
// leadership is monotonic within a term — so acking the second request also
// releases the first.
func TestLeaderReadIndexConcurrentRctxCorrelation_037l(t *testing.T) {
	r := newTestRaft(1)
	r.peers = []uint64{2, 3}
	r.term = 1
	r.becomeLeader()
	r.storage = &mockStorage{
		entries: []*raftpb.Entry{{Index: 1, Term: 1}},
	}
	r.commitIndex = 1
	r.Advance()

	rctxA := []byte("rctx-A")
	rctxB := []byte("rctx-B")
	require.NoError(t, r.Step(&raftpb.Message{
		Type: raftpb.MessageType_MsgReadIndex, From: 1, Context: rctxA,
	}))
	require.NoError(t, r.Step(&raftpb.Message{
		Type: raftpb.MessageType_MsgReadIndex, From: 1, Context: rctxB,
	}))
	r.Ready() // drain heartbeats
	r.Advance()

	// Peer 2 acks B. readOnly.advance drains the queue up to and including
	// B — releasing A too (subsumed by the later quorum proof).
	require.NoError(t, r.Step(&raftpb.Message{
		Type: raftpb.MessageType_MsgHeartbeatResp, From: 2, Context: rctxB,
	}))

	rd := r.Ready()
	require.Len(t, rd.ReadStates, 2, "both rctx must surface together")
	require.Equal(t, rctxA, rd.ReadStates[0].RequestCtx, "queue order preserved: A first")
	require.Equal(t, rctxB, rd.ReadStates[1].RequestCtx)
	require.Equal(t, uint64(1), rd.ReadStates[0].Index)
	require.Equal(t, uint64(1), rd.ReadStates[1].Index)
}

// #2 Follower forwards MsgReadIndex to its known leader via raft transport.
// The follower must not emit a ReadState on its own — it has no authority to
// prove commitIndex. The outbound MsgReadIndex carries the original rctx and
// targets r.lead.
func TestFollowerForwardsMsgReadIndexToLeader_037l(t *testing.T) {
	r := newTestRaft(2)
	r.peers = []uint64{1, 3}
	r.becomeFollower(1, 1) // term=1, lead=1
	r.Advance()

	rctx := []byte("rctx-fwd")
	require.NoError(t, r.Step(&raftpb.Message{
		Type:    raftpb.MessageType_MsgReadIndex,
		From:    2,
		Context: rctx,
	}))

	rd := r.Ready()
	require.Empty(t, rd.ReadStates, "follower must not emit its own ReadState")
	require.Len(t, rd.Messages, 1)
	fwd := rd.Messages[0]
	require.Equal(t, raftpb.MessageType_MsgReadIndex, fwd.Type)
	require.Equal(t, uint64(1), fwd.To, "forwarded to known leader")
	require.Equal(t, rctx, fwd.Context, "rctx preserved across forward")
}

// #3 A leader that cannot reach a majority (heartbeats go out but no
// MsgHeartbeatResp returns) never emits a ReadState. The request stays
// pending in readOnly until quorum arrives or the caller times out.
func TestLeaderWithoutQuorumDoesNotEmitReadState_037l(t *testing.T) {
	r := newTestRaft(1)
	r.peers = []uint64{2, 3, 4, 5} // quorum = 3
	r.term = 1
	r.becomeLeader()
	r.storage = &mockStorage{
		entries: []*raftpb.Entry{{Index: 1, Term: 1}},
	}
	r.commitIndex = 1
	r.Advance()

	require.NoError(t, r.Step(&raftpb.Message{
		Type:    raftpb.MessageType_MsgReadIndex,
		From:    1,
		Context: []byte("rctx-nq"),
	}))
	r.Ready() // drain heartbeats
	r.Advance()

	// Only one peer acks — not enough for quorum (self + 1 = 2 < 3).
	require.NoError(t, r.Step(&raftpb.Message{
		Type: raftpb.MessageType_MsgHeartbeatResp, From: 2, Context: []byte("rctx-nq"),
	}))

	rd := r.Ready()
	require.Empty(t, rd.ReadStates, "no quorum → no ReadState emitted")
}

// #5 A ReadIndex arriving before the leader has committed any entry in its
// own term is queued into pendingReadIndexRequests. After a current-term
// entry commits (carrying commitIndex forward via maybeCommit), the queue
// drains and the request is processed.
func TestReadIndexPostponedUntilCurrentTermCommit_037l(t *testing.T) {
	r := newTestRaft(1)
	r.peers = []uint64{2}
	r.term = 2
	r.becomeLeader()
	// becomeLeader appended a term-2 no-op at index 1. Pre-populate storage
	// so matchTerm can resolve index 1's term. commitIndex stays at 0.
	r.storage = &mockStorage{
		entries: []*raftpb.Entry{{Index: 1, Term: 2}},
	}
	require.Zero(t, r.commitIndex, "no current-term commit yet")
	r.Advance() // drain becomeLeader's Ready

	rctx := []byte("rctx-pending")
	require.NoError(t, r.Step(&raftpb.Message{
		Type:    raftpb.MessageType_MsgReadIndex,
		From:    1,
		Context: rctx,
	}))
	require.Len(t, r.pendingReadIndexRequests, 1, "queued because no current-term commit yet")

	rd := r.Ready()
	require.Empty(t, rd.ReadStates, "no ReadState while queued")
	for _, m := range rd.Messages {
		require.NotEqual(t, raftpb.MessageType_MsgHeartbeat, m.Type, "no heartbeat while queued")
	}
	r.Advance()

	// Peer acks index 1 (current-term no-op) — maybeCommit advances, drains.
	require.NoError(t, r.Step(&raftpb.Message{
		Type:  raftpb.MessageType_MsgAppResp,
		From:  2,
		Term:  2,
		Index: 1,
	}))
	require.Equal(t, uint64(1), r.commitIndex, "no-op committed via maybeCommit")
	require.Empty(t, r.pendingReadIndexRequests, "queue drained on commit")

	// The drain uses the no-op commit itself as proof of quorum — maybeCommit
	// only advances on a current-term commit, which already required quorum
	// replication. No fresh heartbeat needed; ReadState emitted directly.
	rd = r.Ready()
	require.Len(t, rd.ReadStates, 1)
	require.Equal(t, rctx, rd.ReadStates[0].RequestCtx)
	require.Equal(t, uint64(1), rd.ReadStates[0].Index)
}

// --- 037m: CheckQuorum ---
//
// Invariant: a leader that cannot confirm quorum activity within one
// election-timeout window demotes itself to follower at the same term
// with lead = None.

// newThreeNodeLeader returns a leader (id=1) in term 1 with peers [2,3].
// The no-op from becomeLeader is committed and Ready is drained.
func newThreeNodeLeader(t *testing.T) *Raft {
	t.Helper()
	r := newTestRaft(1)
	r.peers = []uint64{2, 3}
	r.term = 1
	r.becomeLeader()
	r.storage = &mockStorage{
		entries: []*raftpb.Entry{{Index: 1, Term: 1}},
	}
	r.commitIndex = 1
	r.Advance()
	return r
}

// tickElectionWindow ticks tickHeartbeat enough times to trigger MsgCheckQuorum.
func tickElectionWindow(r *Raft) {
	for i := 0; i < r.electionTimeout; i++ {
		r.tickHeartbeat()
		r.Advance() // drain any heartbeat messages
	}
}

func TestLeaderStepsDownWhenQuorumSilent_037m(t *testing.T) {
	r := newThreeNodeLeader(t)
	origTerm := r.term

	// No MsgHeartbeatResp or MsgAppResp from any peer.
	tickElectionWindow(r)

	require.Equal(t, Follower, r.state, "leader must step down after electionTimeout with no peer responses")
	require.Equal(t, None, r.lead, "lead must be None after CheckQuorum demotion")
	require.Equal(t, origTerm, r.term, "term must not change on CheckQuorum step-down")
}

func TestLeaderStaysLeaderWithQuorumResponses_037m(t *testing.T) {
	r := newThreeNodeLeader(t)

	// Send heartbeat responses from one peer periodically within the window.
	// Self + 1 peer = 2 = quorum of 3.
	for i := 0; i < r.electionTimeout; i++ {
		r.tickHeartbeat()
		r.Advance()
		// Respond from peer 2 every few ticks.
		if i%3 == 0 {
			require.NoError(t, r.Step(&raftpb.Message{
				Type: raftpb.MessageType_MsgHeartbeatResp,
				From: 2,
				Term: r.term,
			}))
		}
	}

	require.Equal(t, Leader, r.state, "leader with quorum responses must stay leader")
}

func TestSubQuorumResponsesNotEnough_037m(t *testing.T) {
	r := newTestRaft(1)
	r.peers = []uint64{2, 3, 4, 5} // 5-node cluster, quorum = 3
	r.term = 1
	r.becomeLeader()
	r.storage = &mockStorage{
		entries: []*raftpb.Entry{{Index: 1, Term: 1}},
	}
	r.commitIndex = 1
	r.Advance()

	// Only one peer responds — self + 1 = 2 < quorum(3).
	for i := 0; i < r.electionTimeout; i++ {
		r.tickHeartbeat()
		r.Advance()
		if i%3 == 0 {
			require.NoError(t, r.Step(&raftpb.Message{
				Type: raftpb.MessageType_MsgHeartbeatResp,
				From: 2,
				Term: r.term,
			}))
		}
	}

	require.Equal(t, Follower, r.state, "sub-quorum responses must trigger step-down")
}

func TestRecentActiveResetsAcrossWindows_037m(t *testing.T) {
	r := newThreeNodeLeader(t)

	// Window 1: full quorum — both peers respond early in the window.
	// Responses must stop before the last tick (i=electionTimeout-1) because
	// MsgCheckQuorum fires on that tick and resetRecentActive clears the flags
	// *before* any response on that tick would re-stamp them.
	for i := 0; i < r.electionTimeout; i++ {
		r.tickHeartbeat()
		r.Advance()
		if i%3 == 0 && i < r.electionTimeout-1 {
			require.NoError(t, r.Step(&raftpb.Message{
				Type: raftpb.MessageType_MsgHeartbeatResp,
				From: 2,
				Term: r.term,
			}))
			require.NoError(t, r.Step(&raftpb.Message{
				Type: raftpb.MessageType_MsgHeartbeatResp,
				From: 3,
				Term: r.term,
			}))
		}
	}
	require.Equal(t, Leader, r.state, "leader must survive window 1 with full quorum")

	// Window 2: zero responses.
	tickElectionWindow(r)

	require.Equal(t, Follower, r.state, "leader must step down in window 2 with no responses")
}

func TestSingletonClusterNeverStepsDown_037m(t *testing.T) {
	r := newTestRaft(1)
	// No peers — singleton cluster.
	r.term = 1
	r.becomeLeader()
	r.Advance()

	// Tick through multiple election windows.
	for window := 0; window < 3; window++ {
		tickElectionWindow(r)
	}

	require.Equal(t, Leader, r.state, "singleton leader must never step down")
}

func TestMsgAppRespCountsAsLiveness_037m(t *testing.T) {
	r := newThreeNodeLeader(t)

	// Peer 2 sends only MsgAppResp, no heartbeat responses.
	for i := 0; i < r.electionTimeout; i++ {
		r.tickHeartbeat()
		r.Advance()
		if i%3 == 0 {
			require.NoError(t, r.Step(&raftpb.Message{
				Type:  raftpb.MessageType_MsgAppResp,
				From:  2,
				Term:  r.term,
				Index: 1,
			}))
		}
	}

	require.Equal(t, Leader, r.state, "MsgAppResp must count as liveness for CheckQuorum")
}

func TestStaleTermResponsesDoNotCountForCheckQuorum_037m(t *testing.T) {
	r := newThreeNodeLeader(t)

	// Send heartbeat responses at a lower term.
	for i := 0; i < r.electionTimeout; i++ {
		r.tickHeartbeat()
		r.Advance()
		if i%3 == 0 {
			require.NoError(t, r.Step(&raftpb.Message{
				Type: raftpb.MessageType_MsgHeartbeatResp,
				From: 2,
				Term: r.term - 1, // stale term
			}))
		}
	}

	require.Equal(t, Follower, r.state, "stale-term responses must not count for CheckQuorum")
}

func TestSteppedDownNodeAcceptsHigherTermMsgApp_037m(t *testing.T) {
	r := newThreeNodeLeader(t)
	origTerm := r.term

	// Force CheckQuorum step-down.
	tickElectionWindow(r)
	require.Equal(t, Follower, r.state)

	// Deliver MsgApp at term T+1 from a new leader (node 2).
	require.NoError(t, r.Step(&raftpb.Message{
		Type:    raftpb.MessageType_MsgApp,
		From:    2,
		To:      1,
		Term:    origTerm + 1,
		Index:   1,
		LogTerm: 1,
		Commit:  1,
	}))

	require.Equal(t, origTerm+1, r.term, "term must advance to the new leader's term")
	require.Equal(t, uint64(2), r.lead, "lead must become the new leader")
	require.Equal(t, Follower, r.state, "must remain follower under new leader")
}

func TestStepDownClearsPendingReadIndexQueues_037m(t *testing.T) {
	r := newThreeNodeLeader(t)

	// Seed readOnly with a pending request.
	rctx := []byte("rctx-pending")
	r.readOnly.addRequest(r.commitIndex, &raftpb.Message{
		Type:    raftpb.MessageType_MsgReadIndex,
		From:    1,
		Context: rctx,
	})
	require.NotEmpty(t, r.readOnly.pendingReadIndex, "precondition: readOnly must have a pending request")

	// Seed pendingReadIndexRequests.
	r.pendingReadIndexRequests = append(r.pendingReadIndexRequests, &raftpb.Message{
		Type:    raftpb.MessageType_MsgReadIndex,
		From:    1,
		Context: []byte("rctx-deferred"),
	})
	require.NotEmpty(t, r.pendingReadIndexRequests, "precondition: pending queue must be non-empty")

	// Trigger CheckQuorum step-down.
	tickElectionWindow(r)
	require.Equal(t, Follower, r.state)

	require.Empty(t, r.readOnly.pendingReadIndex, "readOnly must be cleared on step-down")
	require.Empty(t, r.pendingReadIndexRequests, "pendingReadIndexRequests must be cleared on step-down")
}
