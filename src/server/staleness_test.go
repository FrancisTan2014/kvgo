package server

import (
	"sync/atomic"
	"testing"
	"time"
)

// TestIsStaleness tests the staleness detection logic in isolation.
// This is a unit test that directly tests the isStaleness() function
// without starting servers or establishing network connections.
func TestIsStaleness(t *testing.T) {
	tests := []struct {
		name                  string
		isReplica             bool
		primarySeq            uint64
		lastSeq               uint64
		heartbeatAge          time.Duration
		replicaStaleHeartbeat time.Duration
		replicaStaleLag       int
		wantStale             bool
	}{
		{
			name:                  "primary never stale",
			isReplica:             false,
			primarySeq:            100,
			lastSeq:               0,
			heartbeatAge:          10 * time.Hour,
			replicaStaleHeartbeat: 5 * time.Second,
			replicaStaleLag:       1000,
			wantStale:             false,
		},
		{
			name:                  "replica fresh, no lag",
			isReplica:             true,
			primarySeq:            100,
			lastSeq:               100,
			heartbeatAge:          1 * time.Second,
			replicaStaleHeartbeat: 5 * time.Second,
			replicaStaleLag:       1000,
			wantStale:             false,
		},
		{
			name:                  "replica stale by heartbeat",
			isReplica:             true,
			primarySeq:            100,
			lastSeq:               100,
			heartbeatAge:          10 * time.Second,
			replicaStaleHeartbeat: 5 * time.Second,
			replicaStaleLag:       1000,
			wantStale:             true,
		},
		{
			name:                  "replica stale by sequence lag",
			isReplica:             true,
			primarySeq:            2000,
			lastSeq:               100,
			heartbeatAge:          1 * time.Second,
			replicaStaleHeartbeat: 5 * time.Second,
			replicaStaleLag:       1000,
			wantStale:             true,
		},
		{
			name:                  "replica stale by both",
			isReplica:             true,
			primarySeq:            2000,
			lastSeq:               100,
			heartbeatAge:          10 * time.Second,
			replicaStaleHeartbeat: 5 * time.Second,
			replicaStaleLag:       1000,
			wantStale:             true,
		},
		{
			name:                  "replica at exact heartbeat threshold (not stale)",
			isReplica:             true,
			primarySeq:            100,
			lastSeq:               100,
			heartbeatAge:          5 * time.Second,
			replicaStaleHeartbeat: 5 * time.Second,
			replicaStaleLag:       1000,
			wantStale:             false,
		},
		{
			name:                  "replica at exact sequence threshold (not stale)",
			isReplica:             true,
			primarySeq:            1100,
			lastSeq:               100,
			heartbeatAge:          1 * time.Second,
			replicaStaleHeartbeat: 5 * time.Second,
			replicaStaleLag:       1000,
			wantStale:             false,
		},
		{
			name:                  "replica one over heartbeat threshold (stale)",
			isReplica:             true,
			primarySeq:            100,
			lastSeq:               100,
			heartbeatAge:          5*time.Second + 1*time.Nanosecond,
			replicaStaleHeartbeat: 5 * time.Second,
			replicaStaleLag:       1000,
			wantStale:             true,
		},
		{
			name:                  "replica one over sequence threshold (stale)",
			isReplica:             true,
			primarySeq:            1101,
			lastSeq:               100,
			heartbeatAge:          1 * time.Second,
			replicaStaleHeartbeat: 5 * time.Second,
			replicaStaleLag:       1000,
			wantStale:             true,
		},
		{
			name:                  "replica zero lag is not stale",
			isReplica:             true,
			primarySeq:            100,
			lastSeq:               100,
			heartbeatAge:          100 * time.Millisecond,
			replicaStaleHeartbeat: 5 * time.Second,
			replicaStaleLag:       1000,
			wantStale:             false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &Server{
				isReplica:     tt.isReplica,
				primarySeq:    tt.primarySeq,
				lastHeartbeat: time.Now().Add(-tt.heartbeatAge),
				opts: Options{
					ReplicaStaleHeartbeat: tt.replicaStaleHeartbeat,
					ReplicaStaleLag:       tt.replicaStaleLag,
				},
			}
			s.lastSeq = atomic.Uint64{}
			s.lastSeq.Store(tt.lastSeq)

			got := s.isStaleness()
			if got != tt.wantStale {
				t.Errorf("isStaleness() = %v, want %v (seqLag=%d, heartbeatAge=%v)",
					got, tt.wantStale, tt.primarySeq-tt.lastSeq, tt.heartbeatAge)
			}
		})
	}
}
