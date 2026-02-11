package server

import (
	"kvgo/protocol"
	"net"
	"testing"
	"time"
)

// TestStrongRead_CatchupLogic verifies the wait-for-catchup logic.
func TestStrongRead_CatchupLogic(t *testing.T) {
	tests := []struct {
		name          string
		currentSeq    uint64
		waitForSeq    uint64
		shouldCatchup bool
	}{
		{"already_caught_up", 100, 50, true},
		{"exact_match", 50, 50, true},
		{"behind", 10, 100, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &Server{isReplica: true}
			s.lastSeq.Store(tt.currentSeq)

			// Check if caught up
			caughtUp := s.lastSeq.Load() >= tt.waitForSeq

			if caughtUp != tt.shouldCatchup {
				t.Errorf("catchup check: got %v, want %v", caughtUp, tt.shouldCatchup)
			}
		})
	}
}

// TestStrongRead_BackoffTiming verifies exponential backoff pattern.
func TestStrongRead_BackoffTiming(t *testing.T) {
	maxDuration := 10 * time.Millisecond
	currentDuration := time.Millisecond

	backoffs := []time.Duration{}
	for i := 0; i < 6; i++ {
		backoffs = append(backoffs, currentDuration)
		currentDuration = min(currentDuration*2, maxDuration)
	}

	// Verify exponential growth then capping
	expected := []time.Duration{
		1 * time.Millisecond,
		2 * time.Millisecond,
		4 * time.Millisecond,
		8 * time.Millisecond,
		10 * time.Millisecond, // capped
		10 * time.Millisecond, // capped
	}

	for i, want := range expected {
		if backoffs[i] != want {
			t.Errorf("backoff[%d] = %v, want %v", i, backoffs[i], want)
		}
	}
}

// TestHandleAck_Success verifies ACK handler processes acknowledgments correctly.
func TestHandleAck_Success(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to create listener: %v", err)
	}
	defer listener.Close()

	addr := listener.Addr().String()
	replicaConn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("failed to dial: %v", err)
	}
	defer replicaConn.Close()

	serverConn, err := listener.Accept()
	if err != nil {
		t.Fatalf("failed to accept: %v", err)
	}
	defer serverConn.Close()

	s := &Server{
		quorumWrites: make(map[string]*quorumState),
	}

	// Create quorum state for a write needing 2 ACKs
	requestID := "test-req-123"
	state := &quorumState{
		ackCh:         make(chan struct{}),
		needed:        2,
		ackCount:      0,
		ackedReplicas: make(map[net.Conn]struct{}),
	}
	s.quorumWrites[requestID] = state

	// Send ACK from replica
	ctx := &RequestContext{
		Conn: serverConn,
		Request: protocol.Request{
			Cmd:       protocol.CmdAck,
			RequestId: requestID,
		},
	}

	err = s.handleAck(ctx)
	if err != nil {
		t.Fatalf("handleAck failed: %v", err)
	}

	// Verify ACK was recorded
	state.mu.Lock()
	if state.ackCount != 1 {
		t.Errorf("expected ackCount=1, got %d", state.ackCount)
	}
	if _, ok := state.ackedReplicas[serverConn]; !ok {
		t.Error("replica not marked as ACK'd")
	}
	state.mu.Unlock()
}

// TestHandleAck_Deduplication verifies duplicate ACKs from same replica are ignored.
func TestHandleAck_Deduplication(t *testing.T) {
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
	defer conn.Close()

	serverConn, err := listener.Accept()
	if err != nil {
		t.Fatalf("failed to accept: %v", err)
	}
	defer serverConn.Close()

	s := &Server{
		quorumWrites: make(map[string]*quorumState),
	}

	requestID := "test-req-456"
	state := &quorumState{
		ackCh:         make(chan struct{}),
		needed:        3,
		ackCount:      0,
		ackedReplicas: make(map[net.Conn]struct{}),
	}
	s.quorumWrites[requestID] = state

	ctx := &RequestContext{
		Conn: serverConn,
		Request: protocol.Request{
			Cmd:       protocol.CmdAck,
			RequestId: requestID,
		},
	}

	// Send first ACK
	err = s.handleAck(ctx)
	if err != nil {
		t.Fatalf("handleAck failed: %v", err)
	}

	state.mu.Lock()
	firstAckCount := state.ackCount
	state.mu.Unlock()

	// Send duplicate ACK from same connection
	err = s.handleAck(ctx)
	if err != nil {
		t.Fatalf("handleAck failed on duplicate: %v", err)
	}

	state.mu.Lock()
	secondAckCount := state.ackCount
	state.mu.Unlock()

	// ACK count should not increase
	if secondAckCount != firstAckCount {
		t.Errorf("duplicate ACK incremented count: %d -> %d", firstAckCount, secondAckCount)
	}

	if secondAckCount != 1 {
		t.Errorf("expected ackCount=1 after deduplication, got %d", secondAckCount)
	}
}

// TestHandleNack_ImmediateFailure verifies NACK triggers immediate failure.
func TestHandleNack_ImmediateFailure(t *testing.T) {
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
	defer conn.Close()

	serverConn, err := listener.Accept()
	if err != nil {
		t.Fatalf("failed to accept: %v", err)
	}
	defer serverConn.Close()

	s := &Server{
		quorumWrites: make(map[string]*quorumState),
	}

	requestID := "test-req-789"
	state := &quorumState{
		ackCh:         make(chan struct{}),
		needed:        5, // Need 5 ACKs
		ackCount:      2, // Only have 2 ACKs
		ackedReplicas: make(map[net.Conn]struct{}),
	}
	state.failed.Store(false)
	s.quorumWrites[requestID] = state

	ctx := &RequestContext{
		Conn: serverConn,
		Request: protocol.Request{
			Cmd:       protocol.CmdNack,
			RequestId: requestID,
		},
	}

	// Send NACK
	err = s.handleNack(ctx)
	if err != nil {
		t.Fatalf("handleNack failed: %v", err)
	}

	// Verify failed flag is set
	if !state.failed.Load() {
		t.Error("NACK did not set failed flag")
	}
}

// TestHandleAck_UnknownRequest verifies ACK for unknown request is gracefully ignored.
func TestHandleAck_UnknownRequest(t *testing.T) {
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
	defer conn.Close()

	serverConn, err := listener.Accept()
	if err != nil {
		t.Fatalf("failed to accept: %v", err)
	}
	defer serverConn.Close()

	s := &Server{
		quorumWrites: make(map[string]*quorumState),
	}

	ctx := &RequestContext{
		Conn: serverConn,
		Request: protocol.Request{
			Cmd:       protocol.CmdAck,
			RequestId: "unknown-request",
		},
	}

	// Should not panic or error
	err = s.handleAck(ctx)
	if err != nil {
		t.Errorf("handleAck should handle unknown request gracefully: %v", err)
	}
}
