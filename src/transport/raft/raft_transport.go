package raft

import (
	"context"
	"errors"
	"kvgo/raftpb"
	"kvgo/transport"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/protobuf/proto"
)

const messageBufferSize = 64

type Peer interface {
	Send(msgs []*raftpb.Message)
	Stop()
}

// PeerInfo is a simple ID+address pair for configuring peers.
type PeerInfo struct {
	ID   uint64
	Addr string
}

// resettable is an optional interface for peers that can be woken from backoff.
type resettable interface {
	Reset()
}

// Raft is the consumer of inbound raft messages delivered by the transport.
type Raft interface {
	Process(ctx context.Context, m *raftpb.Message) error
}

type RaftTransportConfig struct {
	ListenAddr   string
	Listener     net.Listener // optional: pre-bound listener, skips net.Listen if set
	WriteTimeout time.Duration
}

var ErrTransportStarted = errors.New("transport already started")
var ErrTransportStopped = errors.New("transport already stopped")

type RaftTransport struct {
	cfg     RaftTransportConfig
	raft    Raft
	peers   map[uint64]Peer
	peersMu sync.RWMutex

	ln net.Listener

	mu    sync.Mutex
	conns map[net.Conn]struct{}

	started atomic.Bool
	stopped atomic.Bool
	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup

	lg *slog.Logger
}

func NewRaftTransport(cfg RaftTransportConfig, raft Raft, lg *slog.Logger) (*RaftTransport, error) {
	if raft == nil {
		return nil, errors.New("raft is required")
	}
	if cfg.ListenAddr == "" && cfg.Listener == nil {
		return nil, errors.New("listen address or listener is required")
	}
	if lg == nil {
		return nil, errors.New("logger is required")
	}

	ctx, cancel := context.WithCancel(context.Background())

	return &RaftTransport{
		cfg:    cfg,
		raft:   raft,
		peers:  make(map[uint64]Peer),
		conns:  make(map[net.Conn]struct{}),
		ctx:    ctx,
		cancel: cancel,
		lg:     lg,
	}, nil
}

func (r *RaftTransport) Send(msgs []*raftpb.Message) {
	if r.stopped.Load() {
		return
	}
	if len(msgs) == 0 {
		r.lg.Warn("send called with no messages")
		return
	}

	groups := make(map[uint64][]*raftpb.Message)
	for _, m := range msgs {
		groups[m.To] = append(groups[m.To], m)
	}

	r.peersMu.RLock()
	for to, ml := range groups {
		p, exists := r.peers[to]
		if !exists {
			r.lg.Warn("unknown peer, message dropped", "to", to)
			continue
		}
		p.Send(ml)
	}
	r.peersMu.RUnlock()
}

func (r *RaftTransport) Start() error {
	if r.stopped.Load() {
		return ErrTransportStopped
	}
	if !r.started.CompareAndSwap(false, true) {
		return ErrTransportStarted
	}

	if r.cfg.Listener != nil {
		r.ln = r.cfg.Listener
	} else {
		ln, err := net.Listen("tcp", r.cfg.ListenAddr)
		if err != nil {
			r.started.Store(false)
			return err
		}
		r.ln = ln
	}
	r.wg.Go(r.acceptLoop)

	return nil
}

func (r *RaftTransport) acceptLoop() {
	for {
		conn, err := r.ln.Accept()
		if err != nil {
			select {
			case <-r.ctx.Done():
				return
			default:
			}
			r.lg.Warn("failed to accept connection", "error", err)
			continue
		}

		r.mu.Lock()
		r.conns[conn] = struct{}{}
		r.mu.Unlock()

		f := transport.NewFramer(conn, conn)
		r.wg.Go(func() { r.readLoop(conn, f) })
	}
}

func (r *RaftTransport) readLoop(conn net.Conn, f *transport.Framer) {
	defer func() {
		conn.Close()
		r.mu.Lock()
		delete(r.conns, conn)
		r.mu.Unlock()
	}()
	identified := false
	for {
		select {
		case <-r.ctx.Done():
			return
		default:
		}

		payload, err := f.Read()
		if err != nil {
			select {
			case <-r.ctx.Done():
				return
			default:
			}
			r.lg.Debug("read failed", "error", err)
			return
		}

		msg := &raftpb.Message{}
		if err := proto.Unmarshal(payload, msg); err != nil {
			r.lg.Warn("failed to unmarshal message", "error", err)
			continue
		}

		if err := r.raft.Process(r.ctx, msg); err != nil {
			r.lg.Warn("failed to process inbound message", "from", msg.From, "error", err)
		}

		// On first message, wake the peer's writer from backoff.
		if !identified {
			identified = true
			r.peersMu.RLock()
			p, exists := r.peers[msg.From]
			r.peersMu.RUnlock()
			if exists {
				if rp, ok := p.(resettable); ok {
					rp.Reset()
				}
			}
		}
	}
}

func (r *RaftTransport) Stop() {
	r.stopped.Store(true)
	r.cancel()

	if r.ln != nil {
		r.ln.Close()
	}

	r.peersMu.RLock()
	for _, p := range r.peers {
		p.Stop()
	}
	r.peersMu.RUnlock()

	r.mu.Lock()
	for c := range r.conns {
		c.Close()
	}
	r.mu.Unlock()

	r.wg.Wait()
}

func (r *RaftTransport) Addr() net.Addr {
	if r.ln != nil {
		return r.ln.Addr()
	}
	return nil
}

func (r *RaftTransport) AddPeer(id uint64, addr string) {
	r.peersMu.Lock()
	defer r.peersMu.Unlock()

	if _, exists := r.peers[id]; exists {
		return
	}

	r.peers[id] = newTCPPeer(id, addr, r.cfg.WriteTimeout, r.lg)
}
