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
		role                  Role
		primarySeq            uint64
		lastSeq               uint64
		heartbeatAge          time.Duration
		replicaStaleHeartbeat time.Duration
		replicaStaleLag       int
		wantStale             bool
	}{
		{
			name:                  "primary never stale",
			role:                  RoleLeader,
			primarySeq:            100,
			lastSeq:               0,
			heartbeatAge:          10 * time.Hour,
			replicaStaleHeartbeat: 5 * time.Second,
			replicaStaleLag:       1000,
			wantStale:             false,
		},
		{
			name:                  "replica fresh, no lag",
			role:                  RoleFollower,
			primarySeq:            100,
			lastSeq:               100,
			heartbeatAge:          1 * time.Second,
			replicaStaleHeartbeat: 5 * time.Second,
			replicaStaleLag:       1000,
			wantStale:             false,
		},
		{
			name:                  "replica stale by heartbeat",
			role:                  RoleFollower,
			primarySeq:            100,
			lastSeq:               100,
			heartbeatAge:          10 * time.Second,
			replicaStaleHeartbeat: 5 * time.Second,
			replicaStaleLag:       1000,
			wantStale:             true,
		},
		{
			name:                  "replica stale by sequence lag",
			role:                  RoleFollower,
			primarySeq:            2000,
			lastSeq:               100,
			heartbeatAge:          1 * time.Second,
			replicaStaleHeartbeat: 5 * time.Second,
			replicaStaleLag:       1000,
			wantStale:             true,
		},
		{
			name:                  "replica stale by both",
			role:                  RoleFollower,
			primarySeq:            2000,
			lastSeq:               100,
			heartbeatAge:          10 * time.Second,
			replicaStaleHeartbeat: 5 * time.Second,
			replicaStaleLag:       1000,
			wantStale:             true,
		},
		{
			name:                  "replica at exact heartbeat threshold (not stale)",
			role:                  RoleFollower,
			primarySeq:            100,
			lastSeq:               100,
			heartbeatAge:          5 * time.Second,
			replicaStaleHeartbeat: 5 * time.Second,
			replicaStaleLag:       1000,
			wantStale:             false,
		},
		{
			name:                  "replica at exact sequence threshold (not stale)",
			role:                  RoleFollower,
			primarySeq:            1100,
			lastSeq:               100,
			heartbeatAge:          1 * time.Second,
			replicaStaleHeartbeat: 5 * time.Second,
			replicaStaleLag:       1000,
			wantStale:             false,
		},
		{
			name:                  "replica one over heartbeat threshold (stale)",
			role:                  RoleFollower,
			primarySeq:            100,
			lastSeq:               100,
			heartbeatAge:          5*time.Second + 1*time.Nanosecond,
			replicaStaleHeartbeat: 5 * time.Second,
			replicaStaleLag:       1000,
			wantStale:             true,
		},
		{
			name:                  "replica one over sequence threshold (stale)",
			role:                  RoleFollower,
			primarySeq:            1101,
			lastSeq:               100,
			heartbeatAge:          1 * time.Second,
			replicaStaleHeartbeat: 5 * time.Second,
			replicaStaleLag:       1000,
			wantStale:             true,
		},
		{
			name:                  "replica zero lag is not stale",
			role:                  RoleFollower,
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
				primarySeq:    tt.primarySeq,
				lastHeartbeat: time.Now().Add(-tt.heartbeatAge),
				opts: Options{
					ReplicaStaleHeartbeat: tt.replicaStaleHeartbeat,
					ReplicaStaleLag:       tt.replicaStaleLag,
				},
			}
			s.role.Store(uint32(tt.role))
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
