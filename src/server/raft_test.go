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
		Peers:     []uint64{2},
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
			cfg := newInternalRaftHostConfig_036m()
			tt.mutate(&cfg)

			_, err := newRaftHost(cfg)
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

			_, err := NewRaftHost(cfg)
			require.EqualError(t, err, tt.wantErr)
		})
	}
}

func TestRaftHostConfigOwnsPeerList_036m(t *testing.T) {
	cfg := newInternalRaftHostConfig_036m()
	cfg.Peers = []uint64{2, 3}

	host, err := newRaftHost(cfg)
	require.NoError(t, err)
	require.Equal(t, cfg.Peers, host.peers)

	cfg.Peers[0] = 9
	require.Equal(t, []uint64{2, 3}, host.peers)
}

func TestStoppedRaftHostDoesNotRestart_036m(t *testing.T) {
	fakeNode := &fakeNode{c: make(chan raft.Ready, 1)}
	host, err := newRaftHost(raftHostConfig{
		RaftHostConfig: newRaftHostConfig(),
		n:              fakeNode,
	})
	require.NoError(t, err)

	host.Start()
	host.Stop()
	host.Start()

	select {
	case <-host.done:
	default:
		t.Fatal("expected stopped host to remain done")
	}
}

func TestHostProposeFeedsProposalIntoNode_036m(t *testing.T) {
	cfg := newRaftHostConfig()
	fakeNode := &fakeNode{}
	rc := raftHostConfig{
		RaftHostConfig: cfg,
		n:              fakeNode,
	}

	host, err := newRaftHost(rc)
	require.NoError(t, err)

	data := []byte("set x 1")
	require.NoError(t, host.Propose(context.Background(), data))
	require.Equal(t, data, fakeNode.proposed)
}

func TestHostSendsReadyMessagesByRaftID_036m(t *testing.T) {
	transport := &mockRaftTransport{sent: make(chan *raftpb.Message, 1)}
	cfg := newRaftHostConfig()
	cfg.Transport = transport

	fakeNode := &fakeNode{c: make(chan raft.Ready, 1)}
	rc := raftHostConfig{
		RaftHostConfig: cfg,
		n:              fakeNode,
	}

	host, err := newRaftHost(rc)
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
	cfg := newRaftHostConfig()
	fakeNode := &fakeNode{}
	rc := raftHostConfig{
		RaftHostConfig: cfg,
		n:              fakeNode,
	}

	host, err := newRaftHost(rc)
	require.NoError(t, err)

	msg := &raftpb.Message{To: 2}
	require.NoError(t, host.Step(context.Background(), msg))
	require.NotNil(t, fakeNode.steppedMsg)
	require.Equal(t, msg, fakeNode.steppedMsg)
}

func TestHostCampaignFeedsNodeCampaign_036m(t *testing.T) {
	cfg := newRaftHostConfig()
	fakeNode := &fakeNode{}
	rc := raftHostConfig{
		RaftHostConfig: cfg,
		n:              fakeNode,
	}

	host, err := newRaftHost(rc)
	require.NoError(t, err)
	require.NoError(t, host.Campaign(context.Background()))
	require.True(t, fakeNode.campaigned)
}

func TestHostDrainsReadyThenAdvance_036m(t *testing.T) {
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

	host, err := newRaftHost(rc)
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
	fakeStorage := &mockStorage{savedCh: make(chan []*raftpb.Entry, 1)}
	fakeNode := &fakeNode{c: make(chan raft.Ready, 1), advancedCh: make(chan struct{}, 1)}
	cfg := newRaftHostConfig()
	cfg.Storage = fakeStorage
	rc := raftHostConfig{
		RaftHostConfig: cfg,
		n:              fakeNode,
	}

	host, err := newRaftHost(rc)
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
	cfg := newRaftHostConfig()
	fakeNode := &fakeNode{c: make(chan raft.Ready, 1)}
	fakeStorage := &mockStorage{saveErr: errors.New("save failed")}
	cfg.Storage = fakeStorage
	rc := raftHostConfig{
		RaftHostConfig: cfg,
		n:              fakeNode,
	}

	host, err := newRaftHost(rc)
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

	require.False(t, fakeNode.advanced)
}

// ---------------------------------------------------------------------------
// 036u — The Wiring (raftHost-level)
// ---------------------------------------------------------------------------

func TestHandleBatchUnblocksOnStopcWhenApplycHasNoReader_036u(t *testing.T) {
	// handleBatch sends committed entries on the unbuffered applyc.
	// If nothing reads applyc (e.g. apply loop already exited), the send
	// must unblock when stopc is closed instead of hanging forever.
	fn := &fakeNode{c: make(chan raft.Ready, 1)}
	host, err := newRaftHost(raftHostConfig{
		RaftHostConfig: newRaftHostConfig(),
		n:              fn,
	})
	require.NoError(t, err)

	host.Start()

	// Send a Ready with committed entries — nobody reads applyc.
	fn.c <- raft.Ready{
		CommittedEntries: []*raftpb.Entry{{Index: 1, Term: 1, Data: []byte("x")}},
	}

	// Give the goroutine time to reach the applyc send.
	time.Sleep(20 * time.Millisecond)

	// Stop must return (not deadlock) because handleBatch selects on stopc.
	done := make(chan struct{})
	go func() {
		host.Stop()
		close(done)
	}()

	select {
	case <-done:
		// success — Stop returned
	case <-time.After(2 * time.Second):
		t.Fatal("Stop deadlocked — handleBatch did not unblock on stopc")
	}
}

func TestStopCallsInnerNodeStop_036u(t *testing.T) {
	fn := &fakeNode{c: make(chan raft.Ready, 1)}
	host, err := newRaftHost(raftHostConfig{
		RaftHostConfig: newRaftHostConfig(),
		n:              fn,
	})
	require.NoError(t, err)

	host.Start()
	host.Stop()

	require.True(t, fn.stopped, "raftHost.Stop must call node.Stop")
}

func TestDoubleStartIsNoop_036u(t *testing.T) {
	fn := &fakeNode{c: make(chan raft.Ready, 1)}
	host, err := newRaftHost(raftHostConfig{
		RaftHostConfig: newRaftHostConfig(),
		n:              fn,
	})
	require.NoError(t, err)

	host.Start()
	host.Start() // must not launch a second goroutine
	host.Stop()

	// If two goroutines were running, the second would panic on
	// close(done) — or done would never close. A clean exit proves
	// idempotency.
	select {
	case <-host.done:
	default:
		t.Fatal("expected host.done to be closed after Stop")
	}
}
