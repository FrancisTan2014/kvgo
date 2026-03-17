package raft

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func newStartedNode(ctx context.Context, id uint64, peers ...uint64) Node {
	return NewNode(ctx, Config{ID: id, Peers: peers, Storage: &mockStorage{}})
}

func TestConsumeEntryAfterPropose_036b(t *testing.T) {
	ctx := context.Background()
	n := setupNode(Config{ID: 1, Storage: &mockStorage{}})
	n.r.state = Leader
	go n.run(ctx)

	require.NoError(t, n.Propose(ctx, []byte("foo")))
	r := <-n.Ready()
	require.Len(t, r.Entries, 1)
}

func TestAdvanceMovesForward_036b(t *testing.T) {
	ctx := context.Background()
	n := setupNode(Config{ID: 1, Storage: &mockStorage{}})
	n.r.state = Leader
	go n.run(ctx)

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

	leader := setupNode(Config{ID: 1, Peers: []uint64{2}, Storage: &mockStorage{}})
	leader.r.state = Leader
	go leader.run(ctx)
	require.NoError(t, leader.Propose(ctx, []byte("foo")))

	msg := (<-leader.Ready()).Messages[0]
	follower := newStartedNode(ctx, 2)
	require.NoError(t, follower.Step(ctx, msg))

	rd := <-follower.Ready()
	require.Len(t, rd.Entries, 1)
	require.Equal(t, []byte("foo"), rd.Entries[0].Data)
}

func TestNodeCampaignExposesVoteMessages_036l(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	n := newStartedNode(ctx, 1, 2)
	require.NoError(t, n.Campaign(ctx))

	rd := <-n.Ready()
	require.Len(t, rd.Messages, 1)
	require.Equal(t, MsgVote, rd.Messages[0].Type)
	require.Equal(t, uint64(2), rd.Messages[0].To)
}

func TestNodeConfigOwnsInitialMembership_036l(t *testing.T) {
	n := setupNode(Config{ID: 1, Peers: []uint64{2, 3}, Storage: &mockStorage{}})
	require.Equal(t, []uint64{2, 3}, n.r.peers)
}

func TestNodeReadyCarriesHardStateFromRaft_036m(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	n := newStartedNode(ctx, 1, 2)
	require.NoError(t, n.Campaign(ctx))

	rd := <-n.Ready()
	require.Equal(t, HardState{Term: 1, VotedFor: 1, CommittedIndex: 0}, rd.HardState)
}

func TestNodeReadyCarriesHardStateWithoutMessagesOrEntries_036m(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	n := setupNode(Config{ID: 1, Peers: []uint64{2}, Storage: &mockStorage{}})
	n.r.state = Leader
	n.r.term = 1
	go n.run(ctx)

	require.NoError(t, n.Step(ctx, Message{Type: MsgApp, From: 2, To: 1, Term: 2}))

	select {
	case rd := <-n.Ready():
		require.Equal(t, HardState{Term: 2, VotedFor: 0, CommittedIndex: 0}, rd.HardState)
		require.Len(t, rd.Messages, 0)
		require.Len(t, rd.Entries, 0)
		require.Len(t, rd.CommittedEntries, 0)
	case <-time.After(100 * time.Millisecond):
		t.Fatal("timeout waiting for node ready carrying hard state")
	}
}
