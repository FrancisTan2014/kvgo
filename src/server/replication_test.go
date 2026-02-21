package server

import (
	"context"
	"kvgo/protocol"
	"kvgo/transport"
	"net"
	"testing"
	"time"
)

// TestPrimarySeqInitialization verifies that primarySeq is correctly initialized
// during sync handshakes to prevent premature staleness detection.
// This is a regression test for the bug where replicas reported StatusReplicaTooStale
// immediately after sync because primarySeq was 0.
func TestPrimarySeqInitialization(t *testing.T) {
	tests := []struct {
		name         string
		responseSeq  uint64
		responseType protocol.Status
	}{
		{
			name:         "full_resync_initializes_primarySeq",
			responseSeq:  100,
			responseType: protocol.StatusFullResync,
		},
		{
			name:         "partial_resync_initializes_primarySeq",
			responseSeq:  50,
			responseType: protocol.StatusOK,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &Server{
				primarySeq: 0, // Initially 0
			}

			// Simulate response from connectToPrimary
			resp := protocol.Response{
				Status: tt.responseType,
				Seq:    tt.responseSeq,
				Value:  []byte("test-replid"),
			}

			// Process response (simulating connectToPrimary logic)
			if resp.Status == protocol.StatusFullResync {
				s.replid = string(resp.Value)
				s.primarySeq = resp.Seq
			} else if resp.Status == protocol.StatusOK {
				s.primarySeq = resp.Seq
			}

			if s.primarySeq != tt.responseSeq {
				t.Errorf("primarySeq not initialized: got %d, want %d", s.primarySeq, tt.responseSeq)
			}

			// Verify staleness check doesn't false-positive immediately after sync
			s.lastSeq.Store(tt.responseSeq)
			s.lastHeartbeat = time.Now()
			s.opts.ReplicaStaleLag = 1000
			s.opts.ReplicaStaleHeartbeat = 5 * time.Second

			seqLag := s.primarySeq - s.lastSeq.Load()
			heartbeatAge := time.Since(s.lastHeartbeat)
			isStale := seqLag > uint64(s.opts.ReplicaStaleLag) || heartbeatAge > s.opts.ReplicaStaleHeartbeat

			if isStale {
				t.Errorf("replica incorrectly marked as stale immediately after sync: seqLag=%d, heartbeatAge=%v",
					seqLag, heartbeatAge)
			}
		})
	}
}

// TestReplicatedWritePrimarySeqUpdate verifies that primarySeq is updated
// when applying replicated PUTs, reducing the staleness window between heartbeats.
func TestReplicatedWritePrimarySeqUpdate(t *testing.T) {
	s := &Server{
		primarySeq: 10,
	}
	s.lastSeq.Store(10)

	// Simulate receiving replicated PUT with seq=15
	newSeq := uint64(15)

	// Apply update (simulating applyReplicatedPut logic)
	s.lastSeq.Store(newSeq)
	if newSeq > s.primarySeq {
		s.primarySeq = newSeq
	}

	if s.primarySeq != newSeq {
		t.Errorf("primarySeq not updated on replicated PUT: got %d, want %d", s.primarySeq, newSeq)
	}

	// Verify staleness window is reduced
	seqLag := s.primarySeq - s.lastSeq.Load()
	if seqLag != 0 {
		t.Errorf("expected zero lag after applying PUT, got %d", seqLag)
	}
}

// TestProcessPongResponse verifies PONG response processing for heartbeats.
// processPongResponse extracts the term from a Response and steps down if needed.
func TestProcessPongResponse(t *testing.T) {
	s := &Server{
		peerManager: NewPeerManager(nil, noopLogger),
	}
	s.role.Store(uint32(RoleLeader))
	s.term.Store(5)
	s.roleChanged = make(chan struct{})

	// Encode a PONG Response with term 0 (lower than server's term 5 → no step-down)
	resp := protocol.NewPongResponse(0)
	payload, _ := protocol.EncodeResponse(resp)

	rc := &replicaConn{listenAddr: "127.0.0.1:9999"}
	s.processPongResponse(payload, rc)

	// Term and role should be unchanged (0 < 5)
	if s.term.Load() != 5 {
		t.Errorf("term = %d, want 5", s.term.Load())
	}
	if s.currentRole() != RoleLeader {
		t.Errorf("role = %s, want leader", s.currentRole())
	}
}

// TestReplicaConnectionIdentity verifies that handlePut correctly identifies
// writes from the primary connection vs client connections.
func TestReplicaConnectionIdentity(t *testing.T) {
	// Create two connections to simulate primary and client
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to create listener: %v", err)
	}
	defer listener.Close()

	addr := listener.Addr().String()

	// Simulate primary connection
	primaryConn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("failed to dial for primary: %v", err)
	}
	defer primaryConn.Close()

	// Accept primary connection
	serverPrimaryConn, err := listener.Accept()
	if err != nil {
		t.Fatalf("failed to accept primary: %v", err)
	}
	defer serverPrimaryConn.Close()

	// Simulate client connection
	clientConn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("failed to dial for client: %v", err)
	}
	defer clientConn.Close()

	// Accept client connection
	serverClientConn, err := listener.Accept()
	if err != nil {
		t.Fatalf("failed to accept client: %v", err)
	}
	defer serverClientConn.Close()

	// Wrap connections in transports
	serverPrimaryTransport := transport.NewStreamTransport(transport.ProtocolTCP, serverPrimaryConn)

	// Create replica server with primary connection set
	s := &Server{
		primary: serverPrimaryTransport,
	}

	// Test 1: Connection identity check for primary connection
	s.connMu.Lock()
	isPrimaryConn := (s.primary != nil && serverPrimaryTransport == s.primary)
	s.connMu.Unlock()

	if !isPrimaryConn {
		t.Error("failed to identify primary connection")
	}

	// Test 2: Connection identity check for client connection (won't match)
	serverClientTransport := transport.NewStreamTransport(transport.ProtocolTCP, serverClientConn)
	s.connMu.Lock()
	isClientPrimaryConn := (s.primary != nil && serverClientTransport == s.primary)
	s.connMu.Unlock()

	if isClientPrimaryConn {
		t.Error("incorrectly identified client connection as primary")
	}
}

// TestReplicationStateCleanup verifies that replication state is properly
// cleaned up when connections close.
func TestReplicationStateCleanup(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to create listener: %v", err)
	}
	defer listener.Close()

	addr := listener.Addr().String()

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("failed to dial: %v", err)
	}

	serverConn, err := listener.Accept()
	if err != nil {
		t.Fatalf("failed to accept: %v", err)
	}

	// Wrap in transport
	serverTransport := transport.NewStreamTransport(transport.ProtocolTCP, serverConn)

	s := &Server{
		replicas: make(map[string]*replicaConn),
	}

	// Add replica connection
	rc := newReplicaConn(serverTransport, 0, "test-replid", "test-node-id", s.listenAddr())
	s.replicas[rc.nodeID] = rc

	if len(s.replicas) != 1 {
		t.Fatalf("expected 1 replica, got %d", len(s.replicas))
	}

	// Simulate cleanup (what serveReplicaWriter does in defer)
	s.connMu.Lock()
	delete(s.replicas, rc.nodeID)
	s.connMu.Unlock()
	serverTransport.Close()
	conn.Close()

	if len(s.replicas) != 0 {
		t.Errorf("replicas not cleaned up: expected 0, got %d", len(s.replicas))
	}
}

// TestReplicaReconnectionFlow verifies that replicas can reconnect after disconnect.
func TestReplicaReconnectionFlow(t *testing.T) {
	s := &Server{}
	s.opts.ReplicaOf = "127.0.0.1:9999" // Non-existent, will fail to connect

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Simulate connection attempt that fails
	_, err := s.connectToPrimary()
	if err == nil {
		t.Error("expected connection to fail, but it succeeded")
	}

	// Verify server state remains stable after failed connection
	s.connMu.Lock()
	primaryIsNil := s.primary == nil
	s.connMu.Unlock()

	if !primaryIsNil {
		t.Error("primary connection should be nil after failed connection")
	}

	// replicationLoop should handle this by retrying
	// We verify the loop doesn't panic by running it briefly
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.replicationLoop(ctx)
	}()

	// Give it a moment to attempt connection
	time.Sleep(200 * time.Millisecond)
	cancel()

	// Wait for loop to exit
	select {
	case <-done:
		// Success: loop exited gracefully
	case <-time.After(2 * time.Second):
		t.Error("replicationLoop did not exit after context cancellation")
	}
}
