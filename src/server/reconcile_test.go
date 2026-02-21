package server

import (
	"context"
	"kvgo/protocol"
	"kvgo/transport"
	"sort"
	"sync"
	"testing"
	"time"
)

// capturingTransport records payloads sent via Request for later inspection.
type capturingTransport struct {
	mu       sync.Mutex
	requests [][]byte
	resp     []byte
}

func (c *capturingTransport) Request(ctx context.Context, payload []byte) ([]byte, error) {
	c.mu.Lock()
	c.requests = append(c.requests, payload)
	c.mu.Unlock()
	if c.resp != nil {
		return c.resp, nil
	}
	resp, _ := protocol.EncodeResponse(protocol.Response{Status: protocol.StatusOK})
	return resp, nil
}

func (c *capturingTransport) Close() error       { return nil }
func (c *capturingTransport) RemoteAddr() string { return "capture:0" }

func (c *capturingTransport) payloads() [][]byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([][]byte, len(c.requests))
	copy(out, c.requests)
	return out
}

// ---------------------------------------------------------------------------
// reconcilePeers
// ---------------------------------------------------------------------------

func TestReconcilePeers(t *testing.T) {
	t.Run("sends REPLICAOF to peer not in replicas map", func(t *testing.T) {
		capture := &capturingTransport{}
		pm := NewPeerManager(func(addr string) (transport.RequestTransport, error) {
			return capture, nil
		}, noopLogger)
		pm.MergePeers([]PeerInfo{
			{NodeID: "orphan", Addr: "10.0.0.99:4000"},
		})

		s := &Server{
			nodeID:      "leader",
			peerManager: pm,
			replicas:    map[string]*replicaConn{},
			opts:        Options{Host: "10.0.0.1", Port: 4000},
		}
		s.role.Store(uint32(RoleLeader))

		s.reconcilePeers()

		// Give goroutine time to fire
		time.Sleep(50 * time.Millisecond)

		payloads := capture.payloads()
		if len(payloads) != 1 {
			t.Fatalf("want 1 request, got %d", len(payloads))
		}
		req, err := protocol.DecodeRequest(payloads[0])
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if req.Cmd != protocol.CmdReplicaOf {
			t.Errorf("cmd = %v, want CmdReplicaOf", req.Cmd)
		}
		if string(req.Value) != s.listenAddr() {
			t.Errorf("value = %q, want %q", req.Value, s.listenAddr())
		}
	})

	t.Run("skips self", func(t *testing.T) {
		capture := &capturingTransport{}
		pm := NewPeerManager(func(addr string) (transport.RequestTransport, error) {
			return capture, nil
		}, noopLogger)
		pm.MergePeers([]PeerInfo{
			{NodeID: "leader", Addr: "10.0.0.1:4000"},
		})

		s := &Server{
			nodeID:      "leader",
			peerManager: pm,
			replicas:    map[string]*replicaConn{},
			opts:        Options{Host: "10.0.0.1", Port: 4000},
		}
		s.role.Store(uint32(RoleLeader))

		s.reconcilePeers()
		time.Sleep(50 * time.Millisecond)

		if len(capture.payloads()) != 0 {
			t.Errorf("should not send REPLICAOF to self, got %d requests", len(capture.payloads()))
		}
	})

	t.Run("skips connected replicas", func(t *testing.T) {
		capture := &capturingTransport{}
		pm := NewPeerManager(func(addr string) (transport.RequestTransport, error) {
			return capture, nil
		}, noopLogger)
		pm.MergePeers([]PeerInfo{
			{NodeID: "r1", Addr: "10.0.0.2:4000"},
		})

		rc := &replicaConn{nodeID: "r1", listenAddr: "10.0.0.2:4000"}
		rc.connected.Store(true)

		s := &Server{
			nodeID:      "leader",
			peerManager: pm,
			replicas:    map[string]*replicaConn{"r1": rc},
			opts:        Options{Host: "10.0.0.1", Port: 4000},
		}
		s.role.Store(uint32(RoleLeader))

		s.reconcilePeers()
		time.Sleep(50 * time.Millisecond)

		if len(capture.payloads()) != 0 {
			t.Errorf("should not send to connected replica, got %d requests", len(capture.payloads()))
		}
	})

	t.Run("skips reconcile when all replicas disconnected", func(t *testing.T) {
		capture := &capturingTransport{}
		pm := NewPeerManager(func(addr string) (transport.RequestTransport, error) {
			return capture, nil
		}, noopLogger)
		pm.MergePeers([]PeerInfo{
			{NodeID: "r1", Addr: "10.0.0.2:4000"},
		})

		rc := &replicaConn{nodeID: "r1", listenAddr: "10.0.0.2:4000"}
		rc.connected.Store(false) // disconnected

		s := &Server{
			nodeID:      "leader",
			peerManager: pm,
			replicas:    map[string]*replicaConn{"r1": rc},
			opts:        Options{Host: "10.0.0.1", Port: 4000},
		}
		s.role.Store(uint32(RoleLeader))

		s.reconcilePeers()
		time.Sleep(50 * time.Millisecond)

		// Leader with all replicas disconnected is likely stale — skip reconcile
		// to allow fenceDetected to fire and step down.
		if len(capture.payloads()) != 0 {
			t.Fatalf("stale leader should not reconcile, got %d requests", len(capture.payloads()))
		}
	})

	t.Run("no-op when not leader", func(t *testing.T) {
		capture := &capturingTransport{}
		pm := NewPeerManager(func(addr string) (transport.RequestTransport, error) {
			return capture, nil
		}, noopLogger)
		pm.MergePeers([]PeerInfo{
			{NodeID: "orphan", Addr: "10.0.0.99:4000"},
		})

		s := &Server{
			nodeID:      "follower",
			peerManager: pm,
			replicas:    map[string]*replicaConn{},
			opts:        Options{Host: "10.0.0.1", Port: 4000},
		}
		s.role.Store(uint32(RoleFollower))

		s.reconcilePeers()
		time.Sleep(50 * time.Millisecond)

		if len(capture.payloads()) != 0 {
			t.Errorf("follower should not reconcile, got %d requests", len(capture.payloads()))
		}
	})

	t.Run("sends to multiple unreached peers", func(t *testing.T) {
		var mu sync.Mutex
		transports := make(map[string]*capturingTransport)
		pm := NewPeerManager(func(addr string) (transport.RequestTransport, error) {
			mu.Lock()
			defer mu.Unlock()
			ct := &capturingTransport{}
			transports[addr] = ct
			return ct, nil
		}, noopLogger)
		pm.MergePeers([]PeerInfo{
			{NodeID: "r1", Addr: "10.0.0.2:4000"},
			{NodeID: "orphan1", Addr: "10.0.0.3:4000"},
			{NodeID: "orphan2", Addr: "10.0.0.4:4000"},
		})

		rc := &replicaConn{nodeID: "r1", listenAddr: "10.0.0.2:4000"}
		rc.connected.Store(true)

		s := &Server{
			nodeID:      "leader",
			peerManager: pm,
			replicas:    map[string]*replicaConn{"r1": rc},
			opts:        Options{Host: "10.0.0.1", Port: 4000},
		}
		s.role.Store(uint32(RoleLeader))

		s.reconcilePeers()
		time.Sleep(100 * time.Millisecond)

		mu.Lock()
		defer mu.Unlock()

		// Should have dialed orphan1 and orphan2 but not r1
		var dialed []string
		for addr := range transports {
			dialed = append(dialed, addr)
		}
		sort.Strings(dialed)
		if len(dialed) != 2 {
			t.Fatalf("want 2 dials, got %d: %v", len(dialed), dialed)
		}
		if dialed[0] != "10.0.0.3:4000" || dialed[1] != "10.0.0.4:4000" {
			t.Errorf("dialed = %v, want [10.0.0.3:4000 10.0.0.4:4000]", dialed)
		}
	})

	t.Run("handles dial failure gracefully", func(t *testing.T) {
		pm := NewPeerManager(func(addr string) (transport.RequestTransport, error) {
			return nil, context.DeadlineExceeded
		}, noopLogger)
		pm.MergePeers([]PeerInfo{
			{NodeID: "unreachable", Addr: "10.0.0.99:4000"},
		})

		s := &Server{
			nodeID:      "leader",
			peerManager: pm,
			replicas:    map[string]*replicaConn{},
			opts:        Options{Host: "10.0.0.1", Port: 4000},
		}
		s.role.Store(uint32(RoleLeader))

		// Should not panic
		s.reconcilePeers()
		time.Sleep(50 * time.Millisecond)
	})

	t.Run("no peers — no action", func(t *testing.T) {
		pm := NewPeerManager(nil, noopLogger)

		s := &Server{
			nodeID:      "leader",
			peerManager: pm,
			replicas:    map[string]*replicaConn{},
			opts:        Options{Host: "10.0.0.1", Port: 4000},
		}
		s.role.Store(uint32(RoleLeader))

		// Should not panic
		s.reconcilePeers()
	})
}
