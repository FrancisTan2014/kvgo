package server

import (
	"strconv"
	"sync"
	"testing"
	"time"
)

// TestComputeReplicaAcksNeeded verifies replica ACK calculation (unit test)
func TestComputeReplicaAcksNeeded(t *testing.T) {
	tests := []struct {
		name     string
		replicas int
		want     int
	}{
		{"1 node (primary only)", 0, 0}, // quorum=1, need 0 replica ACKs
		{"2 nodes", 1, 1},               // quorum=2, need 1 replica ACK
		{"3 nodes", 2, 1},               // quorum=2, need 1 replica ACK
		{"4 nodes", 3, 2},               // quorum=3, need 2 replica ACKs
		{"5 nodes", 4, 2},               // quorum=3, need 2 replica ACKs
		{"101 nodes", 100, 50},          // quorum=51, need 50 replica ACKs
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &Server{
				replicas: make(map[string]*replicaConn),
			}
			// Populate fake replicas with distinct keys
			for i := 0; i < tt.replicas; i++ {
				nodeID := "node" + strconv.Itoa(i)
				s.replicas[nodeID] = &replicaConn{nodeID: nodeID}
			}

			got := s.computeReplicaAcksNeeded()

			if got != tt.want {
				t.Errorf("computeReplicaAcksNeeded() = %d, want %d (replicas=%d, actual len=%d)", got, tt.want, tt.replicas, len(s.replicas))
			}
		})
	}
}

// TestQuorumStateThreadSafety verifies sync.Once prevents double-close (unit test)
func TestQuorumStateThreadSafety(t *testing.T) {
	state := &quorumWriteState{
		needed:        2,
		ackCh:         make(chan struct{}),
		ackedReplicas: make(map[string]struct{}),
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
