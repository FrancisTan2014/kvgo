package server

import (
	"context"
	"fmt"
	"kvgo/transport"
	"log/slog"
	"sync"
	"time"
)

// DialFunc dials a peer and returns a RequestTransport.
// Injected to keep PeerManager testable without real connections.
type DialFunc func(addr string) (transport.RequestTransport, error)

// PeerInfo carries the identity and network address of a peer node.
type PeerInfo struct {
	NodeID string
	Addr   string
}

type PeerManager struct {
	mu    sync.RWMutex
	peers map[string]*peerEntry // keyed by nodeID

	dial   DialFunc
	logger *slog.Logger
}

type peerEntry struct {
	addr  string
	inner transport.RequestTransport // raw transport (nil until first dial)
	wrap  transport.RequestTransport // reconnect wrapper around inner (nil until first dial)
}

func NewPeerManager(dial DialFunc, logger *slog.Logger) *PeerManager {
	return &PeerManager{
		peers:  make(map[string]*peerEntry),
		dial:   dial,
		logger: logger,
	}
}

// Run starts the peer health loop. It periodically dials unconnected peers and
// probes connected ones via probe(). Failed probes trigger auto-invalidation
// (handled by reconnectTransport), so the next tick re-dials. Exits when ctx
// is canceled.
func (p *PeerManager) Run(ctx context.Context, interval time.Duration, probe func(ctx context.Context, t transport.RequestTransport) error) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.healthCheck(ctx, probe)
		}
	}
}

func (p *PeerManager) healthCheck(ctx context.Context, probe func(ctx context.Context, t transport.RequestTransport) error) {
	p.mu.RLock()
	ids := make([]string, 0, len(p.peers))
	for id := range p.peers {
		ids = append(ids, id)
	}
	p.mu.RUnlock()

	for _, id := range ids {
		t, err := p.GetTransport(id)
		if err != nil {
			p.logger.Debug("peer health: dial failed", "node_id", id, "error", err)
			continue
		}
		if err := probe(ctx, t); err != nil {
			p.logger.Warn("peer health: probe failed", "node_id", id, "error", err)
			// reconnectTransport.Request already invalidated the connection
		}
	}
}

// MergePeers upserts peers into the known topology. New peers are added lazily
// (no dial until Get). Existing peers with changed addresses are re-dialed.
// Peers absent from the incoming list are retained — historical nodes are never
// removed by a topology update. A nil or empty topology is a no-op.
func (p *PeerManager) MergePeers(topology []PeerInfo) {
	if len(topology) == 0 {
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	for _, pi := range topology {
		if pi.NodeID == "" || pi.Addr == "" {
			p.logger.Warn("peer manager: skipping peer with empty nodeID or addr")
			continue
		}
		if existing, exists := p.peers[pi.NodeID]; exists {
			if existing.addr != pi.Addr {
				// Address changed — close old transport so next Get re-dials
				if existing.inner != nil {
					_ = existing.inner.Close()
				}
				existing.addr = pi.Addr
				existing.inner = nil
				existing.wrap = nil
				p.logger.Info("peer manager: peer address updated", "node_id", pi.NodeID, "addr", pi.Addr)
			}
			continue
		}
		p.logger.Info("peer manager: discovered peer", "node_id", pi.NodeID, "addr", pi.Addr)
		p.peers[pi.NodeID] = &peerEntry{addr: pi.Addr}
	}
}

// Get returns the PeerInfo for the given nodeID, or false if unknown.
func (p *PeerManager) Get(nodeID string) (PeerInfo, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	entry, exists := p.peers[nodeID]
	if !exists {
		return PeerInfo{}, false
	}
	return PeerInfo{NodeID: nodeID, Addr: entry.addr}, true
}

// GetTransport returns a RequestTransport for the given nodeID, dialing lazily on first use.
// The returned transport auto-invalidates on error: if Request() fails, the cached
// connection is cleared so the next GetTransport call redials transparently.
func (p *PeerManager) GetTransport(nodeID string) (transport.RequestTransport, error) {
	p.mu.RLock()
	entry, exists := p.peers[nodeID]
	if !exists {
		p.mu.RUnlock()
		return nil, fmt.Errorf("peer manager: unknown peer %s", nodeID)
	}
	if entry.wrap != nil {
		t := entry.wrap
		p.mu.RUnlock()
		return t, nil
	}
	p.mu.RUnlock()

	// Upgrade to write lock to dial
	p.mu.Lock()
	defer p.mu.Unlock()

	// Double-check after re-acquiring lock
	entry, exists = p.peers[nodeID]
	if !exists {
		return nil, fmt.Errorf("peer manager: peer removed during dial %s", nodeID)
	}
	if entry.wrap != nil {
		return entry.wrap, nil
	}

	t, err := p.dial(entry.addr)
	if err != nil {
		return nil, fmt.Errorf("peer manager: dial %s (%s): %w", nodeID, entry.addr, err)
	}
	entry.inner = t
	entry.wrap = &reconnectTransport{inner: t, pm: p, nodeID: nodeID}
	return entry.wrap, nil
}

// NodeIDs returns the nodeIDs of all known peers (connected or not).
func (p *PeerManager) NodeIDs() []string {
	p.mu.RLock()
	defer p.mu.RUnlock()

	out := make([]string, 0, len(p.peers))
	for nodeID := range p.peers {
		out = append(out, nodeID)
	}
	return out
}

// PeerInfos returns a snapshot of all known peers (connected or not).
func (p *PeerManager) PeerInfos() []PeerInfo {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]PeerInfo, 0, len(p.peers))
	for nodeID, entry := range p.peers {
		out = append(out, PeerInfo{NodeID: nodeID, Addr: entry.addr})
	}
	return out
}

// Close closes all peer transports and clears the peer map.
func (p *PeerManager) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()

	for nodeID, entry := range p.peers {
		if entry.inner != nil {
			_ = entry.inner.Close()
		}
		delete(p.peers, nodeID)
	}
}

// invalidate clears the cached transport for a peer so the next GetTransport redials.
// Only clears if the cached inner transport matches the one that failed (avoids racing with a fresh dial).
func (p *PeerManager) invalidate(nodeID string, failed transport.RequestTransport) {
	p.mu.Lock()
	defer p.mu.Unlock()

	entry, exists := p.peers[nodeID]
	if !exists {
		return
	}
	if entry.inner == failed {
		_ = entry.inner.Close()
		entry.inner = nil
		entry.wrap = nil
	}
}

// reconnectTransport wraps a raw RequestTransport and invalidates the
// PeerManager cache on error so the next GetTransport call redials.
type reconnectTransport struct {
	inner  transport.RequestTransport
	pm     *PeerManager
	nodeID string
}

func (r *reconnectTransport) Request(ctx context.Context, payload []byte) ([]byte, error) {
	resp, err := r.inner.Request(ctx, payload)
	if err != nil {
		r.pm.invalidate(r.nodeID, r.inner)
	}
	return resp, err
}

func (r *reconnectTransport) Close() error { return r.inner.Close() }

func (r *reconnectTransport) RemoteAddr() string { return r.inner.RemoteAddr() }
