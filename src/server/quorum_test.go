package server

import (
	"context"
	"kvgo/protocol"
	"net"
	"sync"
	"testing"
	"time"
)

// TestComputeQuorum verifies quorum calculation (unit test)
func TestComputeQuorum(t *testing.T) {
	tests := []struct {
		name     string
		replicas int
		want     int
	}{
		{"1 node (primary only)", 0, 1}, // (0+1)/2 + 1 = 1
		{"2 nodes", 1, 2},               // (1+1)/2 + 1 = 2
		{"3 nodes", 2, 2},               // (2+1)/2 + 1 = 2
		{"4 nodes", 3, 3},               // (3+1)/2 + 1 = 3
		{"5 nodes", 4, 3},               // (4+1)/2 + 1 = 3
		{"101 nodes", 100, 51},          // (100+1)/2 + 1 = 51
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &Server{
				replicas: make(map[net.Conn]*replicaConn),
			}
			// Populate fake replicas with distinct pointers
			fakeConns := make([]fakeConn, tt.replicas)
			for i := 0; i < tt.replicas; i++ {
				s.replicas[&fakeConns[i]] = &replicaConn{}
			}

			got := s.computeQuorum()

			if got != tt.want {
				t.Errorf("computeQuorum() = %d, want %d (replicas=%d, actual len=%d)", got, tt.want, tt.replicas, len(s.replicas))
			}
		})
	}
}

// TestQuorumStateThreadSafety verifies sync.Once prevents double-close (unit test)
func TestQuorumStateThreadSafety(t *testing.T) {
	state := &quorumState{
		needed:        2,
		ackCh:         make(chan struct{}),
		ackedReplicas: make(map[net.Conn]struct{}),
	}

	// Simulate 10 concurrent ACKs arriving
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			state.mu.Lock()
			state.ackCount++
			count := state.ackCount
			state.mu.Unlock()

			if count >= int32(state.needed) {
				state.closeOnce.Do(func() {
					close(state.ackCh)
				})
			}
		}()
	}

	wg.Wait()

	// Verify channel is closed exactly once (no panic)
	select {
	case <-state.ackCh:
		// good: channel closed
	case <-time.After(100 * time.Millisecond):
		t.Fatal("ackCh was not closed")
	}

	// Final count should be 10
	state.mu.Lock()
	finalCount := state.ackCount
	state.mu.Unlock()
	if finalCount != 10 {
		t.Errorf("ackCount = %d, want 10", finalCount)
	}
}

// TestQuorumWrite_Success verifies quorum write succeeds with majority ACKs (integration test)
func TestQuorumWrite_Success(t *testing.T) {
	// Setup: 1 primary + 2 replicas (quorum = 2)
	primary, replicas := setupQuorumCluster(t, 2)
	defer shutdownCluster(t, primary, replicas)

	// Client sends quorum write to primary
	conn, err := net.Dial("tcp", primary.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	f := protocol.NewConnFramer(conn)

	req := protocol.Request{
		Cmd:           protocol.CmdPut,
		Key:           []byte("test-key"),
		Value:         []byte("test-value"),
		RequireQuorum: true,
	}
	payload, _ := protocol.EncodeRequest(req)
	if err := f.Write(payload); err != nil {
		t.Fatal(err)
	}

	// Should succeed: 2 ACKs (replicas) >= quorum 2
	resp, err := f.ReadWithTimeout(1 * time.Second)
	if err != nil {
		t.Fatal("quorum write timeout:", err)
	}

	decoded, _ := protocol.DecodeResponse(resp)
	if decoded.Status != protocol.StatusOK {
		t.Errorf("status = %d, want StatusOK", decoded.Status)
	}

	// Verify write persisted on primary
	verifyKeyExists(t, primary, "test-key", "test-value")

	// Verify quorum of nodes have the write (not necessarily all)
	// With 3 nodes (1 primary + 2 replicas), quorum = 2
	// Primary has it (1), need 1 more from replicas
	count := 1 // primary has it
	for i, replica := range replicas {
		if verifyKeyExistsSilent(replica, "test-key", "test-value") {
			count++
			t.Logf("replica %d: has key", i)
		} else {
			t.Logf("replica %d: does not have key (OK - quorum doesn't require all)", i)
		}
	}

	quorum := (len(replicas)+1)/2 + 1 // Same calculation as computeQuorum
	if count < quorum {
		t.Errorf("only %d/%d nodes have the key, need quorum of %d", count, len(replicas)+1, quorum)
	}
}

// TestQuorumWrite_Timeout verifies quorum write times out without majority (integration test)
func TestQuorumWrite_Timeout(t *testing.T) {
	// Setup: 1 primary + 2 replicas (quorum = 2)
	// Kill both replicas so 0 can ACK → quorum fails
	primary, replicas := setupQuorumCluster(t, 2)
	defer shutdownCluster(t, primary, replicas)

	// Kill both replicas before write
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		replicas[i].Shutdown(ctx)
	}

	time.Sleep(500 * time.Millisecond) // Let disconnections settle and connections close

	// Client sends quorum write
	conn, err := net.Dial("tcp", primary.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	f := protocol.NewConnFramer(conn)
	req := protocol.Request{
		Cmd:           protocol.CmdPut,
		Key:           []byte("timeout-key"),
		Value:         []byte("timeout-value"),
		RequireQuorum: true,
	}
	payload, _ := protocol.EncodeRequest(req)
	if err := f.Write(payload); err != nil {
		t.Fatal(err)
	}

	// Should timeout: 1 ACK < quorum 3
	resp, err := f.ReadWithTimeout(1 * time.Second)
	if err != nil {
		t.Fatal("no response:", err)
	}

	decoded, _ := protocol.DecodeResponse(resp)
	if decoded.Status != protocol.StatusError {
		t.Errorf("status = %d, want StatusError (timeout)", decoded.Status)
	}
}

// TestQuorumWrite_PartialReplication verifies async writes don't wait (integration test)
func TestAsyncWrite_NoWait(t *testing.T) {
	// Setup: 1 primary + 2 replicas
	primary, replicas := setupQuorumCluster(t, 2)
	defer shutdownCluster(t, primary, replicas)

	// Kill all replicas
	ctx := context.Background()
	for _, replica := range replicas {
		replica.Shutdown(ctx)
	}

	time.Sleep(100 * time.Millisecond)

	// Client sends ASYNC write (default)
	conn, err := net.Dial("tcp", primary.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	f := protocol.NewConnFramer(conn)
	req := protocol.Request{
		Cmd:           protocol.CmdPut,
		Key:           []byte("async-key"),
		Value:         []byte("async-value"),
		RequireQuorum: false, // async
	}
	payload, _ := protocol.EncodeRequest(req)
	if err := f.Write(payload); err != nil {
		t.Fatal(err)
	}

	// Should succeed immediately (no waiting for replicas)
	resp, err := f.ReadWithTimeout(200 * time.Millisecond)
	if err != nil {
		t.Fatal("async write should not wait:", err)
	}

	decoded, _ := protocol.DecodeResponse(resp)
	if decoded.Status != protocol.StatusOK {
		t.Errorf("status = %d, want StatusOK", decoded.Status)
	}
}

// Helper: setup primary + N replicas
func setupQuorumCluster(t *testing.T, numReplicas int) (*Server, []*Server) {
	t.Helper()

	// Start primary
	primaryDir := t.TempDir()
	primary, err := NewServer(Options{
		Network:            NetworkTCP,
		Host:               "127.0.0.1",
		Port:               0, // random port
		DataDir:            primaryDir,
		QuorumWriteTimeout: 500 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := primary.Start(); err != nil {
		t.Fatal(err)
	}

	// Start replicas
	replicas := make([]*Server, numReplicas)
	for i := 0; i < numReplicas; i++ {
		replicaDir := t.TempDir()
		replica, err := NewServer(Options{
			Network:   NetworkTCP,
			Host:      "127.0.0.1",
			Port:      0,
			DataDir:   replicaDir,
			ReplicaOf: primary.Addr(),
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := replica.Start(); err != nil {
			t.Fatal(err)
		}
		replicas[i] = replica
	}

	// Wait for replication connections to establish
	time.Sleep(200 * time.Millisecond)

	return primary, replicas
}

func shutdownCluster(t *testing.T, primary *Server, replicas []*Server) {
	t.Helper()
	ctx := context.Background()
	for _, replica := range replicas {
		_ = replica.Shutdown(ctx)
	}
	_ = primary.Shutdown(ctx)
}

func verifyKeyExists(t *testing.T, s *Server, key, expectedValue string) {
	t.Helper()
	val, ok := s.db.Get(key)
	if !ok {
		t.Errorf("key %q not found in database", key)
		return
	}
	if string(val) != expectedValue {
		t.Errorf("key %q = %q, want %q", key, val, expectedValue)
	}
}

func verifyKeyExistsSilent(s *Server, key, expectedValue string) bool {
	val, ok := s.db.Get(key)
	if !ok {
		return false
	}
	return string(val) == expectedValue
}

// Fake conn for unit tests
type fakeConn struct {
	id int // Make non-zero-sized so each instance gets unique address
}

func (f *fakeConn) Read(b []byte) (n int, err error)   { return 0, nil }
func (f *fakeConn) Write(b []byte) (n int, err error)  { return len(b), nil }
func (f *fakeConn) Close() error                       { return nil }
func (f *fakeConn) LocalAddr() net.Addr                { return &net.TCPAddr{} }
func (f *fakeConn) RemoteAddr() net.Addr               { return &net.TCPAddr{} }
func (f *fakeConn) SetDeadline(t time.Time) error      { return nil }
func (f *fakeConn) SetReadDeadline(t time.Time) error  { return nil }
func (f *fakeConn) SetWriteDeadline(t time.Time) error { return nil }
