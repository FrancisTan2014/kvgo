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

type Peer struct {
	ID   uint64
	Addr string

	mu        sync.Mutex
	connected atomic.Bool
	conn      net.Conn
	framer    *transport.Framer
	writec    chan *raftpb.Message
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
	cfg   RaftTransportConfig
	raft  Raft
	peers map[uint64]*Peer

	ln net.Listener

	mu    sync.Mutex
	conns []net.Conn

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
		peers:  make(map[uint64]*Peer),
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

	for to, ml := range groups {
		r.sendTo(to, ml)
	}
}

func (r *RaftTransport) Start() error {
	if r.stopped.Load() {
		return ErrTransportStopped
	}
	if !r.started.CompareAndSwap(false, true) {
		return ErrTransportStarted
	}

	// TODO: revisit listener ownership — consider letting the caller own accept (etcd pattern)
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
		r.conns = append(r.conns, conn)
		r.mu.Unlock()

		f := transport.NewFramer(conn, conn)
		r.wg.Go(func() { r.readLoop(f) })
	}
}

func (r *RaftTransport) readLoop(f *transport.Framer) {
	for {
		select {
		case <-r.ctx.Done():
			return
		default:
		}

		if payload, err := f.Read(); err != nil {
			select {
			case <-r.ctx.Done():
				return // clean shutdown, no log
			default:
			}
			r.lg.Warn("failed to read frame", "error", err)
			return
		} else {
			msg := &raftpb.Message{}
			if err := proto.Unmarshal(payload, msg); err != nil {
				r.lg.Warn("failed to unmarshal message", "error", err)
				continue
			}
			if err := r.raft.Process(r.ctx, msg); err != nil {
				r.lg.Warn("failed to process inbound message", "from", msg.From, "error", err)
			}
		}
	}
}

func (p *Peer) writeLoop(r *RaftTransport, f *transport.Framer) {
	for {
		select {
		case <-r.ctx.Done():
			return
		case m := <-p.writec:
			data, err := proto.Marshal(m)
			if err != nil {
				r.lg.Warn("failed to marshal message", "error", err)
				continue
			}

			if err := f.WriteWithTimeout(data, r.cfg.WriteTimeout); err != nil {
				r.lg.Warn("failed to write message", "To", m.To, "error", err)
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

	for _, p := range r.peers {
		if p.conn != nil {
			p.conn.Close()
		}
	}

	r.mu.Lock()
	for _, c := range r.conns {
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
	if _, exists := r.peers[id]; exists {
		return
	}

	r.peers[id] = &Peer{
		ID:     id,
		Addr:   addr,
		writec: make(chan *raftpb.Message, messageBufferSize),
	}
}

func (p *Peer) dialPeer() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.connected.Load() {
		// another goroutine dialed the peer while this one waited for the lock
		return nil
	}

	c, err := net.Dial("tcp", p.Addr)
	if err != nil {
		return err
	}

	p.conn = c
	p.framer = transport.NewFramer(c, c)
	p.connected.Store(true)

	return nil
}

func (r *RaftTransport) sendTo(to uint64, msgs []*raftpb.Message) {
	p, exists := r.peers[to]
	if !exists {
		r.lg.Warn("unknown peer, message dropped", "to", to)
		return
	}

	if !p.connected.Load() {
		if err := p.dialPeer(); err != nil {
			r.lg.Warn("failed to dial peer", "to", to, "addr", p.Addr, "error", err)
			return
		} else {
			r.wg.Go(func() { p.writeLoop(r, p.framer) })
		}
	}

	for _, m := range msgs {
		select {
		case p.writec <- m:
		default:
			r.lg.Warn("write buffer full, message dropped", "to", to)
		}
	}
}
