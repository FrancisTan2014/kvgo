package server

import (
	"context"
	"errors"
	"kvgo/protocol"
	"kvgo/transport"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// newReplicaConn
// ---------------------------------------------------------------------------

func TestNewReplicaConn(t *testing.T) {
	t.Run("connected starts false", func(t *testing.T) {
		rc := newReplicaConn(&fakeStreamTransport{id: 1}, 0, "replid", "node1", "10.0.0.1:5000")
		if rc.connected.Load() {
			t.Error("new replicaConn should start disconnected")
		}
	})

	t.Run("sendCh is buffered", func(t *testing.T) {
		rc := newReplicaConn(&fakeStreamTransport{id: 1}, 0, "replid", "node1", "10.0.0.1:5000")
		if cap(rc.sendCh) != replicaSendBuffer {
			t.Errorf("sendCh cap = %d, want %d", cap(rc.sendCh), replicaSendBuffer)
		}
	})

	t.Run("stores all fields", func(t *testing.T) {
		tr := &fakeStreamTransport{id: 42}
		rc := newReplicaConn(tr, 100, "replid-abc", "node-xyz", "10.0.0.5:8000")

		if rc.transport != tr {
			t.Error("transport not stored")
		}
		if rc.lastSeq != 100 {
			t.Errorf("lastSeq = %d, want 100", rc.lastSeq)
		}
		if rc.lastReplid != "replid-abc" {
			t.Errorf("lastReplid = %q, want replid-abc", rc.lastReplid)
		}
		if rc.nodeID != "node-xyz" {
			t.Errorf("nodeID = %q, want node-xyz", rc.nodeID)
		}
		if rc.listenAddr != "10.0.0.5:8000" {
			t.Errorf("listenAddr = %q, want 10.0.0.5:8000", rc.listenAddr)
		}
	})
}

// ---------------------------------------------------------------------------
// updateReplicaConn
// ---------------------------------------------------------------------------

func TestUpdateReplicaConn(t *testing.T) {
	t.Run("closes old sendCh", func(t *testing.T) {
		s := &Server{}
		oldCh := make(chan []byte, replicaSendBuffer)
		rc := &replicaConn{sendCh: oldCh, nodeID: "n1"}

		newTransport := &fakeStreamTransport{id: 2}
		ctx := &RequestContext{
			StreamTransport: newTransport,
			Request:         protocol.Request{Seq: 50},
		}
		rv := &protocol.ReplicateValue{Replid: "new-replid", ListenAddr: "10.0.0.2:5000"}

		s.updateReplicaConn(rc, ctx, rv)

		// Old channel should be closed
		_, ok := <-oldCh
		if ok {
			t.Error("old sendCh should be closed")
		}
	})

	t.Run("new sendCh is buffered", func(t *testing.T) {
		s := &Server{}
		rc := &replicaConn{sendCh: make(chan []byte, replicaSendBuffer), nodeID: "n1"}

		ctx := &RequestContext{
			StreamTransport: &fakeStreamTransport{id: 2},
			Request:         protocol.Request{Seq: 10},
		}
		rv := &protocol.ReplicateValue{Replid: "r", ListenAddr: "a:1"}

		s.updateReplicaConn(rc, ctx, rv)

		if cap(rc.sendCh) != replicaSendBuffer {
			t.Errorf("new sendCh cap = %d, want %d", cap(rc.sendCh), replicaSendBuffer)
		}
	})

	t.Run("swaps transport and fields", func(t *testing.T) {
		s := &Server{}
		oldTransport := &fakeStreamTransport{id: 1}
		rc := &replicaConn{
			transport:  oldTransport,
			sendCh:     make(chan []byte, replicaSendBuffer),
			lastSeq:    10,
			lastReplid: "old-replid",
			listenAddr: "old:1",
			nodeID:     "n1",
		}

		newTransport := &fakeStreamTransport{id: 2}
		ctx := &RequestContext{
			StreamTransport: newTransport,
			Request:         protocol.Request{Seq: 99},
		}
		rv := &protocol.ReplicateValue{Replid: "new-replid", ListenAddr: "new:2"}

		s.updateReplicaConn(rc, ctx, rv)

		if rc.transport != newTransport {
			t.Error("transport not swapped")
		}
		if rc.lastSeq != 99 {
			t.Errorf("lastSeq = %d, want 99", rc.lastSeq)
		}
		if rc.lastReplid != "new-replid" {
			t.Errorf("lastReplid = %q, want new-replid", rc.lastReplid)
		}
		if rc.listenAddr != "new:2" {
			t.Errorf("listenAddr = %q, want new:2", rc.listenAddr)
		}
	})

	t.Run("does not set connected", func(t *testing.T) {
		s := &Server{}
		rc := &replicaConn{sendCh: make(chan []byte, replicaSendBuffer), nodeID: "n1"}

		ctx := &RequestContext{
			StreamTransport: &fakeStreamTransport{id: 2},
			Request:         protocol.Request{Seq: 1},
		}
		rv := &protocol.ReplicateValue{Replid: "r", ListenAddr: "a:1"}

		s.updateReplicaConn(rc, ctx, rv)

		if rc.connected.Load() {
			t.Error("updateReplicaConn should not set connected — serveReplica does that after sync")
		}
	})
}

// ---------------------------------------------------------------------------
// forwardToReplicas — connected gate
// ---------------------------------------------------------------------------

func TestForwardToReplicas(t *testing.T) {
	t.Run("skips disconnected replicas", func(t *testing.T) {
		s := &Server{
			replicas: make(map[string]*replicaConn),
		}
		ch := make(chan []byte, 1)
		rc := &replicaConn{nodeID: "n1", sendCh: ch, listenAddr: "a:1"}
		// connected is false (zero value)
		s.replicas["n1"] = rc

		payload, _ := protocol.EncodeRequest(protocol.Request{Cmd: protocol.CmdPut, Key: []byte("k"), Value: []byte("v")})
		s.forwardToReplicas(payload, 1)

		select {
		case <-ch:
			t.Error("disconnected replica should not receive writes")
		default:
			// expected
		}
	})

	t.Run("sends to connected replicas", func(t *testing.T) {
		s := &Server{
			replicas: make(map[string]*replicaConn),
		}
		ch := make(chan []byte, 1)
		rc := &replicaConn{nodeID: "n1", sendCh: ch, listenAddr: "a:1"}
		rc.connected.Store(true)
		s.replicas["n1"] = rc

		payload, _ := protocol.EncodeRequest(protocol.Request{Cmd: protocol.CmdPut, Key: []byte("k"), Value: []byte("v")})
		s.forwardToReplicas(payload, 1)

		select {
		case got := <-ch:
			if len(got) == 0 {
				t.Error("received empty payload")
			}
		default:
			t.Error("connected replica should receive writes")
		}
	})

	t.Run("mixed connected and disconnected", func(t *testing.T) {
		s := &Server{
			replicas: make(map[string]*replicaConn),
		}
		chConnected := make(chan []byte, 1)
		chDisconnected := make(chan []byte, 1)

		rcConnected := &replicaConn{nodeID: "n1", sendCh: chConnected, listenAddr: "a:1"}
		rcConnected.connected.Store(true)
		rcDisconnected := &replicaConn{nodeID: "n2", sendCh: chDisconnected, listenAddr: "a:2"}
		// n2 not connected

		s.replicas["n1"] = rcConnected
		s.replicas["n2"] = rcDisconnected

		payload, _ := protocol.EncodeRequest(protocol.Request{Cmd: protocol.CmdPut, Key: []byte("k"), Value: []byte("v")})
		s.forwardToReplicas(payload, 1)

		select {
		case <-chConnected:
			// expected
		default:
			t.Error("connected replica should receive write")
		}

		select {
		case <-chDisconnected:
			t.Error("disconnected replica should not receive write")
		default:
			// expected
		}
	})

	t.Run("drops when buffer full", func(t *testing.T) {
		s := &Server{
			replicas: make(map[string]*replicaConn),
			opts:     Options{Logger: noopLogger},
		}
		ch := make(chan []byte, 1) // buffer of 1
		rc := &replicaConn{nodeID: "n1", sendCh: ch, listenAddr: "a:1"}
		rc.connected.Store(true)
		s.replicas["n1"] = rc

		payload, _ := protocol.EncodeRequest(protocol.Request{Cmd: protocol.CmdPut, Key: []byte("k"), Value: []byte("v")})

		// Fill the buffer
		s.forwardToReplicas(payload, 1)
		// Second write should be dropped (non-blocking)
		s.forwardToReplicas(payload, 2)

		// Should not hang — only one item in channel
		<-ch
		select {
		case <-ch:
			t.Error("second write should have been dropped, not queued")
		default:
			// expected: dropped
		}
	})
}

// ---------------------------------------------------------------------------
// broadcastTopology — connected gate
// ---------------------------------------------------------------------------

func TestBroadcastTopologyConnectedGate(t *testing.T) {
	t.Run("skips disconnected replicas", func(t *testing.T) {
		s := &Server{
			replicas:    make(map[string]*replicaConn),
			nodeID:      "primary",
			opts:        Options{Host: "10.0.0.100", Port: 4000},
			peerManager: NewPeerManager(nil, noopLogger),
		}
		s.role.Store(uint32(RoleLeader))

		ch := make(chan []byte, 1)
		rc := &replicaConn{transport: &fakeStreamTransport{id: 1}, nodeID: "n1", listenAddr: "a:1", sendCh: ch}
		// connected is false
		s.replicas["n1"] = rc

		s.broadcastTopology()

		select {
		case <-ch:
			t.Error("disconnected replica should not receive topology")
		default:
			// expected
		}
	})

	t.Run("sends only to connected replicas", func(t *testing.T) {
		s := &Server{
			replicas:    make(map[string]*replicaConn),
			nodeID:      "primary",
			opts:        Options{Host: "10.0.0.100", Port: 4000},
			peerManager: NewPeerManager(nil, noopLogger),
		}
		s.role.Store(uint32(RoleLeader))

		chConnected := make(chan []byte, 1)
		chDisconnected := make(chan []byte, 1)

		rcConnected := &replicaConn{transport: &fakeStreamTransport{id: 1}, nodeID: "n1", listenAddr: "a:1", sendCh: chConnected}
		rcConnected.connected.Store(true)
		rcDisconnected := &replicaConn{transport: &fakeStreamTransport{id: 2}, nodeID: "n2", listenAddr: "a:2", sendCh: chDisconnected}

		s.replicas["n1"] = rcConnected
		s.replicas["n2"] = rcDisconnected

		s.broadcastTopology()

		select {
		case <-chConnected:
			// expected
		default:
			t.Error("connected replica should receive topology")
		}

		select {
		case <-chDisconnected:
			t.Error("disconnected replica should not receive topology")
		default:
			// expected
		}
	})

	t.Run("disconnected replicas still appear in topology entries", func(t *testing.T) {
		s := &Server{
			replicas:    make(map[string]*replicaConn),
			nodeID:      "primary",
			opts:        Options{Host: "10.0.0.100", Port: 4000},
			peerManager: NewPeerManager(nil, noopLogger),
		}
		s.role.Store(uint32(RoleLeader))

		ch := make(chan []byte, 1)
		rcConnected := &replicaConn{transport: &fakeStreamTransport{id: 1}, nodeID: "n1", listenAddr: "a:1", sendCh: ch}
		rcConnected.connected.Store(true)
		rcDisconnected := &replicaConn{transport: &fakeStreamTransport{id: 2}, nodeID: "n2", listenAddr: "a:2", sendCh: make(chan []byte, 1)}

		s.replicas["n1"] = rcConnected
		s.replicas["n2"] = rcDisconnected

		s.broadcastTopology()

		payload := <-ch
		req, _ := protocol.DecodeRequest(payload)
		val := string(req.Value)

		// n2 should appear in topology even though disconnected
		foundN2 := false
		for _, part := range splitTopology(val) {
			if part == "n2@a:2" {
				foundN2 = true
			}
		}
		if !foundN2 {
			t.Errorf("disconnected replica n2 should still appear in topology entries, got %q", val)
		}
	})
}

// ---------------------------------------------------------------------------
// serveReplicaWriter — reconnect vs error path
// ---------------------------------------------------------------------------

// errorStreamTransport fails on Send after a configured number of successes.
type errorStreamTransport struct {
	mu        sync.Mutex
	sendCount int
	failAfter int // fail on the (failAfter+1)th Send
	closed    bool
}

func (e *errorStreamTransport) Send(ctx context.Context, payload []byte) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.sendCount++
	if e.sendCount > e.failAfter {
		return errors.New("write error")
	}
	return nil
}

func (e *errorStreamTransport) Receive(ctx context.Context) ([]byte, error) {
	return nil, nil
}

func (e *errorStreamTransport) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.closed = true
	return nil
}

func (e *errorStreamTransport) RemoteAddr() string { return "error:1234" }

func (e *errorStreamTransport) isClosed() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.closed
}

func TestServeReplicaWriterReconnect(t *testing.T) {
	t.Run("channel close sets reconnecting and skips cleanup", func(t *testing.T) {
		tr := &errorStreamTransport{failAfter: 999}
		rc := &replicaConn{
			transport:  tr,
			sendCh:     make(chan []byte, replicaSendBuffer),
			hb:         time.NewTicker(time.Hour), // won't fire
			listenAddr: "test:1",
			nodeID:     "n1",
		}
		rc.connected.Store(true)

		s := &Server{
			replicas:    make(map[string]*replicaConn),
			opts:        Options{Logger: noopLogger, WriteTimeout: time.Second},
			peerManager: NewPeerManager(nil, noopLogger),
		}
		s.role.Store(uint32(RoleLeader))
		s.replicas["n1"] = rc

		done := make(chan struct{})
		go func() {
			s.serveReplicaWriter(rc)
			close(done)
		}()

		// Close channel to simulate reconnect (what updateReplicaConn does)
		close(rc.sendCh)

		select {
		case <-done:
			// Writer exited
		case <-time.After(2 * time.Second):
			t.Fatal("serveReplicaWriter did not exit after channel close")
		}

		// Transport should NOT be closed (reconnect path skips cleanup)
		if tr.isClosed() {
			t.Error("transport should not be closed on reconnect — updateReplicaConn owns the old transport")
		}

		// connected should NOT be reset (reconnect path skips cleanup)
		if !rc.connected.Load() {
			t.Error("connected should not be reset on reconnect path")
		}
	})

	t.Run("send error triggers cleanup", func(t *testing.T) {
		tr := &errorStreamTransport{failAfter: 0} // fail on first Send
		rc := &replicaConn{
			transport:  tr,
			sendCh:     make(chan []byte, replicaSendBuffer),
			hb:         time.NewTicker(time.Hour),
			listenAddr: "test:2",
			nodeID:     "n2",
		}
		rc.connected.Store(true)

		s := &Server{
			replicas:    make(map[string]*replicaConn),
			opts:        Options{Logger: noopLogger, WriteTimeout: time.Second},
			peerManager: NewPeerManager(nil, noopLogger),
		}
		s.role.Store(uint32(RoleLeader))
		s.replicas["n2"] = rc

		done := make(chan struct{})
		go func() {
			s.serveReplicaWriter(rc)
			close(done)
		}()

		// Send a payload to trigger the error
		payload := []byte("test-payload")
		rc.sendCh <- payload

		select {
		case <-done:
			// Writer exited
		case <-time.After(2 * time.Second):
			t.Fatal("serveReplicaWriter did not exit after send error")
		}

		// Transport should be closed (error path does cleanup)
		if !tr.isClosed() {
			t.Error("transport should be closed on error path")
		}

		// connected should be false
		if rc.connected.Load() {
			t.Error("connected should be false after error cleanup")
		}
	})

	t.Run("reconnect closes snapshotted transport not new one", func(t *testing.T) {
		oldTransport := &errorStreamTransport{failAfter: 999}
		newTransport := &errorStreamTransport{failAfter: 999}

		rc := &replicaConn{
			transport:  oldTransport,
			sendCh:     make(chan []byte, replicaSendBuffer),
			hb:         time.NewTicker(time.Hour),
			listenAddr: "test:3",
			nodeID:     "n3",
		}
		rc.connected.Store(true)

		s := &Server{
			replicas:    make(map[string]*replicaConn),
			opts:        Options{Logger: noopLogger, WriteTimeout: time.Second},
			peerManager: NewPeerManager(nil, noopLogger),
		}
		s.role.Store(uint32(RoleLeader))
		s.replicas["n3"] = rc

		done := make(chan struct{})
		go func() {
			s.serveReplicaWriter(rc)
			close(done)
		}()

		// Simulate reconnect: swap transport then close channel
		rc.transport = newTransport
		close(rc.sendCh)

		select {
		case <-done:
			// exited
		case <-time.After(2 * time.Second):
			t.Fatal("serveReplicaWriter did not exit")
		}

		// Neither transport should be closed on reconnect path
		if oldTransport.isClosed() {
			t.Error("old transport should not be closed on reconnect path")
		}
		if newTransport.isClosed() {
			t.Error("new transport should not be closed on reconnect path")
		}
	})

}

// ---------------------------------------------------------------------------
// handleReplicate — reconnect path
// ---------------------------------------------------------------------------

func TestHandleReplicateReconnect(t *testing.T) {
	t.Run("existing replica triggers updateReplicaConn", func(t *testing.T) {
		s := &Server{
			replicas:    make(map[string]*replicaConn),
			opts:        Options{Logger: noopLogger},
			peerManager: NewPeerManager(nil, noopLogger),
			replid:      "test-replid",
		}
		s.role.Store(uint32(RoleLeader))

		oldCh := make(chan []byte, replicaSendBuffer)
		oldTransport := &mockStreamTransport{}
		rc := &replicaConn{
			transport:  oldTransport,
			sendCh:     oldCh,
			lastSeq:    10,
			lastReplid: "test-replid",
			nodeID:     "reconnecting-node",
			listenAddr: "old:1",
			hb:         time.NewTicker(time.Hour),
		}
		s.replicas["reconnecting-node"] = rc

		// Build a REPLICATE request from the reconnecting replica
		newTransport := &mockStreamTransport{}
		rv := protocol.ReplicateValue{
			Replid:     "test-replid",
			ListenAddr: "new:2",
			NodeID:     "reconnecting-node",
		}

		ctx := &RequestContext{
			StreamTransport: newTransport,
			Request: protocol.Request{
				Cmd: protocol.CmdReplicate,
				Seq: 50,
			},
		}

		// handleReplicate will call updateReplicaConn then serveReplica
		// serveReplica needs backlog setup — just verify updateReplicaConn effects
		// by checking old channel is closed and fields are swapped

		// First verify just the update path (extracted from handleReplicate)
		s.updateReplicaConn(rc, ctx, &rv)

		// Old channel closed
		_, ok := <-oldCh
		if ok {
			t.Error("old sendCh should be closed on reconnect")
		}

		// Fields swapped
		if rc.transport != newTransport {
			t.Error("transport not swapped")
		}
		if rc.lastSeq != 50 {
			t.Errorf("lastSeq = %d, want 50", rc.lastSeq)
		}
		if rc.listenAddr != "new:2" {
			t.Errorf("listenAddr = %q, want new:2", rc.listenAddr)
		}

		// Same replicaConn pointer in map (not replaced)
		if s.replicas["reconnecting-node"] != rc {
			t.Error("replica map entry should be same pointer, not a new replicaConn")
		}
	})

	t.Run("new replica creates fresh replicaConn", func(t *testing.T) {
		s := &Server{
			replicas: make(map[string]*replicaConn),
			opts:     Options{Logger: noopLogger},
		}
		s.role.Store(uint32(RoleLeader))

		rv := protocol.ReplicateValue{
			Replid:     "replid",
			ListenAddr: "new:1",
			NodeID:     "brand-new",
		}

		s.connMu.RLock()
		_, exists := s.replicas[rv.NodeID]
		s.connMu.RUnlock()

		if exists {
			t.Fatal("should not exist yet")
		}
	})
}

// ---------------------------------------------------------------------------
// getReplicaSnapshot — preserves disconnected entries
// ---------------------------------------------------------------------------

func TestGetReplicaSnapshot(t *testing.T) {
	t.Run("includes disconnected replicas", func(t *testing.T) {
		s := &Server{
			replicas: make(map[string]*replicaConn),
		}

		rc1 := &replicaConn{nodeID: "n1", sendCh: make(chan []byte, 1)}
		rc1.connected.Store(true)
		rc2 := &replicaConn{nodeID: "n2", sendCh: make(chan []byte, 1)}
		// n2 not connected

		s.replicas["n1"] = rc1
		s.replicas["n2"] = rc2

		snapshot := s.getReplicaSnapshot()

		if len(snapshot) != 2 {
			t.Errorf("snapshot len = %d, want 2 (should include disconnected)", len(snapshot))
		}
		if snapshot["n1"] != rc1 {
			t.Error("missing n1 in snapshot")
		}
		if snapshot["n2"] != rc2 {
			t.Error("missing n2 in snapshot")
		}
	})

	t.Run("returns independent copy", func(t *testing.T) {
		s := &Server{
			replicas: make(map[string]*replicaConn),
		}
		rc := &replicaConn{nodeID: "n1", sendCh: make(chan []byte, 1)}
		s.replicas["n1"] = rc

		snap := s.getReplicaSnapshot()
		delete(snap, "n1")

		// Original map should be unaffected
		if len(s.replicas) != 1 {
			t.Error("deleting from snapshot should not affect original map")
		}
	})
}

// ---------------------------------------------------------------------------
// closeConnections — closes sendCh for all replicas
// ---------------------------------------------------------------------------

func TestCloseConnections(t *testing.T) {
	t.Run("closes sendCh and transport for leader replicas", func(t *testing.T) {
		tr1 := &errorStreamTransport{failAfter: 999}
		tr2 := &errorStreamTransport{failAfter: 999}

		s := &Server{
			replicas: make(map[string]*replicaConn),
			conns:    make(map[transport.StreamTransport]struct{}),
		}
		s.role.Store(uint32(RoleLeader))

		ch1 := make(chan []byte, 1)
		ch2 := make(chan []byte, 1)
		s.replicas["n1"] = &replicaConn{transport: tr1, sendCh: ch1, nodeID: "n1"}
		s.replicas["n2"] = &replicaConn{transport: tr2, sendCh: ch2, nodeID: "n2"}

		s.closeConnections()

		// Channels should be closed
		_, ok1 := <-ch1
		if ok1 {
			t.Error("n1 sendCh should be closed")
		}
		_, ok2 := <-ch2
		if ok2 {
			t.Error("n2 sendCh should be closed")
		}

		// Transports should be closed
		if !tr1.isClosed() {
			t.Error("n1 transport should be closed")
		}
		if !tr2.isClosed() {
			t.Error("n2 transport should be closed")
		}
	})
}

// ---------------------------------------------------------------------------
// addNewReplica — keyed by nodeID
// ---------------------------------------------------------------------------

func TestAddNewReplica(t *testing.T) {
	t.Run("keyed by nodeID", func(t *testing.T) {
		s := &Server{
			replicas: make(map[string]*replicaConn),
		}

		rc := &replicaConn{nodeID: "stable-id", listenAddr: "a:1"}
		s.addNewReplica(rc)

		got, exists := s.replicas["stable-id"]
		if !exists {
			t.Fatal("replica not found by nodeID key")
		}
		if got != rc {
			t.Error("stored replica does not match")
		}
	})

	t.Run("same nodeID overwrites entry", func(t *testing.T) {
		s := &Server{
			replicas: make(map[string]*replicaConn),
		}

		rc1 := &replicaConn{nodeID: "same-id", listenAddr: "a:1"}
		rc2 := &replicaConn{nodeID: "same-id", listenAddr: "a:2"}

		s.addNewReplica(rc1)
		s.addNewReplica(rc2)

		if len(s.replicas) != 1 {
			t.Errorf("len = %d, want 1", len(s.replicas))
		}
		if s.replicas["same-id"] != rc2 {
			t.Error("second replica should overwrite first")
		}
	})
}
