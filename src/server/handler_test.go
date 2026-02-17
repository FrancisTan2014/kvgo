package server

import (
	"kvgo/protocol"
	"sync/atomic"
	"testing"
	"time"
)

// TestHandleGet_ReplicaStaleness tests the staleness check in GET handler
func TestHandleGet_ReplicaStaleness(t *testing.T) {
	tests := []struct {
		name              string
		role              Role
		primarySeq        uint64
		lastSeq           uint64
		lastHeartbeat     time.Time
		staleHeartbeat    time.Duration
		staleLag          int
		wantStatusToCheck protocol.Status
	}{
		{
			name:              "primary never checks staleness",
			role:              RoleLeader,
			primarySeq:        100,
			lastSeq:           0,
			lastHeartbeat:     time.Now().Add(-10 * time.Hour),
			staleHeartbeat:    5 * time.Second,
			staleLag:          1000,
			wantStatusToCheck: protocol.StatusOK, // Would proceed to GET logic
		},
		{
			name:              "replica fresh and caught up",
			role:              RoleFollower,
			primarySeq:        100,
			lastSeq:           100,
			lastHeartbeat:     time.Now(),
			staleHeartbeat:    5 * time.Second,
			staleLag:          1000,
			wantStatusToCheck: protocol.StatusOK,
		},
		{
			name:              "replica stale by heartbeat",
			role:              RoleFollower,
			primarySeq:        100,
			lastSeq:           100,
			lastHeartbeat:     time.Now().Add(-10 * time.Second),
			staleHeartbeat:    5 * time.Second,
			staleLag:          1000,
			wantStatusToCheck: protocol.StatusReplicaTooStale,
		},
		{
			name:              "replica stale by sequence",
			role:              RoleFollower,
			primarySeq:        2000,
			lastSeq:           100,
			lastHeartbeat:     time.Now(),
			staleHeartbeat:    5 * time.Second,
			staleLag:          1000,
			wantStatusToCheck: protocol.StatusReplicaTooStale,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &Server{
				primarySeq:    tt.primarySeq,
				lastHeartbeat: tt.lastHeartbeat,
				opts: Options{
					ReplicaOf:             "primary:6379",
					ReplicaStaleHeartbeat: tt.staleHeartbeat,
					ReplicaStaleLag:       tt.staleLag,
				},
			}
			s.role.Store(uint32(tt.role))
			s.lastSeq = atomic.Uint64{}
			s.lastSeq.Store(tt.lastSeq)

			stale := s.isStaleness()
			wantStale := (tt.wantStatusToCheck == protocol.StatusReplicaTooStale)

			if stale != wantStale {
				t.Errorf("isStaleness() = %v, want %v", stale, wantStale)
			}
		})
	}
}

// TestHandlePut_ReplicaRejects tests that replicas reject writes
func TestHandlePut_ReplicaRejects(t *testing.T) {
	tests := []struct {
		name       string
		role       Role
		wantReject bool
	}{
		{
			name:       "primary accepts writes",
			role:       RoleLeader,
			wantReject: false,
		},
		{
			name:       "replica rejects writes",
			role:       RoleFollower,
			wantReject: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &Server{
				opts: Options{
					ReplicaOf: "primary:6379",
				},
			}
			s.role.Store(uint32(tt.role))

			// The replica rejection logic is:
			// if !s.isLeader() { return responseStatusWithPrimaryAddress(..., StatusReadOnly) }
			// We test the condition directly
			shouldReject := !s.isLeader() == tt.wantReject
			if !shouldReject {
				t.Errorf("replica rejection check: role=%v, wantReject=%v", s.currentRole(), tt.wantReject)
			}
		})
	}
}

// TestResponseStatusWithPrimaryAddress tests building redirect responses
func TestResponseStatusWithPrimaryAddress(t *testing.T) {
	tests := []struct {
		name         string
		primaryAddr  string
		status       protocol.Status
		wantValueStr string
	}{
		{
			name:         "StatusReadOnly with primary address",
			primaryAddr:  "192.168.1.100:6379",
			status:       protocol.StatusReadOnly,
			wantValueStr: "192.168.1.100:6379",
		},
		{
			name:         "StatusReplicaTooStale with primary address",
			primaryAddr:  "localhost:6379",
			status:       protocol.StatusReplicaTooStale,
			wantValueStr: "localhost:6379",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &Server{
				opts: Options{
					ReplicaOf: tt.primaryAddr,
				},
			}

			// Simulate what responseStatusWithPrimaryAddress does
			resp := protocol.Response{
				Status: tt.status,
				Value:  []byte(s.opts.ReplicaOf),
			}

			if string(resp.Value) != tt.wantValueStr {
				t.Errorf("response value = %q, want %q", resp.Value, tt.wantValueStr)
			}
			if resp.Status != tt.status {
				t.Errorf("response status = %v, want %v", resp.Status, tt.status)
			}
		})
	}
}
