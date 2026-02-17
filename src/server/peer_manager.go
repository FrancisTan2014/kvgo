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

type PeerManager struct {
	mu    sync.RWMutex
	peers map[string]*peerEntry

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

// SavePeers replaces the known topology. New addresses are added lazily
// (no dial until Get). Addresses absent from topology are removed and
// their transports closed. A nil or empty topology clears all peers.
func (p *PeerManager) SavePeers(topology []string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	incoming := make(map[string]struct{}, len(topology))
	for _, addr := range topology {
		if addr == "" {
			p.logger.Warn("peer manager: skipping empty address")
			continue
		}
		incoming[addr] = struct{}{}
	}

	// Remove peers no longer in topology
	for addr, entry := range p.peers {
		if _, keep := incoming[addr]; keep {
			continue
		}
		if entry.transport != nil {
			_ = entry.transport.Close()
		}
		p.logger.Info("peer manager: removed peer", "addr", addr)
		delete(p.peers, addr)
	}

	// Add new peers (lazy — no transport yet)
	for addr := range incoming {
		if _, exists := p.peers[addr]; exists {
			continue
		}
		p.logger.Info("peer manager: discovered peer", "addr", addr)
		p.peers[addr] = &peerEntry{addr: addr}
	}
}

// Get returns a RequestTransport for addr, dialing lazily on first use.
// Returns an error if addr is unknown or dial fails.
func (p *PeerManager) Get(addr string) (transport.RequestTransport, error) {
	p.mu.RLock()
	entry, exists := p.peers[addr]
	if !exists {
		p.mu.RUnlock()
		return nil, fmt.Errorf("peer manager: unknown peer %s", addr)
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
	entry, exists = p.peers[addr]
	if !exists {
		return nil, fmt.Errorf("peer manager: peer removed during dial %s", addr)
	}
	if entry.transport != nil {
		return entry.transport, nil
	}

	t, err := p.dial(addr)
	if err != nil {
		return nil, fmt.Errorf("peer manager: dial %s: %w", addr, err)
	}
	entry.transport = t
	return t, nil
}

// Snapshot returns a copy of all currently-connected transports.
// Peers that haven't been dialed yet are excluded.
func (p *PeerManager) Snapshot() map[string]transport.RequestTransport {
	p.mu.RLock()
	defer p.mu.RUnlock()

	out := make(map[string]transport.RequestTransport)
	for addr, entry := range p.peers {
		if entry.transport != nil {
			out[addr] = entry.transport
		}
	}
	return out
}

// Addrs returns addresses of all known peers (connected or not).
func (p *PeerManager) Addrs() []string {
	p.mu.RLock()
	defer p.mu.RUnlock()

	out := make([]string, 0, len(p.peers))
	for addr := range p.peers {
		out = append(out, addr)
	}
	return out
}

// Close closes all peer transports and clears the peer map.
func (p *PeerManager) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()

	for addr, entry := range p.peers {
		if entry.transport != nil {
			_ = entry.transport.Close()
		}
		delete(p.peers, addr)
	}
}
