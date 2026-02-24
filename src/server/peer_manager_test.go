package server

import (
	"context"
	"errors"
	"fmt"
	"kvgo/transport"
	"sort"
	"sync"
	"testing"
)

func TestPeerManager_MergePeers(t *testing.T) {
	t.Run("adds new peers lazily", func(t *testing.T) {
		pm := NewPeerManager(nil, noopLogger)
		pm.MergePeers([]PeerInfo{
			{NodeID: "n1", Addr: "a:1"},
			{NodeID: "n2", Addr: "b:2"},
			{NodeID: "n3", Addr: "c:3"},
		})

		ids := pm.NodeIDs()
		sort.Strings(ids)
		if len(ids) != 3 {
			t.Fatalf("len = %d, want 3", len(ids))
		}
		want := []string{"n1", "n2", "n3"}
		for i, id := range ids {
			if id != want[i] {
				t.Errorf("ids[%d] = %q, want %q", i, id, want[i])
			}
		}

		// No transports dialed yet
		snap := pm.Snapshot()
		if len(snap) != 0 {
			t.Errorf("snapshot len = %d, want 0 (lazy)", len(snap))
		}
	})

	t.Run("retains absent peers without closing transport", func(t *testing.T) {
		closed := false
		pm := NewPeerManager(func(addr string) (transport.RequestTransport, error) {
			return &mockRequestTransport{address: addr}, nil
		}, noopLogger)

		pm.MergePeers([]PeerInfo{
			{NodeID: "n1", Addr: "a:1"},
			{NodeID: "n2", Addr: "b:2"},
		})
		// Dial n1 to cache transport
		pm.GetTransport("n1")

		// Override close to track it
		pm.mu.Lock()
		pm.peers["n1"].transport = &closeTrackingTransport{closed: &closed}
		pm.mu.Unlock()

		// New topology drops n1 — but MergePeers retains it
		pm.MergePeers([]PeerInfo{{NodeID: "n2", Addr: "b:2"}})

		if closed {
			t.Error("retained peer transport should not be closed")
		}
		ids := pm.NodeIDs()
		sort.Strings(ids)
		if len(ids) != 2 || ids[0] != "n1" || ids[1] != "n2" {
			t.Errorf("ids = %v, want [n1 n2]", ids)
		}
	})

	t.Run("nil topology is a no-op", func(t *testing.T) {
		pm := NewPeerManager(nil, noopLogger)
		pm.MergePeers([]PeerInfo{
			{NodeID: "n1", Addr: "a:1"},
			{NodeID: "n2", Addr: "b:2"},
		})
		pm.MergePeers(nil)

		if len(pm.NodeIDs()) != 2 {
			t.Errorf("len = %d, want 2 after nil topology (no-op)", len(pm.NodeIDs()))
		}
	})

	t.Run("skips entries with empty nodeID or addr", func(t *testing.T) {
		pm := NewPeerManager(nil, noopLogger)
		pm.MergePeers([]PeerInfo{
			{NodeID: "n1", Addr: "a:1"},
			{NodeID: "", Addr: "b:2"},
			{NodeID: "n3", Addr: ""},
			{NodeID: "n4", Addr: "d:4"},
		})

		ids := pm.NodeIDs()
		sort.Strings(ids)
		if len(ids) != 2 {
			t.Fatalf("len = %d, want 2", len(ids))
		}
	})

	t.Run("retains existing connections on unchanged topology", func(t *testing.T) {
		pm := NewPeerManager(func(addr string) (transport.RequestTransport, error) {
			return &mockRequestTransport{address: addr}, nil
		}, noopLogger)

		pm.MergePeers([]PeerInfo{
			{NodeID: "n1", Addr: "a:1"},
			{NodeID: "n2", Addr: "b:2"},
		})
		t1, _ := pm.GetTransport("n1")

		// Same topology again
		pm.MergePeers([]PeerInfo{
			{NodeID: "n1", Addr: "a:1"},
			{NodeID: "n2", Addr: "b:2"},
		})
		t2, _ := pm.GetTransport("n1")

		if t1.RemoteAddr() != t2.RemoteAddr() {
			t.Error("transport replaced on unchanged topology")
		}
	})
}

func TestPeerManager_Addr(t *testing.T) {
	pm := NewPeerManager(nil, noopLogger)
	pm.MergePeers([]PeerInfo{{NodeID: "n1", Addr: "a:1"}, {NodeID: "n2", Addr: "b:2"}})

	addr, ok := pm.Addr("n1")
	if !ok || addr != "a:1" {
		t.Errorf("Addr(n1) = (%q, %v), want (a:1, true)", addr, ok)
	}

	_, ok = pm.Addr("unknown")
	if ok {
		t.Error("Addr(unknown) should return false")
	}
}

func TestPeerManager_AnyAddr(t *testing.T) {
	pm := NewPeerManager(nil, noopLogger)

	_, ok := pm.AnyAddr()
	if ok {
		t.Error("AnyAddr on empty should return false")
	}

	pm.MergePeers([]PeerInfo{{NodeID: "n1", Addr: "a:1"}})
	addr, ok := pm.AnyAddr()
	if !ok || addr != "a:1" {
		t.Errorf("AnyAddr = (%q, %v), want (a:1, true)", addr, ok)
	}
}

func TestPeerManager_PeerInfos(t *testing.T) {
	pm := NewPeerManager(nil, noopLogger)

	// Empty manager returns empty slice
	infos := pm.PeerInfos()
	if len(infos) != 0 {
		t.Errorf("PeerInfos on empty = %d, want 0", len(infos))
	}

	pm.MergePeers([]PeerInfo{{NodeID: "n1", Addr: "a:1"}, {NodeID: "n2", Addr: "b:2"}})
	infos = pm.PeerInfos()
	if len(infos) != 2 {
		t.Fatalf("PeerInfos = %d, want 2", len(infos))
	}

	// Check both peers are present (order not guaranteed)
	got := make(map[string]string)
	for _, pi := range infos {
		got[pi.NodeID] = pi.Addr
	}
	if got["n1"] != "a:1" || got["n2"] != "b:2" {
		t.Errorf("PeerInfos = %v, want n1->a:1 n2->b:2", got)
	}
}

func TestPeerManager_GetTransport(t *testing.T) {
	t.Run("lazy dial on first GetTransport", func(t *testing.T) {
		dialCount := 0
		pm := NewPeerManager(func(addr string) (transport.RequestTransport, error) {
			dialCount++
			return &mockRequestTransport{address: addr}, nil
		}, noopLogger)

		pm.MergePeers([]PeerInfo{{NodeID: "n1", Addr: "a:1"}})
		if dialCount != 0 {
			t.Fatalf("dial called on MergePeers, count = %d", dialCount)
		}

		t1, err := pm.GetTransport("n1")
		if err != nil {
			t.Fatalf("GetTransport: %v", err)
		}
		if dialCount != 1 {
			t.Errorf("dial count = %d, want 1", dialCount)
		}
		_ = t1 // wrapper identity differs per call; dial count verifies caching

		// Second GetTransport reuses cached inner transport (dial count stays 1)
		_, err = pm.GetTransport("n1")
		if err != nil {
			t.Fatalf("GetTransport: %v", err)
		}
		if dialCount != 1 {
			t.Errorf("dial count = %d, want 1 (cached)", dialCount)
		}
	})

	t.Run("unknown peer returns error", func(t *testing.T) {
		pm := NewPeerManager(nil, noopLogger)
		_, err := pm.GetTransport("unknown")
		if err == nil {
			t.Error("expected error for unknown peer")
		}
	})

	t.Run("dial failure propagates", func(t *testing.T) {
		pm := NewPeerManager(func(addr string) (transport.RequestTransport, error) {
			return nil, errors.New("connection refused")
		}, noopLogger)

		pm.MergePeers([]PeerInfo{{NodeID: "n1", Addr: "a:1"}})
		_, err := pm.GetTransport("n1")
		if err == nil {
			t.Error("expected dial error")
		}
	})
}

func TestPeerManager_Snapshot(t *testing.T) {
	pm := NewPeerManager(func(addr string) (transport.RequestTransport, error) {
		return &mockRequestTransport{address: addr}, nil
	}, noopLogger)

	pm.MergePeers([]PeerInfo{
		{NodeID: "n1", Addr: "a:1"},
		{NodeID: "n2", Addr: "b:2"},
		{NodeID: "n3", Addr: "c:3"},
	})

	// Only dial two
	pm.GetTransport("n1")
	pm.GetTransport("n3")

	snap := pm.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("snapshot len = %d, want 2", len(snap))
	}
	if snap["n1"] == nil || snap["n3"] == nil {
		t.Error("snapshot missing dialed peers")
	}
	if snap["n2"] != nil {
		t.Error("snapshot includes undialed peer")
	}

	// Snapshot is isolated from mutations
	delete(snap, "n1")
	snap2 := pm.Snapshot()
	if len(snap2) != 2 {
		t.Error("mutation leaked into PeerManager")
	}
}

func TestPeerManager_Close(t *testing.T) {
	var closedAddrs []string
	var mu sync.Mutex

	pm := NewPeerManager(func(addr string) (transport.RequestTransport, error) {
		return &closeTrackingTransport{
			address:    addr,
			closedAddr: &closedAddrs,
			mu:         &mu,
		}, nil
	}, noopLogger)

	pm.MergePeers([]PeerInfo{
		{NodeID: "n1", Addr: "a:1"},
		{NodeID: "n2", Addr: "b:2"},
	})
	pm.GetTransport("n1")
	pm.GetTransport("n2")

	pm.Close()

	sort.Strings(closedAddrs)
	if len(closedAddrs) != 2 {
		t.Fatalf("closed %d transports, want 2", len(closedAddrs))
	}
	if closedAddrs[0] != "a:1" || closedAddrs[1] != "b:2" {
		t.Errorf("closed = %v, want [a:1 b:2]", closedAddrs)
	}
	if len(pm.NodeIDs()) != 0 {
		t.Error("peers not cleared after Close")
	}
}

func TestPeerManager_ConcurrentAccess(t *testing.T) {
	pm := NewPeerManager(func(addr string) (transport.RequestTransport, error) {
		return &mockRequestTransport{address: addr}, nil
	}, noopLogger)

	var wg sync.WaitGroup
	n := 50

	// Concurrent MergePeers + Get + NodeIDs
	wg.Add(n * 3)
	for i := 0; i < n; i++ {
		nodeID := fmt.Sprintf("node%d", i)
		addr := fmt.Sprintf("peer%d:1234", i)
		go func() {
			defer wg.Done()
			pm.MergePeers([]PeerInfo{{NodeID: nodeID, Addr: addr}})
		}()
		go func() {
			defer wg.Done()
			pm.GetTransport(nodeID) // may fail — that's fine
		}()
		go func() {
			defer wg.Done()
			_ = pm.NodeIDs()
		}()
	}
	wg.Wait()
	// No panic = pass
}

func TestPeerManager_Get(t *testing.T) {
	pm := NewPeerManager(nil, noopLogger)
	pm.MergePeers([]PeerInfo{
		{NodeID: "n1", Addr: "a:1"},
		{NodeID: "n2", Addr: "b:2"},
	})

	t.Run("returns known peer", func(t *testing.T) {
		pi, ok := pm.Get("n1")
		if !ok {
			t.Fatal("expected ok")
		}
		if pi.NodeID != "n1" || pi.Addr != "a:1" {
			t.Errorf("Get(n1) = %+v, want {n1 a:1}", pi)
		}
	})

	t.Run("returns false for unknown peer", func(t *testing.T) {
		_, ok := pm.Get("unknown")
		if ok {
			t.Error("expected not ok for unknown peer")
		}
	})
}

// closeTrackingTransport tracks Close() calls for test assertions
type closeTrackingTransport struct {
	address    string
	closed     *bool
	closedAddr *[]string
	mu         *sync.Mutex
}

func (c *closeTrackingTransport) Request(ctx context.Context, payload []byte) ([]byte, error) {
	return nil, nil
}

func (c *closeTrackingTransport) Close() error {
	if c.closed != nil {
		*c.closed = true
	}
	if c.closedAddr != nil {
		c.mu.Lock()
		*c.closedAddr = append(*c.closedAddr, c.address)
		c.mu.Unlock()
	}
	return nil
}

func (c *closeTrackingTransport) RemoteAddr() string {
	return c.address
}

func TestPeerManager_ReconnectOnError(t *testing.T) {
	t.Run("redials after request error", func(t *testing.T) {
		dialCount := 0
		failOnce := true
		pm := NewPeerManager(func(addr string) (transport.RequestTransport, error) {
			dialCount++
			if failOnce {
				failOnce = false
				return &mockRequestTransport{address: addr, err: errors.New("conn reset")}, nil
			}
			return &mockRequestTransport{address: addr}, nil
		}, noopLogger)

		pm.MergePeers([]PeerInfo{{NodeID: "n1", Addr: "a:1"}})

		// First dial returns a transport that will fail on Request
		t1, err := pm.GetTransport("n1")
		if err != nil {
			t.Fatalf("GetTransport: %v", err)
		}
		if dialCount != 1 {
			t.Fatalf("dial count = %d, want 1", dialCount)
		}

		// Request fails — triggers invalidation
		_, err = t1.Request(context.Background(), nil)
		if err == nil {
			t.Fatal("expected error from broken transport")
		}

		// Next GetTransport should redial (dial count goes to 2)
		t2, err := pm.GetTransport("n1")
		if err != nil {
			t.Fatalf("GetTransport after error: %v", err)
		}
		if dialCount != 2 {
			t.Fatalf("dial count = %d, want 2 (should have redialed)", dialCount)
		}

		// New transport works
		_, err = t2.Request(context.Background(), nil)
		if err != nil {
			t.Fatalf("Request on new transport: %v", err)
		}
	})

	t.Run("successful request preserves cache", func(t *testing.T) {
		dialCount := 0
		pm := NewPeerManager(func(addr string) (transport.RequestTransport, error) {
			dialCount++
			return &mockRequestTransport{address: addr}, nil
		}, noopLogger)

		pm.MergePeers([]PeerInfo{{NodeID: "n1", Addr: "a:1"}})

		t1, _ := pm.GetTransport("n1")
		_, _ = t1.Request(context.Background(), nil) // success — no invalidation

		_, _ = pm.GetTransport("n1")
		if dialCount != 1 {
			t.Errorf("dial count = %d, want 1 (cache should survive successful request)", dialCount)
		}
	})
}
