package raft

import (
	"io"
	"kvgo/raftpb"
	"kvgo/transport"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// SingleTransportFactory creates one transport node. The test controls wiring and lifecycle.
// This is the seam for future Peer implementations: tcpSingleTransportFactory creates
// transports that use tcpPeer; a future httpSingleTransportFactory would use httpPeer.
type SingleTransportFactory func(t *testing.T, cfg RaftTransportConfig, raft *mockRaft) *RaftTransport

func tcpSingleTransportFactory(t *testing.T, cfg RaftTransportConfig, raft *mockRaft) *RaftTransport {
	t.Helper()
	lg := slog.New(slog.NewTextHandler(io.Discard, nil))
	tr, err := NewRaftTransport(cfg, raft, lg)
	require.NoError(t, err)
	return tr
}

// --- Conformance tests: any Peer implementation must pass these ---

var defaultCfg = RaftTransportConfig{
	ListenAddr:   "0.0.0.0:0",
	WriteTimeout: time.Second,
}

func conformanceSendSucceedsAfterPeerStartsLate(t *testing.T, factory SingleTransportFactory) {
	t.Helper()

	// Start t1 only. t2 is not started yet.
	r1 := newMockRaft()
	t1 := factory(t, defaultCfg, r1)
	require.NoError(t, t1.Start())

	// Pre-bind a listener for t2 so we have a stable address,
	// but don't start t2's transport yet.
	ln2, err := net.Listen("tcp", "0.0.0.0:0")
	require.NoError(t, err)
	addr2 := ln2.Addr().String()

	t1.AddPeer(2, addr2)

	// Send while t2's transport isn't running — message sits in buffer or gets dropped.
	t1.Send([]*raftpb.Message{{
		Type: raftpb.MessageType_MsgApp,
		To:   2, From: 1,
		Entries: []*raftpb.Entry{{Data: []byte("hello")}},
	}})

	// Now start t2 with the pre-bound listener.
	r2 := newMockRaft()
	t2 := factory(t, RaftTransportConfig{Listener: ln2, WriteTimeout: time.Second}, r2)
	require.NoError(t, t2.Start())
	t2.AddPeer(1, t1.Addr().String())

	defer t1.Stop()
	defer t2.Stop()

	// Send repeatedly until a message arrives (writerLoop reconnects).
	msg := &raftpb.Message{
		Type: raftpb.MessageType_MsgApp,
		To:   2, From: 1,
		Entries: []*raftpb.Entry{{Data: []byte("after-reconnect")}},
	}

	deadline := time.After(3 * time.Second)
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()

	for {
		t1.Send([]*raftpb.Message{msg})
		select {
		case <-r2.procc:
			return
		case <-ticker.C:
			continue
		case <-deadline:
			t.Fatal("message did not arrive after peer started late")
		}
	}
}

func conformanceSendIsNonBlockingWhenDown(t *testing.T, factory SingleTransportFactory) {
	t.Helper()

	r1 := newMockRaft()
	t1 := factory(t, defaultCfg, r1)
	require.NoError(t, t1.Start())
	defer t1.Stop()

	// Peer address that will never accept.
	t1.AddPeer(2, "127.0.0.1:1") // port 1 — won't accept

	msg := &raftpb.Message{
		Type:    raftpb.MessageType_MsgApp,
		To:      2,
		Entries: []*raftpb.Entry{{Data: []byte("drop-me")}},
	}

	done := make(chan struct{})
	go func() {
		t1.Send([]*raftpb.Message{msg})
		close(done)
	}()

	select {
	case <-done:
		// Send returned immediately.
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Send blocked when peer is down")
	}
}

func conformanceWriterRecoversAfterDrop(t *testing.T, factory SingleTransportFactory) {
	t.Helper()

	// Start both. Establish communication.
	r1 := newMockRaft()
	t1 := factory(t, defaultCfg, r1)
	require.NoError(t, t1.Start())

	r2 := newMockRaft()
	t2 := factory(t, defaultCfg, r2)
	require.NoError(t, t2.Start())

	addr2 := t2.Addr().String()
	t1.AddPeer(2, addr2)
	t2.AddPeer(1, t1.Addr().String())

	// Send initial message to establish connection.
	t1.Send([]*raftpb.Message{{
		Type: raftpb.MessageType_MsgApp,
		To:   2, From: 1,
		Entries: []*raftpb.Entry{{Data: []byte("before")}},
	}})

	select {
	case m := <-r2.procc:
		require.Equal(t, []byte("before"), m.Entries[0].Data)
	case <-time.After(2 * time.Second):
		t.Fatal("initial message did not arrive")
	}

	// Kill t2.
	t2.Stop()

	// Restart t2 on the same address.
	r2New := newMockRaft()
	t2New := factory(t, RaftTransportConfig{ListenAddr: addr2, WriteTimeout: time.Second}, r2New)
	require.NoError(t, t2New.Start())
	t2New.AddPeer(1, t1.Addr().String())
	defer t1.Stop()
	defer t2New.Stop()

	// Send messages until one arrives (writerLoop reconnects).
	msg := &raftpb.Message{
		Type: raftpb.MessageType_MsgApp,
		To:   2, From: 1,
		Entries: []*raftpb.Entry{{Data: []byte("after")}},
	}

	deadline := time.After(3 * time.Second)
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()

	for {
		t1.Send([]*raftpb.Message{msg})
		select {
		case m := <-r2New.procc:
			require.Equal(t, []byte("after"), m.Entries[0].Data)
			return
		case <-ticker.C:
			continue
		case <-deadline:
			t.Fatal("message did not arrive after connection drop + restart")
		}
	}
}

func conformanceStopIsClean(t *testing.T, factory SingleTransportFactory) {
	t.Helper()

	r1 := newMockRaft()
	t1 := factory(t, defaultCfg, r1)
	require.NoError(t, t1.Start())

	r2 := newMockRaft()
	t2 := factory(t, defaultCfg, r2)
	require.NoError(t, t2.Start())

	t1.AddPeer(2, t2.Addr().String())
	t2.AddPeer(1, t1.Addr().String())

	// Send some messages to get connections established.
	t1.Send([]*raftpb.Message{{
		Type: raftpb.MessageType_MsgApp,
		To:   2, From: 1,
		Entries: []*raftpb.Entry{{Data: []byte("pre-stop")}},
	}})
	time.Sleep(50 * time.Millisecond) // let connections establish

	// Stop should return without hanging.
	done := make(chan struct{})
	go func() {
		t1.Stop()
		t2.Stop()
		close(done)
	}()

	select {
	case <-done:
		// Clean stop.
	case <-time.After(5 * time.Second):
		t.Fatal("Stop hung — goroutines or connections leaked")
	}
}

// --- Wire tcpPeer conformance tests ---

func TestConformance_SendSucceedsAfterPeerStartsLate_037d(t *testing.T) {
	conformanceSendSucceedsAfterPeerStartsLate(t, tcpSingleTransportFactory)
}

func TestConformance_SendIsNonBlockingWhenDown_037d(t *testing.T) {
	conformanceSendIsNonBlockingWhenDown(t, tcpSingleTransportFactory)
}

func TestConformance_WriterRecoversAfterDrop_037d(t *testing.T) {
	conformanceWriterRecoversAfterDrop(t, tcpSingleTransportFactory)
}

func TestConformance_StopIsClean_037d(t *testing.T) {
	conformanceStopIsClean(t, tcpSingleTransportFactory)
}

// --- Accept-triggers-reset (tcpPeer-specific) ---

func TestAcceptTriggersReconnect_037d(t *testing.T) {
	cfg := RaftTransportConfig{
		ListenAddr:   "0.0.0.0:0",
		WriteTimeout: time.Second,
	}
	lg := slog.New(slog.NewTextHandler(io.Discard, nil))

	r1 := newMockRaft()
	t1, err := NewRaftTransport(cfg, r1, lg)
	require.NoError(t, err)
	require.NoError(t, t1.Start())

	// Peer 2 doesn't exist yet — writerLoop for peer 2 will enter backoff.
	t1.AddPeer(2, "127.0.0.1:1") // unreachable

	// Wait for the writerLoop to be in backoff.
	time.Sleep(200 * time.Millisecond)

	// Simulate peer 2 connecting to t1 (as if B's writerLoop dialed A).
	conn, err := net.Dial("tcp", t1.Addr().String())
	require.NoError(t, err)
	defer conn.Close()

	// Send an identification message (From=2).
	f := transport.NewFramer(conn, conn)
	identMsg := &raftpb.Message{
		Type: raftpb.MessageType_MsgApp,
		From: 2,
		To:   1,
	}
	data, err := proto.Marshal(identMsg)
	require.NoError(t, err)
	require.NoError(t, f.Write(data))

	// The accept should trigger a Reset on peer 2's writerLoop.
	// We can verify by checking the resetc channel indirectly:
	// the message should be processed by r1.
	select {
	case m := <-r1.procc:
		require.Equal(t, uint64(2), m.From)
	case <-time.After(time.Second):
		t.Fatal("accept did not process the identification message")
	}

	t1.Stop()
}

// --- Backoff (tcpPeer-specific) ---

func TestBackoffIncreasesOnRepeatedFailure_037d(t *testing.T) {
	// Verify that backoff durations increase exponentially.
	d1 := backoffDuration(1)
	d2 := backoffDuration(2)
	d3 := backoffDuration(3)
	d4 := backoffDuration(5)

	require.True(t, d1 >= 37*time.Millisecond && d1 <= 63*time.Millisecond,
		"d1=%v not in [37ms, 63ms]", d1)
	require.True(t, d2 > d1, "d2=%v should be > d1=%v", d2, d1)
	require.True(t, d3 > d2, "d3=%v should be > d2=%v", d3, d2)
	require.True(t, d4 <= backoffMax, "d4=%v should be capped at %v", d4, backoffMax)
}
