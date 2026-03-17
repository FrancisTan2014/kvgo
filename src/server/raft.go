package server

import (
	"context"
	"errors"
	"kvgo/raft"
	"sync"
	"sync/atomic"
)

type Peer struct {
	ID uint64
}

type Applier interface {
	Apply(entries []raft.Entry) error
}

type RaftTransporter interface {
	Send(msgs []raft.Message)
}

type RaftHostConfig struct {
	ID        uint64
	Peers     []Peer
	Storage   raft.Storage
	Transport RaftTransporter
	Applier   Applier
}

type raftHostConfig struct {
	RaftHostConfig
	n raft.Node
}

type RaftHost interface {
	Propose(ctx context.Context, data []byte) error
	Step(ctx context.Context, m raft.Message) error
	Campaign(ctx context.Context) error

	Start()
	Stop()
	Errors() <-chan error
}

type raftHost struct {
	ctx    context.Context
	cancel context.CancelFunc

	n         raft.Node
	peers     []Peer
	storage   raft.Storage
	transport RaftTransporter
	applier   Applier

	started  atomic.Bool
	stopped  atomic.Bool
	errc     chan error
	done     chan struct{}
	stopOnce sync.Once
}

var newRaftNode = raft.NewNode

func validatePublicRaftHostConfig(cfg RaftHostConfig) error {
	if cfg.Storage == nil {
		return errors.New("storage is not presented")
	}
	if cfg.Transport == nil {
		return errors.New("transport is not presented")
	}
	if cfg.Applier == nil {
		return errors.New("applier is not presented")
	}
	return nil
}

func validateRaftHostConfig(cfg raftHostConfig) error {
	if cfg.Storage == nil {
		return errors.New("storage is not presented")
	}
	if cfg.Transport == nil {
		return errors.New("transport is not presented")
	}
	if cfg.Applier == nil {
		return errors.New("applier is not presented")
	}
	if cfg.n == nil {
		return errors.New("node is not presented")
	}
	return nil
}

func newRaftHost(ctx context.Context, cfg raftHostConfig) (*raftHost, error) {
	if err := validateRaftHostConfig(cfg); err != nil {
		return nil, err
	}

	rctx, cancel := context.WithCancel(ctx)
	peers := make([]Peer, len(cfg.Peers))
	copy(peers, cfg.Peers)
	host := &raftHost{
		ctx:       rctx,
		cancel:    cancel,
		n:         cfg.n,
		peers:     peers,
		storage:   cfg.Storage,
		transport: cfg.Transport,
		applier:   cfg.Applier,
		errc:      make(chan error, 1),
		done:      make(chan struct{}),
	}

	return host, nil
}

func NewRaftHost(ctx context.Context, cfg RaftHostConfig) (*raftHost, error) {
	if err := validatePublicRaftHostConfig(cfg); err != nil {
		return nil, err
	}

	rctx, cancel := context.WithCancel(ctx)
	pids := make([]uint64, 0)
	for _, p := range cfg.Peers {
		pids = append(pids, p.ID)
	}

	n := newRaftNode(rctx, raft.Config{
		ID:      cfg.ID,
		Storage: cfg.Storage,
		Peers:   pids,
	})

	peers := make([]Peer, len(cfg.Peers))
	copy(peers, cfg.Peers)

	return &raftHost{
		ctx:       rctx,
		cancel:    cancel,
		n:         n,
		peers:     peers,
		storage:   cfg.Storage,
		transport: cfg.Transport,
		applier:   cfg.Applier,
		errc:      make(chan error, 1),
		done:      make(chan struct{}),
	}, nil
}

func (r *raftHost) Start() {
	if r.stopped.Load() {
		return
	}
	if r.started.CompareAndSwap(false, true) {
		go r.start()
	}
}

func (r *raftHost) start() {
	for {
		select {
		case <-r.ctx.Done():
			r.Stop()
			return
		case rd := <-r.n.Ready():
			if err := r.handleBatch(rd); err == nil {
				r.n.Advance()
			} else {
				r.errc <- err
				r.Stop()
				return
			}
		}
	}
}

func (r *raftHost) Stop() {
	r.stopped.Store(true)
	r.started.Store(false)
	r.cancel()
	r.stopOnce.Do(func() {
		close(r.done)
	})
}

func (r *raftHost) handleBatch(rd raft.Ready) error {
	if len(rd.Entries) > 0 || !raft.IsEmptyHardState(rd.HardState) {
		if err := r.storage.Save(rd.Entries, rd.HardState); err != nil {
			return err
		}
	}
	if len(rd.Messages) > 0 {
		r.transport.Send(rd.Messages)
	}
	if len(rd.CommittedEntries) > 0 {
		if err := r.applier.Apply(rd.CommittedEntries); err != nil {
			return err
		}
	}
	return nil
}

func (r *raftHost) Propose(ctx context.Context, data []byte) error {
	return r.n.Propose(ctx, data)
}

func (r *raftHost) Step(ctx context.Context, m raft.Message) error {
	return r.n.Step(ctx, m)
}

func (r *raftHost) Campaign(ctx context.Context) error {
	return r.n.Campaign(ctx)
}

func (r *raftHost) Errors() <-chan error {
	return r.errc
}
