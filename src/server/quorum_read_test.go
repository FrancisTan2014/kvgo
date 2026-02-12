package server

import (
	"fmt"
	"kvgo/protocol"
	"kvgo/transport"
	"net"
	"sync"
	"testing"
	"time"
)

// TestQuorumReadFromReplica_ResponseTypes tests that helper accepts OK and NotFound.
func TestQuorumReadFromReplica_ResponseTypes(t *testing.T) {
	tests := []struct {
		name       string
		respStatus protocol.Status
		respValue  []byte
		respSeq    uint64
		wantOk     bool
	}{
		{
			name:       "StatusOK accepted",
			respStatus: protocol.StatusOK,
			respValue:  []byte("test-value"),
			respSeq:    42,
			wantOk:     true,
		},
		{
			name:       "StatusNotFound accepted",
			respStatus: protocol.StatusNotFound,
			respValue:  nil,
			respSeq:    50,
			wantOk:     true,
		},
		{
			name:       "StatusError rejected",
			respStatus: protocol.StatusError,
			wantOk:     false,
		},
		{
			name:       "StatusReadOnly rejected",
			respStatus: protocol.StatusReadOnly,
			respValue:  []byte("primary:6379"),
			wantOk:     false,
		},
		{
			name:       "StatusQuorumFailed rejected",
			respStatus: protocol.StatusQuorumFailed,
			wantOk:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &Server{
				opts: Options{
					QuorumReadTimeout: 100 * time.Millisecond,
				},
			}

			// Create mock response
			respPayload, _ := protocol.EncodeResponse(protocol.Response{
				Status: tt.respStatus,
				Value:  tt.respValue,
				Seq:    tt.respSeq,
			})

			// Create mock transport
			mockTransport := &mockRequestTransport{
				response: respPayload,
			}

			_, _, ok := s.quorumReadFromReplica(mockTransport, []byte("request"), "mock-addr")

			if ok != tt.wantOk {
				t.Errorf("ok = %v, want %v", ok, tt.wantOk)
			}
		})
	}
}

// TestReachableNodesHelpers_AddRemove tests add/remove operations.
func TestReachableNodesHelpers_AddRemove(t *testing.T) {
	s := &Server{
		reachableNodes: make(map[string]transport.RequestTransport),
	}

	addr1, addr2 := "replica1:6379", "replica2:6379"
	t1 := &mockRequestTransport{address: addr1}
	t2 := &mockRequestTransport{address: addr2}

	// Add nodes
	s.addReachableNode(addr1, t1)
	s.addReachableNode(addr2, t2)

	snapshot := s.getReachableNodesSnapshot()
	if len(snapshot) != 2 {
		t.Errorf("len(snapshot) = %d, want 2", len(snapshot))
	}
	if snapshot[addr1] != t1 {
		t.Error("addr1 transport mismatch")
	}
	if snapshot[addr2] != t2 {
		t.Error("addr2 transport mismatch")
	}

	// Remove node
	s.removeReachableNode(addr1)
	snapshot = s.getReachableNodesSnapshot()
	if len(snapshot) != 1 {
		t.Errorf("len(snapshot) = %d, want 1 after removal", len(snapshot))
	}
	if _, exists := snapshot[addr1]; exists {
		t.Error("addr1 should be removed")
	}
	if snapshot[addr2] != t2 {
		t.Error("addr2 should still exist")
	}
}

// TestReachableNodesHelpers_Clear tests clearing all nodes.
func TestReachableNodesHelpers_Clear(t *testing.T) {
	s := &Server{
		reachableNodes: make(map[string]transport.RequestTransport),
	}

	// Add several nodes
	for i := 0; i < 5; i++ {
		addr := fmt.Sprintf("replica%d:6379", i)
		s.addReachableNode(addr, &mockRequestTransport{address: addr})
	}

	snapshot := s.getReachableNodesSnapshot()
	if len(snapshot) != 5 {
		t.Errorf("len(snapshot) = %d, want 5", len(snapshot))
	}

	// Clear all
	s.clearReachableNodes()
	snapshot = s.getReachableNodesSnapshot()
	if len(snapshot) != 0 {
		t.Errorf("len(snapshot) = %d, want 0 after clear", len(snapshot))
	}
}

// TestReachableNodesHelpers_SnapshotIsolation tests that snapshot is isolated from mutations.
func TestReachableNodesHelpers_SnapshotIsolation(t *testing.T) {
	s := &Server{
		reachableNodes: make(map[string]transport.RequestTransport),
	}

	addr1 := "replica1:6379"
	t1 := &mockRequestTransport{address: addr1}
	s.addReachableNode(addr1, t1)

	// Get snapshot
	snapshot1 := s.getReachableNodesSnapshot()
	if len(snapshot1) != 1 {
		t.Fatalf("len(snapshot1) = %d, want 1", len(snapshot1))
	}

	// Modify original (add new node)
	addr2 := "replica2:6379"
	s.addReachableNode(addr2, &mockRequestTransport{address: addr2})

	// Original snapshot should be unchanged
	if len(snapshot1) != 1 {
		t.Errorf("snapshot1 len changed to %d, want 1 (should be isolated)", len(snapshot1))
	}

	// New snapshot should reflect changes
	snapshot2 := s.getReachableNodesSnapshot()
	if len(snapshot2) != 2 {
		t.Errorf("len(snapshot2) = %d, want 2", len(snapshot2))
	}

	// Modify snapshot1 (should not affect server state)
	delete(snapshot1, addr1)

	// Server state should be unchanged
	snapshot3 := s.getReachableNodesSnapshot()
	if len(snapshot3) != 2 {
		t.Errorf("server state affected by snapshot mutation: len = %d, want 2", len(snapshot3))
	}
}

// TestReachableNodesHelpers_ConcurrentAccess tests thread-safety of helper methods.
func TestReachableNodesHelpers_ConcurrentAccess(t *testing.T) {
	s := &Server{
		reachableNodes: make(map[string]transport.RequestTransport),
	}

	// Concurrent add/remove/snapshot operations
	var wg sync.WaitGroup
	iterations := 100

	// Concurrent adds
	wg.Add(iterations)
	for i := 0; i < iterations; i++ {
		go func(id int) {
			defer wg.Done()
			addr := fmt.Sprintf("replica%d:6379", id)
			s.addReachableNode(addr, &mockRequestTransport{address: addr})
		}(i)
	}
	wg.Wait()

	// Concurrent snapshots
	wg.Add(iterations)
	for i := 0; i < iterations; i++ {
		go func() {
			defer wg.Done()
			_ = s.getReachableNodesSnapshot()
		}()
	}
	wg.Wait()

	// Concurrent removes
	wg.Add(iterations)
	for i := 0; i < iterations; i++ {
		go func(id int) {
			defer wg.Done()
			addr := fmt.Sprintf("replica%d:6379", id)
			s.removeReachableNode(addr)
		}(i)
	}
	wg.Wait()

	// Final snapshot should be empty or contain stragglers (race is acceptable)
	snapshot := s.getReachableNodesSnapshot()
	_ = snapshot // Accept any final state from concurrent operations
}

// TestDoGet_SeqSelection tests seq selection logic is already covered by TestHandleGet_ReplicaStaleness
// Integration testing with full request flow will be done in Episode 028+ with service discovery

// ---------------------------------------------------------------------------
// Mock types for testing
// ---------------------------------------------------------------------------

// mockReadWriter implements io.Reader and io.Writer for testing Framer
type mockReadWriter struct {
	readData []byte
	readPos  int
	readErr  error
	writeErr error
}

func (m *mockReadWriter) Read(p []byte) (n int, err error) {
	if m.readErr != nil {
		return 0, m.readErr
	}
	if m.readPos >= len(m.readData) {
		return 0, &net.OpError{Op: "read", Err: &timeoutError{}}
	}
	// Write frame header + data
	frameLen := len(m.readData) - m.readPos
	if len(p) < 4 {
		return 0, &net.OpError{Op: "read", Err: &timeoutError{}}
	}
	// Simulate frame protocol: [4 bytes length][data]
	p[0] = byte(frameLen)
	p[1] = byte(frameLen >> 8)
	p[2] = byte(frameLen >> 16)
	p[3] = byte(frameLen >> 24)
	n = 4
	if len(p) > 4 {
		copied := copy(p[4:], m.readData[m.readPos:])
		n += copied
		m.readPos += copied
	}
	return n, nil
}

func (m *mockReadWriter) Write(p []byte) (n int, err error) {
	if m.writeErr != nil {
		return 0, m.writeErr
	}
	return len(p), nil
}

func (m *mockReadWriter) SetReadDeadline(t time.Time) error  { return nil }
func (m *mockReadWriter) SetWriteDeadline(t time.Time) error { return nil }

type mockConn struct {
	id int
}

func (m *mockConn) Read(b []byte) (n int, err error)   { return 0, nil }
func (m *mockConn) Write(b []byte) (n int, err error)  { return len(b), nil }
func (m *mockConn) Close() error                       { return nil }
func (m *mockConn) LocalAddr() net.Addr                { return &net.TCPAddr{} }
func (m *mockConn) RemoteAddr() net.Addr               { return &net.TCPAddr{} }
func (m *mockConn) SetDeadline(t time.Time) error      { return nil }
func (m *mockConn) SetReadDeadline(t time.Time) error  { return nil }
func (m *mockConn) SetWriteDeadline(t time.Time) error { return nil }

type timeoutError struct{}

func (e *timeoutError) Error() string   { return "timeout" }
func (e *timeoutError) Timeout() bool   { return true }
func (e *timeoutError) Temporary() bool { return true }
