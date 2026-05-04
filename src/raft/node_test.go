package raft

import (
	"context"
	"testing"
	"time"

	"kvgo/raftpb"

	"github.com/stretchr/testify/require"
)

func newStartedNode(id uint64, peers ...uint64) Node {
	return NewNode(Config{ID: id, Peers: peers, Storage: &mockStorage{}, ElectionTick: 10, HeartbeatTick: 1})
}

func TestConsumeEntryAfterPropose_036b(t *testing.T) {
	ctx := context.Background()
	n := setupNode(Config{ID: 1, Storage: &mockStorage{}, ElectionTick: 10, HeartbeatTick: 1})
	defer n.Stop()

	n.r.becomeLeader()
	// 037m: commit and drain the no-op from becomeLeader before run loop.
	n.r.CommitTo(n.r.raftLog.lastIndex())
	rd0 := n.r.Ready()
	if len(rd0.Entries) > 0 {
		n.r.raftLog.storage.(*mockStorage).entries = append(n.r.raftLog.storage.(*mockStorage).entries, rd0.Entries...)
	}
	n.r.Advance(rd0)
	go n.run()

	require.NoError(t, n.Propose(ctx, []byte("foo")))
	r := <-n.Ready()
	require.Len(t, r.Entries, 1)
}

func TestAdvanceMovesForward_036b(t *testing.T) {
	ctx := context.Background()
	n := setupNode(Config{ID: 1, Storage: &mockStorage{}, ElectionTick: 10, HeartbeatTick: 1})
	defer n.Stop()

	n.r.becomeLeader()
	// 037m: commit and drain the no-op from becomeLeader before run loop.
	n.r.CommitTo(n.r.raftLog.lastIndex())
	rd0 := n.r.Ready()
	if len(rd0.Entries) > 0 {
		n.r.raftLog.storage.(*mockStorage).entries = append(n.r.raftLog.storage.(*mockStorage).entries, rd0.Entries...)
	}
	n.r.Advance(rd0)
	go n.run()

	require.NoError(t, n.Propose(ctx, []byte("foo")))
	r1 := <-n.Ready()
	require.Len(t, r1.Entries, 1)

	n.Advance()

	require.NoError(t, n.Propose(ctx, []byte("bar")))
	r2 := <-n.Ready()
	require.Len(t, r2.Entries, 1) // only "bar", not "foo"+"bar"
	require.Equal(t, []byte("bar"), r2.Entries[0].Data)
}

func TestNodeStepFeedsInboundMessageIntoRaft_036l(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	leader := setupNode(Config{ID: 1, Peers: []uint64{2}, Storage: &mockStorage{}, ElectionTick: 10, HeartbeatTick: 1})
	defer leader.Stop()

	leader.r.becomeLeader()
	// 037m: commit and drain the no-op. Keep peer progress at NextIndex=1
	// so the MsgApp starts from the beginning (fresh follower has empty log).
	leader.r.CommitTo(leader.r.raftLog.lastIndex())
	rd0 := leader.r.Ready()
	if len(rd0.Entries) > 0 {
		leader.r.raftLog.storage.(*mockStorage).entries = append(leader.r.raftLog.storage.(*mockStorage).entries, rd0.Entries...)
	}
	leader.r.Advance(rd0)
	go leader.run()
	require.NoError(t, leader.Propose(ctx, []byte("foo")))

	rd := <-leader.Ready()
	require.NotEmpty(t, rd.Messages)
	msg := rd.Messages[0]
	// MsgApp carries entries from NextIndex=1: [no-op, "foo"].
	require.Equal(t, raftpb.MessageType_MsgApp, msg.Type)

	follower := newStartedNode(2)
	defer follower.Stop()

	require.NoError(t, follower.Step(ctx, msg))

	frd := <-follower.Ready()
	// 037m: follower receives all entries from the MsgApp.
	require.GreaterOrEqual(t, len(frd.Entries), 1)
	fLast := frd.Entries[len(frd.Entries)-1]
	require.Equal(t, []byte("foo"), fLast.Data)
}

func TestNodeCampaignExposesVoteMessages_036l(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	n := newStartedNode(1, 2)
	defer n.Stop()

	require.NoError(t, n.Campaign(ctx))

	// 038: Campaign enters PreCandidate first (PreVote). The first Ready
	// carries MsgPreVote, not MsgVote.
	rd := <-n.Ready()
	require.Len(t, rd.Messages, 1)
	require.Equal(t, raftpb.MessageType_MsgPreVote, rd.Messages[0].Type)
	require.Equal(t, uint64(2), rd.Messages[0].To)
}

func TestNodeConfigOwnsInitialMembership_036l(t *testing.T) {
	n := setupNode(Config{ID: 1, Peers: []uint64{2, 3}, Storage: &mockStorage{}, ElectionTick: 10, HeartbeatTick: 1})
	defer n.Stop()
	require.Equal(t, []uint64{2, 3}, n.r.peers)
}

func TestNodeReadyCarriesHardStateFromRaft_036m(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	n := newStartedNode(1, 2)
	defer n.Stop()

	require.NoError(t, n.Campaign(ctx))

	// 038: Campaign enters PreCandidate first. PreCandidate does not advance
	// term or set votedFor, so HardState is unchanged from the initial state.
	rd := <-n.Ready()
	require.Equal(t, &raftpb.HardState{Term: 0, VotedFor: 0, CommittedIndex: 0}, rd.HardState)
}

func TestNodeReadyCarriesHardStateWithoutMessagesOrEntries_036m(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	n := setupNode(Config{ID: 1, Peers: []uint64{2}, Storage: &mockStorage{}, ElectionTick: 10, HeartbeatTick: 1})
	n.r.becomeLeader()
	n.r.term = 1
	// 037m: commit and drain the no-op from becomeLeader before run loop.
	n.r.CommitTo(n.r.raftLog.lastIndex())
	rd0 := n.r.Ready()
	if len(rd0.Entries) > 0 {
		n.r.raftLog.storage.(*mockStorage).entries = append(n.r.raftLog.storage.(*mockStorage).entries, rd0.Entries...)
	}
	n.r.Advance(rd0)
	go n.run()
	defer n.Stop()

	require.NoError(t, n.Step(ctx, &raftpb.Message{Type: raftpb.MessageType_MsgApp, From: 2, To: 1, Term: 2}))

	select {
	case rd := <-n.Ready():
		// 037m: commitIndex is 1 from the no-op committed in setup (CommitTo never decreases).
		require.Equal(t, &raftpb.HardState{Term: 2, VotedFor: 0, CommittedIndex: 1}, rd.HardState)
		// The follower responds to MsgApp with MsgAppResp.
		require.Len(t, rd.Entries, 0)
		require.Len(t, rd.CommittedEntries, 0)
	case <-time.After(100 * time.Millisecond):
		t.Fatal("timeout waiting for node ready carrying hard state")
	}
}
