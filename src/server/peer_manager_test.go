package server

import (
	"errors"
	"fmt"
	"kvgo/transport"
	"sort"
	"sync"
	"testing"
	"time"
)

func TestPeerManager_SavePeers(t *testing.T) {
	t.Run("adds new peers lazily", func(t *testing.T) {
		pm := NewPeerManager(nil, noopLogger)
		pm.SavePeers([]string{"a:1", "b:2", "c:3"})

		addrs := pm.Addrs()
		sort.Strings(addrs)
		if len(addrs) != 3 {
			t.Fatalf("len = %d, want 3", len(addrs))
		}
		want := []string{"a:1", "b:2", "c:3"}
		for i, a := range addrs {
			if a != want[i] {
				t.Errorf("addrs[%d] = %q, want %q", i, a, want[i])
			}
		}

		// No transports dialed yet
		snap := pm.Snapshot()
		if len(snap) != 0 {
			t.Errorf("snapshot len = %d, want 0 (lazy)", len(snap))
		}
	})

	t.Run("removes absent peers and closes transport", func(t *testing.T) {
		closed := false
		pm := NewPeerManager(func(addr string) (transport.RequestTransport, error) {
			return &mockRequestTransport{address: addr}, nil
		}, noopLogger)

		pm.SavePeers([]string{"a:1", "b:2"})
		// Dial a:1 to cache transport
		pm.Get("a:1")

		// Override close to track it
		pm.mu.Lock()
		pm.peers["a:1"].transport = &closeTrackingTransport{closed: &closed}
		pm.mu.Unlock()

		// New topology drops a:1
		pm.SavePeers([]string{"b:2"})

		if !closed {
			t.Error("removed peer transport not closed")
		}
		addrs := pm.Addrs()
		if len(addrs) != 1 || addrs[0] != "b:2" {
			t.Errorf("addrs = %v, want [b:2]", addrs)
		}
	})

	t.Run("nil topology clears all peers", func(t *testing.T) {
		pm := NewPeerManager(nil, noopLogger)
		pm.SavePeers([]string{"a:1", "b:2"})
		pm.SavePeers(nil)

		if len(pm.Addrs()) != 0 {
			t.Errorf("len = %d, want 0 after nil topology", len(pm.Addrs()))
		}
	})

	t.Run("skips empty addresses", func(t *testing.T) {
		pm := NewPeerManager(nil, noopLogger)
		pm.SavePeers([]string{"a:1", "", "b:2", ""})

		addrs := pm.Addrs()
		sort.Strings(addrs)
		if len(addrs) != 2 {
			t.Fatalf("len = %d, want 2", len(addrs))
		}
	})

	t.Run("retains existing connections on unchanged topology", func(t *testing.T) {
		pm := NewPeerManager(func(addr string) (transport.RequestTransport, error) {
			return &mockRequestTransport{address: addr}, nil
		}, noopLogger)

		pm.SavePeers([]string{"a:1", "b:2"})
		t1, _ := pm.Get("a:1")

		// Same topology again
		pm.SavePeers([]string{"a:1", "b:2"})
		t2, _ := pm.Get("a:1")

		if t1 != t2 {
			t.Error("transport replaced on unchanged topology")
		}
	})
}

func TestPeerManager_Get(t *testing.T) {
	t.Run("lazy dial on first Get", func(t *testing.T) {
		dialCount := 0
		pm := NewPeerManager(func(addr string) (transport.RequestTransport, error) {
			dialCount++
			return &mockRequestTransport{address: addr}, nil
		}, noopLogger)

		pm.SavePeers([]string{"a:1"})
		if dialCount != 0 {
			t.Fatalf("dial called on SavePeers, count = %d", dialCount)
		}

		t1, err := pm.Get("a:1")
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if dialCount != 1 {
			t.Errorf("dial count = %d, want 1", dialCount)
		}

		// Second Get reuses cached transport
		t2, err := pm.Get("a:1")
		if err != nil {
			t.Fatalf("Get: %v", err)
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
		_, err := pm.Get("unknown:1")
		if err == nil {
			t.Error("expected error for unknown peer")
		}
	})

	t.Run("dial failure propagates", func(t *testing.T) {
		pm := NewPeerManager(func(addr string) (transport.RequestTransport, error) {
			return nil, errors.New("connection refused")
		}, noopLogger)

		pm.SavePeers([]string{"a:1"})
		_, err := pm.Get("a:1")
		if err == nil {
			t.Error("expected dial error")
		}
	})
}

func TestPeerManager_Snapshot(t *testing.T) {
	pm := NewPeerManager(func(addr string) (transport.RequestTransport, error) {
		return &mockRequestTransport{address: addr}, nil
	}, noopLogger)

	pm.SavePeers([]string{"a:1", "b:2", "c:3"})

	// Only dial two
	pm.Get("a:1")
	pm.Get("c:3")

	snap := pm.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("snapshot len = %d, want 2", len(snap))
	}
	if snap["a:1"] == nil || snap["c:3"] == nil {
		t.Error("snapshot missing dialed peers")
	}
	if snap["b:2"] != nil {
		t.Error("snapshot includes undialed peer")
	}

	// Snapshot is isolated from mutations
	delete(snap, "a:1")
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

	pm.SavePeers([]string{"a:1", "b:2"})
	pm.Get("a:1")
	pm.Get("b:2")

	pm.Close()

	sort.Strings(closedAddrs)
	if len(closedAddrs) != 2 {
		t.Fatalf("closed %d transports, want 2", len(closedAddrs))
	}
	if closedAddrs[0] != "a:1" || closedAddrs[1] != "b:2" {
		t.Errorf("closed = %v, want [a:1 b:2]", closedAddrs)
	}
	if len(pm.Addrs()) != 0 {
		t.Error("peers not cleared after Close")
	}
}

func TestPeerManager_ConcurrentAccess(t *testing.T) {
	pm := NewPeerManager(func(addr string) (transport.RequestTransport, error) {
		return &mockRequestTransport{address: addr}, nil
	}, noopLogger)

	var wg sync.WaitGroup
	n := 50

	// Concurrent SavePeers + Get + Addrs
	wg.Add(n * 3)
	for i := 0; i < n; i++ {
		addr := fmt.Sprintf("peer%d:1234", i)
		go func() {
			defer wg.Done()
			pm.SavePeers([]string{addr})
		}()
		go func() {
			defer wg.Done()
			pm.Get(addr) // may fail — that's fine
		}()
		go func() {
			defer wg.Done()
			_ = pm.Addrs()
		}()
	}
	wg.Wait()
	// No panic = pass
}

// closeTrackingTransport tracks Close() calls for test assertions
type closeTrackingTransport struct {
	address    string
	closed     *bool
	closedAddr *[]string
	mu         *sync.Mutex
}

func (c *closeTrackingTransport) Request(payload []byte, timeout time.Duration) ([]byte, error) {
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
