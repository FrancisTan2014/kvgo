package server

import (
	"context"
	"errors"
	"fmt"
	"kvgo/transport"
	"sort"
	"sync"
	"testing"
	"time"
)

func TestPeerManager_MergePeers(t *testing.T) {
	t.Run("adds new peers lazily", func(t *testing.T) {
		dialCount := 0
		pm := NewPeerManager(func(addr string) (transport.RequestTransport, error) {
			dialCount++
			return &mockRequestTransport{address: addr}, nil
		}, noopLogger)
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
		if dialCount != 0 {
			t.Errorf("dial count = %d, want 0 (lazy)", dialCount)
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

		// Override inner to track Close
		pm.mu.Lock()
		pm.peers["n1"].inner = &closeTrackingTransport{closed: &closed}
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

		if t1 != t2 {
			t.Error("transport replaced on unchanged topology")
		}
	})

	t.Run("evicts stale nodeID when new nodeID claims same address", func(t *testing.T) {
		closed := false
		pm := NewPeerManager(func(addr string) (transport.RequestTransport, error) {
			return &mockRequestTransport{address: addr}, nil
		}, noopLogger)

		pm.MergePeers([]PeerInfo{
			{NodeID: "old-id", Addr: "127.0.0.1:4001"},
			{NodeID: "n2", Addr: "127.0.0.1:4002"},
		})
		// Dial old-id to cache transport, then track Close
		pm.GetTransport("old-id")
		pm.mu.Lock()
		pm.peers["old-id"].inner = &closeTrackingTransport{closed: &closed}
		pm.mu.Unlock()

		// A replica restarted with a new nodeID at the same address
		pm.MergePeers([]PeerInfo{
			{NodeID: "new-id", Addr: "127.0.0.1:4001"},
		})

		ids := pm.NodeIDs()
		sort.Strings(ids)
		if len(ids) != 2 {
			t.Fatalf("len = %d, want 2", len(ids))
		}
		// old-id should be gone, new-id should exist
		_, oldExists := pm.Get("old-id")
		if oldExists {
			t.Error("stale peer old-id should have been evicted")
		}
		pi, newExists := pm.Get("new-id")
		if !newExists || pi.Addr != "127.0.0.1:4001" {
			t.Errorf("new-id not found or wrong addr: %+v", pi)
		}
		if !closed {
			t.Error("stale peer transport should have been closed")
		}
	})
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

		// Second GetTransport reuses cached transport
		t2, err := pm.GetTransport("n1")
		if err != nil {
			t.Fatalf("GetTransport: %v", err)
		}
		if t1 != t2 {
			t.Error("second Get returned different transport")
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

func TestPeerManager_ReconnectOnError(t *testing.T) {
	dialCount := 0
	pm := NewPeerManager(func(addr string) (transport.RequestTransport, error) {
		dialCount++
		return &mockRequestTransport{address: addr}, nil
	}, noopLogger)

	pm.MergePeers([]PeerInfo{{NodeID: "n1", Addr: "a:1"}})

	t1, _ := pm.GetTransport("n1")
	if dialCount != 1 {
		t.Fatalf("dial count = %d, want 1", dialCount)
	}

	// Wrapper is stable across calls
	t2, _ := pm.GetTransport("n1")
	if t1 != t2 {
		t.Error("second GetTransport returned different wrapper")
	}
	if dialCount != 1 {
		t.Errorf("dial count = %d, want 1 (cached)", dialCount)
	}

	// Trigger error → invalidate → next GetTransport redials
	pm.mu.Lock()
	entry := pm.peers["n1"]
	entry.inner = &mockRequestTransport{err: errors.New("broken"), address: "a:1"}
	entry.wrap = &reconnectTransport{inner: entry.inner, pm: pm, nodeID: "n1"}
	pm.mu.Unlock()

	// Request through the wrapper triggers invalidation
	_, err := entry.wrap.Request(context.Background(), nil)
	if err == nil {
		t.Fatal("expected error from broken transport")
	}

	// Now GetTransport should redial
	t3, _ := pm.GetTransport("n1")
	if dialCount != 2 {
		t.Errorf("dial count = %d, want 2 (redialed)", dialCount)
	}
	if t3 == t1 {
		t.Error("expected new transport after invalidation")
	}
}

func TestPeerManager_Run(t *testing.T) {
	t.Run("exits on context cancel", func(t *testing.T) {
		pm := NewPeerManager(nil, noopLogger)
		ctx, cancel := context.WithCancel(context.Background())

		done := make(chan struct{})
		go func() {
			pm.Run(ctx, 50*time.Millisecond, func(ctx context.Context, t transport.RequestTransport) error {
				return nil
			})
			close(done)
		}()

		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("Run did not exit after context cancel")
		}
	})

	t.Run("probes connected peers", func(t *testing.T) {
		var probeMu sync.Mutex
		probed := make(map[string]int)

		pm := NewPeerManager(func(addr string) (transport.RequestTransport, error) {
			return &mockRequestTransport{address: addr}, nil
		}, noopLogger)
		pm.MergePeers([]PeerInfo{
			{NodeID: "n1", Addr: "a:1"},
			{NodeID: "n2", Addr: "b:2"},
		})

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			pm.Run(ctx, 50*time.Millisecond, func(ctx context.Context, t transport.RequestTransport) error {
				probeMu.Lock()
				probed[t.RemoteAddr()]++
				probeMu.Unlock()
				return nil
			})
			close(done)
		}()

		// Wait long enough for at least 2 ticks
		time.Sleep(150 * time.Millisecond)
		cancel()
		<-done

		probeMu.Lock()
		defer probeMu.Unlock()
		if probed["a:1"] == 0 || probed["b:2"] == 0 {
			t.Errorf("expected both peers probed, got %v", probed)
		}
	})

	t.Run("redials after probe failure", func(t *testing.T) {
		dialCount := 0
		failFirst := true
		pm := NewPeerManager(func(addr string) (transport.RequestTransport, error) {
			dialCount++
			if failFirst {
				failFirst = false
				return &mockRequestTransport{address: addr, err: errors.New("broken pipe")}, nil
			}
			return &mockRequestTransport{address: addr}, nil
		}, noopLogger)
		pm.MergePeers([]PeerInfo{{NodeID: "n1", Addr: "a:1"}})

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			pm.Run(ctx, 50*time.Millisecond, func(ctx context.Context, t transport.RequestTransport) error {
				_, err := t.Request(ctx, nil)
				return err
			})
			close(done)
		}()

		// Wait for at least 3 ticks: tick1 (dial+probe fail→invalidate), tick2 (redial+ok)
		time.Sleep(200 * time.Millisecond)
		cancel()
		<-done

		if dialCount < 2 {
			t.Errorf("dial count = %d, want >= 2 (redial after probe failure)", dialCount)
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
