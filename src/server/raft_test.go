package server

import (
	"context"
	"errors"
	"kvgo/raft"
	"kvgo/raftpb"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func waitForEvent_036m(t *testing.T, events <-chan string) string {
	t.Helper()

	select {
	case event := <-events:
		return event
	case <-time.After(100 * time.Millisecond):
		t.Fatal("timeout waiting for raft host event")
		return ""
	}
}

func newRaftHostConfig() RaftHostConfig {
	return RaftHostConfig{
		ID:        1,
		Peers:     []Peer{{ID: 2}},
		Storage:   &mockStorage{},
		Transport: &mockRaftTransport{},
	}
}

func newInternalRaftHostConfig_036m() raftHostConfig {
	cfg := newRaftHostConfig()
	return raftHostConfig{
		RaftHostConfig: cfg,
		n:              &fakeNode{c: make(chan raft.Ready, 1)},
	}
}

func TestInternalRaftHostRequiresDependencies_036m(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*raftHostConfig)
		wantErr string
	}{
		{
			name: "missing storage",
			mutate: func(cfg *raftHostConfig) {
				cfg.Storage = nil
			},
			wantErr: "storage is not presented",
		},
		{
			name: "missing transport",
			mutate: func(cfg *raftHostConfig) {
				cfg.Transport = nil
			},
			wantErr: "transport is not presented",
		},
		{
			name: "missing node",
			mutate: func(cfg *raftHostConfig) {
				cfg.n = nil
			},
			wantErr: "node is not presented",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			cfg := newInternalRaftHostConfig_036m()
			tt.mutate(&cfg)

			_, err := newRaftHost(ctx, cfg)
			require.EqualError(t, err, tt.wantErr)
		})
	}
}

func TestNewRaftHostRequiresDependencies_036m(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*RaftHostConfig)
		wantErr string
	}{
		{
			name: "missing storage",
			mutate: func(cfg *RaftHostConfig) {
				cfg.Storage = nil
			},
			wantErr: "storage is not presented",
		},
		{
			name: "missing transport",
			mutate: func(cfg *RaftHostConfig) {
				cfg.Transport = nil
			},
			wantErr: "transport is not presented",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := newRaftHostConfig()
			tt.mutate(&cfg)

			_, err := NewRaftHost(context.Background(), cfg)
			require.EqualError(t, err, tt.wantErr)
		})
	}
}

func TestRaftHostConfigOwnsPeerList_036m(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg := newInternalRaftHostConfig_036m()
	cfg.Peers = []Peer{{ID: 2}, {ID: 3}}

	host, err := newRaftHost(ctx, cfg)
	require.NoError(t, err)
	require.Equal(t, cfg.Peers, host.peers)

	cfg.Peers[0].ID = 9
	require.Equal(t, []Peer{{ID: 2}, {ID: 3}}, host.peers)
}

func TestNewRaftHostStopCancelsConstructedNodeContext_036m(t *testing.T) {
	prevFactory := newRaftNode
	defer func() { newRaftNode = prevFactory }()

	ctxObserved := make(chan context.Context, 1)
	newRaftNode = func(ctx context.Context, cfg raft.Config) raft.Node {
		ctxObserved <- ctx
		return &fakeNode{c: make(chan raft.Ready)}
	}

	host, err := NewRaftHost(context.Background(), newRaftHostConfig())
	require.NoError(t, err)

	var nodeCtx context.Context
	select {
	case nodeCtx = <-ctxObserved:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("timeout waiting for node construction context")
	}

	select {
	case <-nodeCtx.Done():
		t.Fatal("node context canceled before host stop")
	default:
	}

	host.Stop()

	select {
	case <-nodeCtx.Done():
	case <-time.After(100 * time.Millisecond):
		t.Fatal("timeout waiting for host stop to cancel node context")
	}
}

func TestStoppedRaftHostDoesNotRestart_036m(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fakeNode := &fakeNode{c: make(chan raft.Ready, 1)}
	host, err := newRaftHost(ctx, raftHostConfig{
		RaftHostConfig: newRaftHostConfig(),
		n:              fakeNode,
	})
	require.NoError(t, err)

	host.Start()
	host.Stop()
	host.Start()

	require.False(t, host.started.Load())
	require.True(t, host.stopped.Load())

	select {
	case <-host.done:
	default:
		t.Fatal("expected stopped host to remain done")
	}
}

func TestHostProposeFeedsProposalIntoNode_036m(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg := newRaftHostConfig()
	fakeNode := &fakeNode{}
	rc := raftHostConfig{
		RaftHostConfig: cfg,
		n:              fakeNode,
	}

	host, err := newRaftHost(ctx, rc)
	require.NoError(t, err)

	data := []byte("set x 1")
	require.NoError(t, host.Propose(ctx, data))
	require.Equal(t, data, fakeNode.proposed)
}

func TestHostSendsReadyMessagesByRaftID_036m(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	transport := &mockRaftTransport{sent: make(chan *raftpb.Message, 1)}
	cfg := newRaftHostConfig()
	cfg.Transport = transport

	fakeNode := &fakeNode{c: make(chan raft.Ready, 1)}
	rc := raftHostConfig{
		RaftHostConfig: cfg,
		n:              fakeNode,
	}

	host, err := newRaftHost(ctx, rc)
	require.NoError(t, err)
	host.Start()
	defer host.Stop()

	msg := &raftpb.Message{To: 2}
	fakeNode.c <- raft.Ready{
		Messages: []*raftpb.Message{msg},
	}

	select {
	case got := <-transport.sent:
		require.Equal(t, msg, got)
	case <-time.After(100 * time.Millisecond):
		t.Fatal("timeout waiting for raft transport send")
	}
}

func TestHostStepFeedsInboundRaftMessageIntoNode_036m(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg := newRaftHostConfig()
	fakeNode := &fakeNode{}
	rc := raftHostConfig{
		RaftHostConfig: cfg,
		n:              fakeNode,
	}

	host, err := newRaftHost(ctx, rc)
	require.NoError(t, err)

	msg := &raftpb.Message{To: 2}
	require.NoError(t, host.Step(ctx, msg))
	require.NotNil(t, fakeNode.steppedMsg)
	require.Equal(t, msg, fakeNode.steppedMsg)
}

func TestHostCampaignFeedsNodeCampaign_036m(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg := newRaftHostConfig()
	fakeNode := &fakeNode{}
	rc := raftHostConfig{
		RaftHostConfig: cfg,
		n:              fakeNode,
	}

	host, err := newRaftHost(ctx, rc)
	require.NoError(t, err)
	require.NoError(t, host.Campaign(ctx))
	require.True(t, fakeNode.campaigned)
}

func TestHostDrainsReadyThenAdvance_036m(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	events := make(chan string, 3)
	transport := &mockRaftTransport{sent: make(chan *raftpb.Message, 1), events: events}
	cfg := newRaftHostConfig()
	cfg.Transport = transport

	fakeNode := &fakeNode{c: make(chan raft.Ready, 1), advancedCh: make(chan struct{}, 1), events: events}
	fakeStorage := &mockStorage{savedCh: make(chan []*raftpb.Entry, 1), events: events}
	cfg.Storage = fakeStorage

	rc := raftHostConfig{
		RaftHostConfig: cfg,
		n:              fakeNode,
	}

	host, err := newRaftHost(ctx, rc)
	require.NoError(t, err)
	host.Start()
	defer host.Stop()

	msg := &raftpb.Message{To: 2}
	commitEnt := &raftpb.Entry{Index: 1, Term: 1, Data: []byte("foo")}
	ent := &raftpb.Entry{Index: 2, Term: 1, Data: []byte("bar")}
	fakeNode.c <- raft.Ready{
		Messages:         []*raftpb.Message{msg},
		Entries:          []*raftpb.Entry{ent},
		CommittedEntries: []*raftpb.Entry{commitEnt},
	}

	select {
	case got := <-transport.sent:
		require.Equal(t, msg, got)
	case <-time.After(100 * time.Millisecond):
		t.Fatal("timeout waiting for raft transport send")
	}

	select {
	case saved := <-fakeStorage.savedCh:
		require.Equal(t, []*raftpb.Entry{ent}, saved)
	case <-time.After(100 * time.Millisecond):
		t.Fatal("timeout waiting for raft storage save")
	}

	// handleBatch blocks on unbuffered applyc — read to unblock
	select {
	case applied := <-host.Apply():
		require.Equal(t, toApply{data: [][]byte{[]byte("foo")}}, applied)
	case <-time.After(100 * time.Millisecond):
		t.Fatal("timeout waiting for apply channel")
	}

	select {
	case <-fakeNode.advancedCh:
		require.True(t, fakeNode.advanced)
	case <-time.After(100 * time.Millisecond):
		t.Fatal("timeout waiting for raft advance")
	}

	seen := []string{
		waitForEvent_036m(t, events),
		waitForEvent_036m(t, events),
		waitForEvent_036m(t, events),
	}
	require.Equal(t, "advance", seen[2])
	require.ElementsMatch(t, []string{"send", "save"}, seen[:2])
}

func TestHostPersistsHardStateOnlyBatch_036m(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fakeStorage := &mockStorage{savedCh: make(chan []*raftpb.Entry, 1)}
	fakeNode := &fakeNode{c: make(chan raft.Ready, 1), advancedCh: make(chan struct{}, 1)}
	cfg := newRaftHostConfig()
	cfg.Storage = fakeStorage
	rc := raftHostConfig{
		RaftHostConfig: cfg,
		n:              fakeNode,
	}

	host, err := newRaftHost(ctx, rc)
	require.NoError(t, err)
	host.Start()
	defer host.Stop()

	hard := &raftpb.HardState{Term: 2, VotedFor: 1, CommittedIndex: 3}
	fakeNode.c <- raft.Ready{HardState: hard}

	select {
	case saved := <-fakeStorage.savedCh:
		require.Empty(t, saved)
	case <-time.After(100 * time.Millisecond):
		t.Fatal("timeout waiting for raft storage save")
	}
	require.Equal(t, hard, fakeStorage.savedHard)

	select {
	case <-fakeNode.advancedCh:
		require.True(t, fakeNode.advanced)
	case <-time.After(100 * time.Millisecond):
		t.Fatal("timeout waiting for raft advance")
	}
}

func TestHostReportsBatchFailureAndStops_036m(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg := newRaftHostConfig()
	fakeNode := &fakeNode{c: make(chan raft.Ready, 1)}
	fakeStorage := &mockStorage{saveErr: errors.New("save failed")}
	cfg.Storage = fakeStorage
	rc := raftHostConfig{
		RaftHostConfig: cfg,
		n:              fakeNode,
	}

	host, err := newRaftHost(ctx, rc)
	require.NoError(t, err)
	host.Start()

	fakeNode.c <- raft.Ready{Entries: []*raftpb.Entry{{Index: 1, Term: 1}}, HardState: &raftpb.HardState{Term: 1}}

	select {
	case err := <-host.Errors():
		require.EqualError(t, err, "save failed")
	case <-time.After(100 * time.Millisecond):
		t.Fatal("timeout waiting for raft host error")
	}

	select {
	case <-host.done:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("timeout waiting for raft host stop")
	}

	require.False(t, host.started.Load())
	require.False(t, fakeNode.advanced)
}
