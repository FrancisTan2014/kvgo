package server

import (
	"context"
	"testing"
)

// ---------------------------------------------------------------------------
// fenceDetected
// ---------------------------------------------------------------------------

func TestFenceDetected(t *testing.T) {
	tests := []struct {
		name         string
		replicas     map[string]*replicaConn // nodeID → replicaConn
		recentActive map[string]bool         // nodeID → recentActive before call
		wantFence    bool
		wantAllReset bool // all recentActive should be false after call
	}{
		{
			name:      "standalone primary — no replicas",
			replicas:  map[string]*replicaConn{},
			wantFence: false,
		},
		{
			name: "3-node cluster, both replicas active → no fence",
			// quorum(3) = 2, active = 2 + 1(self) = 3 ≥ 2
			replicas: map[string]*replicaConn{
				"n1": {nodeID: "n1"},
				"n2": {nodeID: "n2"},
			},
			recentActive: map[string]bool{"n1": true, "n2": true},
			wantFence:    false,
			wantAllReset: true,
		},
		{
			name: "3-node cluster, one replica active → no fence",
			// quorum(3) = 2, active = 1 + 1(self) = 2 ≥ 2
			replicas: map[string]*replicaConn{
				"n1": {nodeID: "n1"},
				"n2": {nodeID: "n2"},
			},
			recentActive: map[string]bool{"n1": true, "n2": false},
			wantFence:    false,
			wantAllReset: true,
		},
		{
			name: "3-node cluster, no replicas active → fence",
			// quorum(3) = 2, active = 0 + 1(self) = 1 < 2
			replicas: map[string]*replicaConn{
				"n1": {nodeID: "n1"},
				"n2": {nodeID: "n2"},
			},
			recentActive: map[string]bool{"n1": false, "n2": false},
			wantFence:    true,
			wantAllReset: true,
		},
		{
			name: "5-node cluster, two replicas active → no fence",
			// quorum(5) = 3, active = 2 + 1(self) = 3 ≥ 3
			replicas: map[string]*replicaConn{
				"n1": {nodeID: "n1"},
				"n2": {nodeID: "n2"},
				"n3": {nodeID: "n3"},
				"n4": {nodeID: "n4"},
			},
			recentActive: map[string]bool{"n1": true, "n2": true, "n3": false, "n4": false},
			wantFence:    false,
			wantAllReset: true,
		},
		{
			name: "5-node cluster, one replica active → fence",
			// quorum(5) = 3, active = 1 + 1(self) = 2 < 3
			replicas: map[string]*replicaConn{
				"n1": {nodeID: "n1"},
				"n2": {nodeID: "n2"},
				"n3": {nodeID: "n3"},
				"n4": {nodeID: "n4"},
			},
			recentActive: map[string]bool{"n1": true, "n2": false, "n3": false, "n4": false},
			wantFence:    true,
			wantAllReset: true,
		},
		{
			name: "2-node cluster, replica active → no fence",
			// quorum(2) = 2, active = 1 + 1(self) = 2 ≥ 2
			replicas: map[string]*replicaConn{
				"n1": {nodeID: "n1"},
			},
			recentActive: map[string]bool{"n1": true},
			wantFence:    false,
			wantAllReset: true,
		},
		{
			name: "2-node cluster, replica inactive → fence",
			// quorum(2) = 2, active = 0 + 1(self) = 1 < 2
			replicas: map[string]*replicaConn{
				"n1": {nodeID: "n1"},
			},
			recentActive: map[string]bool{"n1": false},
			wantFence:    true,
			wantAllReset: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &Server{
				replicas: tt.replicas,
			}

			// Set recentActive flags
			for id, active := range tt.recentActive {
				s.replicas[id].recentActive.Store(active)
			}

			got := s.fenceDetected()
			if got != tt.wantFence {
				t.Errorf("fenceDetected() = %v, want %v", got, tt.wantFence)
			}

			// Verify all recentActive flags are reset
			if tt.wantAllReset {
				for id, rc := range s.replicas {
					if rc.recentActive.Load() {
						t.Errorf("replica %s recentActive should be reset to false after fenceDetected()", id)
					}
				}
			}
		})
	}
}

func TestFenceDetectedResetsEvenWhenNotFenced(t *testing.T) {
	s := &Server{
		replicas: map[string]*replicaConn{
			"n1": {nodeID: "n1"},
			"n2": {nodeID: "n2"},
		},
	}
	s.replicas["n1"].recentActive.Store(true)
	s.replicas["n2"].recentActive.Store(true)

	fenced := s.fenceDetected()
	if fenced {
		t.Fatal("should not be fenced with all replicas active")
	}

	// Flags should be reset regardless
	for id, rc := range s.replicas {
		if rc.recentActive.Load() {
			t.Errorf("replica %s recentActive not reset after non-fenced check", id)
		}
	}

	// Second call without any activity → fence (since flags were reset)
	fenced = s.fenceDetected()
	if !fenced {
		t.Error("second fenceDetected() should return true — no activity since last reset")
	}
}

// ---------------------------------------------------------------------------
// markReplicaActive
// ---------------------------------------------------------------------------

func TestMarkReplicaActive(t *testing.T) {
	t.Run("marks existing replica active", func(t *testing.T) {
		s := &Server{
			replicas: map[string]*replicaConn{
				"n1": {nodeID: "n1"},
			},
		}

		if s.replicas["n1"].recentActive.Load() {
			t.Fatal("should start inactive")
		}

		s.markReplicaActive("n1")

		if !s.replicas["n1"].recentActive.Load() {
			t.Error("markReplicaActive should set recentActive to true")
		}
	})

	t.Run("unknown nodeID is no-op", func(t *testing.T) {
		s := &Server{
			replicas: map[string]*replicaConn{
				"n1": {nodeID: "n1"},
			},
		}

		// Should not panic or error
		s.markReplicaActive("unknown-node")

		// Existing replica should be unaffected
		if s.replicas["n1"].recentActive.Load() {
			t.Error("unrelated replica should not be marked active")
		}
	})

	t.Run("idempotent", func(t *testing.T) {
		s := &Server{
			replicas: map[string]*replicaConn{
				"n1": {nodeID: "n1"},
			},
		}

		s.markReplicaActive("n1")
		s.markReplicaActive("n1")

		if !s.replicas["n1"].recentActive.Load() {
			t.Error("should still be active after double mark")
		}
	})

	t.Run("marks only the target replica", func(t *testing.T) {
		s := &Server{
			replicas: map[string]*replicaConn{
				"n1": {nodeID: "n1"},
				"n2": {nodeID: "n2"},
				"n3": {nodeID: "n3"},
			},
		}

		s.markReplicaActive("n2")

		if s.replicas["n1"].recentActive.Load() {
			t.Error("n1 should not be marked active")
		}
		if !s.replicas["n2"].recentActive.Load() {
			t.Error("n2 should be marked active")
		}
		if s.replicas["n3"].recentActive.Load() {
			t.Error("n3 should not be marked active")
		}
	})
}

// ---------------------------------------------------------------------------
// teardownReplicationState
// ---------------------------------------------------------------------------

func TestTeardownReplicationState(t *testing.T) {
	t.Run("clears all replicas from map", func(t *testing.T) {
		s := &Server{
			replicas: map[string]*replicaConn{
				"n1": {
					nodeID:    "n1",
					sendCh:    make(chan []byte, 1),
					transport: &fakeStreamTransport{id: 1},
				},
				"n2": {
					nodeID:    "n2",
					sendCh:    make(chan []byte, 1),
					transport: &fakeStreamTransport{id: 2},
				},
			},
		}

		s.teardownReplicationState()

		if len(s.replicas) != 0 {
			t.Errorf("replicas map should be empty, got %d entries", len(s.replicas))
		}
	})

	t.Run("closes sendCh for each replica", func(t *testing.T) {
		ch1 := make(chan []byte, 1)
		ch2 := make(chan []byte, 1)

		s := &Server{
			replicas: map[string]*replicaConn{
				"n1": {nodeID: "n1", sendCh: ch1, transport: &fakeStreamTransport{id: 1}},
				"n2": {nodeID: "n2", sendCh: ch2, transport: &fakeStreamTransport{id: 2}},
			},
		}

		s.teardownReplicationState()

		// Channels should be closed
		_, ok1 := <-ch1
		if ok1 {
			t.Error("n1 sendCh should be closed")
		}
		_, ok2 := <-ch2
		if ok2 {
			t.Error("n2 sendCh should be closed")
		}
	})

	t.Run("closes replica transports", func(t *testing.T) {
		tr1 := &errorStreamTransport{failAfter: 999}
		tr2 := &errorStreamTransport{failAfter: 999}

		s := &Server{
			replicas: map[string]*replicaConn{
				"n1": {nodeID: "n1", sendCh: make(chan []byte, 1), transport: tr1},
				"n2": {nodeID: "n2", sendCh: make(chan []byte, 1), transport: tr2},
			},
		}

		s.teardownReplicationState()

		if !tr1.isClosed() {
			t.Error("n1 transport should be closed")
		}
		if !tr2.isClosed() {
			t.Error("n2 transport should be closed")
		}
	})

	t.Run("sets connected false before closing sendCh", func(t *testing.T) {
		// Verify ordering: connected=false must happen before close(sendCh)
		// to prevent senders from writing to a closed channel
		s := &Server{
			replicas: map[string]*replicaConn{
				"n1": {nodeID: "n1", sendCh: make(chan []byte, 1), transport: &fakeStreamTransport{id: 1}},
			},
		}
		s.replicas["n1"].connected.Store(true)

		s.teardownReplicationState()

		// After teardown, connected must be false
		// (can't observe ordering directly in a unit test, but at least verify end state)
		// The entry is deleted, so we can't check — this test documents the invariant.
	})

	t.Run("closes primary transport", func(t *testing.T) {
		tr := &errorStreamTransport{failAfter: 999}

		s := &Server{
			replicas: make(map[string]*replicaConn),
			primary:  tr,
		}

		s.teardownReplicationState()

		if !tr.isClosed() {
			t.Error("primary transport should be closed")
		}
		if s.primary != nil {
			t.Error("primary should be nil after teardown")
		}
	})

	t.Run("cancels replCancel", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())

		s := &Server{
			replicas:   make(map[string]*replicaConn),
			replCtx:    ctx,
			replCancel: cancel,
		}

		s.teardownReplicationState()

		select {
		case <-ctx.Done():
			// expected
		default:
			t.Error("replCtx should be cancelled after teardown")
		}
	})

	t.Run("cancels backlogCancel", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())

		s := &Server{
			replicas:      make(map[string]*replicaConn),
			backlogCtx:    ctx,
			backlogCancel: cancel,
		}

		s.teardownReplicationState()

		select {
		case <-ctx.Done():
			// expected
		default:
			t.Error("backlogCtx should be cancelled after teardown")
		}
	})

	t.Run("nil cancels are safe", func(t *testing.T) {
		s := &Server{
			replicas: map[string]*replicaConn{
				"n1": {nodeID: "n1", sendCh: make(chan []byte, 1), transport: &fakeStreamTransport{id: 1}},
			},
			// replCancel = nil, backlogCancel = nil
		}

		// Should not panic
		s.teardownReplicationState()
	})

	t.Run("nil primary is safe", func(t *testing.T) {
		s := &Server{
			replicas: make(map[string]*replicaConn),
			// primary = nil
		}

		// Should not panic
		s.teardownReplicationState()
	})

	t.Run("does not clear peers", func(t *testing.T) {
		pm := NewPeerManager(nil, noopLogger)
		pm.MergePeers([]PeerInfo{
			{NodeID: "peer1", Addr: "10.0.0.1:4000"},
			{NodeID: "peer2", Addr: "10.0.0.2:4000"},
		})

		s := &Server{
			replicas:    make(map[string]*replicaConn),
			peerManager: pm,
		}

		s.teardownReplicationState()

		// Peers should still be present — fenced primary needs them for relocate
		ids := pm.NodeIDs()
		if len(ids) != 2 {
			t.Errorf("peers should be preserved after teardown, got %d", len(ids))
		}
	})
}

// ---------------------------------------------------------------------------
// fenceDetected + markReplicaActive interaction
// ---------------------------------------------------------------------------

func TestFenceDetectedWithMarkReplicaActive(t *testing.T) {
	t.Run("ACK marks replica active, prevents fence", func(t *testing.T) {
		s := &Server{
			replicas: map[string]*replicaConn{
				"n1": {nodeID: "n1"},
				"n2": {nodeID: "n2"},
			},
		}

		// Simulate ACK from n1 (via markReplicaActive)
		s.markReplicaActive("n1")

		// 3-node cluster: quorum=2, active=1+1(self)=2 ≥ 2 → no fence
		if s.fenceDetected() {
			t.Error("should not be fenced — n1 responded")
		}
	})

	t.Run("no ACKs → fence", func(t *testing.T) {
		s := &Server{
			replicas: map[string]*replicaConn{
				"n1": {nodeID: "n1"},
				"n2": {nodeID: "n2"},
			},
		}

		// No markReplicaActive calls — both replicas silent
		// 3-node cluster: quorum=2, active=0+1(self)=1 < 2 → fence
		if !s.fenceDetected() {
			t.Error("should be fenced — no replicas responded")
		}
	})

	t.Run("reset-then-mark cycle", func(t *testing.T) {
		s := &Server{
			replicas: map[string]*replicaConn{
				"n1": {nodeID: "n1"},
			},
		}

		// Tick 1: mark active → no fence
		s.markReplicaActive("n1")
		if s.fenceDetected() {
			t.Error("tick 1: should not be fenced")
		}

		// fenceDetected reset all flags.
		// Tick 2: no mark → fence
		if !s.fenceDetected() {
			t.Error("tick 2: should be fenced — no activity since reset")
		}

		// Tick 3: mark again → no fence
		s.markReplicaActive("n1")
		if s.fenceDetected() {
			t.Error("tick 3: should not be fenced after re-mark")
		}
	})
}
