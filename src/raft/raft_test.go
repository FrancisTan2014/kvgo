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

func TestProposeNotChangeCommitIndex(t *testing.T) {
	r := newLeaderRaftWithOnePeer()
	require.NoError(t, r.Propose([]byte("")))
	require.Zero(t, r.commitIndex)
}

func TestReadyReturnsProposedEntry(t *testing.T) {
	r := newLeaderRaftWithOnePeer()
	require.NoError(t, r.Propose([]byte("")))
	ready := r.Ready()
	require.Len(t, ready.Entries, 1)
	require.Len(t, ready.CommittedEntries, 0)
}

func TestAdvanceClearReadyEntries(t *testing.T) {
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

func TestCommitToIncreasesCommitIndex(t *testing.T) {
	r := newLeaderRaftWithOnePeer()
	require.NoError(t, r.Propose([]byte("foo")))

	r.CommitTo(1)
	require.Equal(t, uint64(1), r.commitIndex)
}

func TestCommitToNeverDecreasesCommitIndex(t *testing.T) {
	r := newLeaderRaftWithOnePeer()
	require.NoError(t, r.Propose([]byte("foo")))

	r.CommitTo(1)
	require.Equal(t, uint64(1), r.commitIndex)

	r.CommitTo(0)
	require.Equal(t, uint64(1), r.commitIndex)
}

func TestReadyExposesCommittedEntsOnlyAfterCommitTo(t *testing.T) {
	r := newLeaderRaftWithOnePeer()
	require.NoError(t, r.Propose([]byte("foo")))

	ready := r.Ready()
	require.Len(t, ready.CommittedEntries, 0)

	r.CommitTo(1)
	ready = r.Ready()
	require.Len(t, ready.CommittedEntries, 1)
}

func TestFullLifecycle(t *testing.T) {
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

func TestAppendMessageIsReadyAfterLeaderPropose(t *testing.T) {
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

func TestNewEntryIsReadyAfterFollowerStepMsgApp(t *testing.T) {
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

func TestTrackerCreatedOnPropose(t *testing.T) {
	leader := newLeaderRaftWithOnePeer()
	require.NoError(t, leader.Propose([]byte("foo")))

	id := entryID{index: 1, term: 1}
	require.Contains(t, leader.acks, id)
	require.True(t, leader.acks[id][leader.id])
}

func TestTrackerUpdatedOnStepMsgAppResp(t *testing.T) {
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

func TestCommittedEntriesReadyAfterQuorumReached(t *testing.T) {
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
