package raft

import (
	"context"
	"io"
	"kvgo/raftpb"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

type mockRaft struct {
	procc chan *raftpb.Message
}

func newMockRaft() *mockRaft {
	return &mockRaft{
		procc: make(chan *raftpb.Message, 1),
	}
}

func (r *mockRaft) Process(ctx context.Context, m *raftpb.Message) error {
	r.procc <- m
	return nil
}

type testSuite struct {
	t1 *RaftTransport
	t2 *RaftTransport
	r1 *mockRaft
	r2 *mockRaft
}

func newTestSuite(t *testing.T) testSuite {
	t.Helper()

	cfg := RaftTransportConfig{
		ListenAddr:   "0.0.0.0:0",
		WriteTimeout: time.Second,
	}

	noopLogger := slog.New(slog.NewTextHandler(io.Discard, nil))

	raft1 := newMockRaft()
	transport1, err := NewRaftTransport(cfg, raft1, noopLogger)
	require.NoError(t, err)
	require.NoError(t, transport1.Start())

	raft2 := newMockRaft()
	transport2, err := NewRaftTransport(cfg, raft2, noopLogger)
	require.NoError(t, err)
	require.NoError(t, transport2.Start())

	transport1.AddPeer(2, transport2.Addr().String())
	transport2.AddPeer(1, transport1.Addr().String())

	return testSuite{
		t1: transport1,
		t2: transport2,
		r1: raft1,
		r2: raft2,
	}
}

func TestMessageSentAndArrivedIntact_038(t *testing.T) {
	suite := newTestSuite(t)
	defer suite.t1.Stop()
	defer suite.t2.Stop()

	msg := &raftpb.Message{
		Type: raftpb.MessageType_MsgApp,
		To:   2,
		Entries: []*raftpb.Entry{{
			Data: []byte("foo"),
		}},
	}
	suite.t1.Send([]*raftpb.Message{msg})

	select {
	case m := <-suite.r2.procc:
		require.True(t, proto.Equal(msg, m), "message not equal")
	case <-time.After(100 * time.Millisecond):
		t.Fatal("message did not arrive within timeout")
	}
}

func TestTransportIsBidirectional_038(t *testing.T) {
	suite := newTestSuite(t)
	defer suite.t1.Stop()
	defer suite.t2.Stop()

	msg1 := &raftpb.Message{
		Type: raftpb.MessageType_MsgApp,
		To:   2,
		Entries: []*raftpb.Entry{{
			Data: []byte("foo"),
		}},
	}
	suite.t1.Send([]*raftpb.Message{msg1})

	msg2 := &raftpb.Message{
		Type: raftpb.MessageType_MsgApp,
		To:   1,
		Entries: []*raftpb.Entry{{
			Data: []byte("bar"),
		}},
	}
	suite.t2.Send([]*raftpb.Message{msg2})

	select {
	case m := <-suite.r2.procc:
		require.True(t, proto.Equal(msg1, m), "msg1 not equal")
	case <-time.After(100 * time.Millisecond):
		t.Fatal("message did not arrive within timeout")
	}

	select {
	case m := <-suite.r1.procc:
		require.True(t, proto.Equal(msg2, m), "msg2 not equal")
	case <-time.After(100 * time.Millisecond):
		t.Fatal("message did not arrive within timeout")
	}
}

func TestUnknownPeerDropped_038(t *testing.T) {
	suite := newTestSuite(t)
	defer suite.t1.Stop()
	defer suite.t2.Stop()

	msg := &raftpb.Message{
		Type: raftpb.MessageType_MsgApp,
		To:   3, // unknow peer
		Entries: []*raftpb.Entry{{
			Data: []byte("foo"),
		}},
	}

	done := make(chan struct{})
	go func() {
		suite.t1.Send([]*raftpb.Message{msg})
		close(done)
	}()

	select {
	case <-done:
		// no panic, no hang
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Send hung on unknown peer")
	}
}

func TestStopDisposesResources_038(t *testing.T) {
	suite := newTestSuite(t)
	defer suite.t2.Stop()

	suite.t1.Send([]*raftpb.Message{{To: 2}})

	addr := suite.t1.Addr().String()
	suite.t1.Stop()

	// If Stop closed the listener, we can rebind the same port
	ln, err := net.Listen("tcp", addr)
	require.NoError(t, err)
	ln.Close()
}
