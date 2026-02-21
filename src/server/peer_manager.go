package server

import (
	"fmt"
	"kvgo/transport"
	"log/slog"
	"sync"
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
	addr      string
	transport transport.RequestTransport // nil until first use
}

func NewPeerManager(dial DialFunc, logger *slog.Logger) *PeerManager {
	return &PeerManager{
		peers:  make(map[string]*peerEntry),
		dial:   dial,
		logger: logger,
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
				if existing.transport != nil {
					_ = existing.transport.Close()
				}
				existing.addr = pi.Addr
				existing.transport = nil
				p.logger.Info("peer manager: peer address updated", "node_id", pi.NodeID, "addr", pi.Addr)
			}
			continue
		}
		p.logger.Info("peer manager: discovered peer", "node_id", pi.NodeID, "addr", pi.Addr)
		p.peers[pi.NodeID] = &peerEntry{addr: pi.Addr}
	}
}

// Get returns a RequestTransport for the given nodeID, dialing lazily on first use.
// Returns an error if nodeID is unknown or dial fails.
func (p *PeerManager) Get(nodeID string) (transport.RequestTransport, error) {
	p.mu.RLock()
	entry, exists := p.peers[nodeID]
	if !exists {
		p.mu.RUnlock()
		return nil, fmt.Errorf("peer manager: unknown peer %s", nodeID)
	}
	if entry.transport != nil {
		t := entry.transport
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
	if entry.transport != nil {
		return entry.transport, nil
	}

	t, err := p.dial(entry.addr)
	if err != nil {
		return nil, fmt.Errorf("peer manager: dial %s (%s): %w", nodeID, entry.addr, err)
	}
	entry.transport = t
	return t, nil
}

// Snapshot returns a copy of all currently-connected transports, keyed by nodeID.
// Peers that haven't been dialed yet are excluded.
func (p *PeerManager) Snapshot() map[string]transport.RequestTransport {
	p.mu.RLock()
	defer p.mu.RUnlock()

	out := make(map[string]transport.RequestTransport)
	for nodeID, entry := range p.peers {
		if entry.transport != nil {
			out[nodeID] = entry.transport
		}
	}
	return out
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

// Addr returns the address of a known peer, or ("", false) if unknown.
func (p *PeerManager) Addr(nodeID string) (string, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	entry, exists := p.peers[nodeID]
	if !exists {
		return "", false
	}
	return entry.addr, true
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

// AnyAddr returns the address of an arbitrary known peer, or ("", false) if none.
func (p *PeerManager) AnyAddr() (string, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, entry := range p.peers {
		return entry.addr, true
	}
	return "", false
}

// Close closes all peer transports and clears the peer map.
func (p *PeerManager) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()

	for nodeID, entry := range p.peers {
		if entry.transport != nil {
			_ = entry.transport.Close()
		}
		delete(p.peers, nodeID)
	}
}
