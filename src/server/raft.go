package server

import (
	"context"
	"errors"
	"fmt"
	"kvgo/raft"
	"kvgo/raftpb"
	"sync"
	"sync/atomic"
	"time"
)

type RaftTransporter interface {
	Send(msgs []*raftpb.Message)
	AddPeer(id uint64, addr string)
	Start() error
	Stop()
}

type RaftHostConfig struct {
	ID            uint64
	Peers         []uint64
	Storage       raft.Storage
	Transport     RaftTransporter
	HeartbeatTick int           // raft logical ticks between heartbeats (default 1)
	ElectionTick  int           // raft logical ticks for election timeout (default 10)
	TickInterval  time.Duration // wall-clock interval per logical tick (default 100ms)
}

type raftHostConfig struct {
	RaftHostConfig
	n raft.Node
}

type toApply struct {
	data        [][]byte
	appliedThru uint64
}

type RaftHost interface {
	Propose(ctx context.Context, data []byte) error
	Step(ctx context.Context, m *raftpb.Message) error
	Campaign(ctx context.Context) error
	Apply() <-chan toApply
	LeaderID() uint64

	Start()
	Stop()
	Errors() <-chan error
}

type raftHost struct {
	n            raft.Node
	peers        []uint64
	storage      raft.Storage
	transport    RaftTransporter
	tickInterval time.Duration
	applyc       chan toApply
	lead         atomic.Uint64

	started  atomic.Bool
	stopc    chan struct{}
	stopOnce sync.Once
	errc     chan error
	done     chan struct{}
}

var newRaftNode = raft.NewNode

func validatePublicRaftHostConfig(cfg RaftHostConfig) error {
	if cfg.Storage == nil {
		return errors.New("storage is not presented")
	}
	if cfg.Transport == nil {
		return errors.New("transport is not presented")
	}
	return nil
}

func validateRaftHostConfig(cfg raftHostConfig) error {
	if err := validatePublicRaftHostConfig(cfg.RaftHostConfig); err != nil {
		return err
	}
	if cfg.n == nil {
		return errors.New("node is not presented")
	}
	return nil
}

func newRaftHost(cfg raftHostConfig) (*raftHost, error) {
	if err := validateRaftHostConfig(cfg); err != nil {
		return nil, err
	}

	tickInterval := cfg.TickInterval
	if tickInterval == 0 {
		tickInterval = 100 * time.Millisecond
	}

	peers := make([]uint64, len(cfg.Peers))
	copy(peers, cfg.Peers)
	host := &raftHost{
		n:            cfg.n,
		peers:        peers,
		storage:      cfg.Storage,
		transport:    cfg.Transport,
		tickInterval: tickInterval,
		applyc:       make(chan toApply, 1),
		errc:         make(chan error, 1),
		stopc:        make(chan struct{}),
		done:         make(chan struct{}),
	}

	return host, nil
}

func NewRaftHost(cfg RaftHostConfig) (*raftHost, error) {
	if err := validatePublicRaftHostConfig(cfg); err != nil {
		return nil, err
	}

	pids := make([]uint64, 0)
	for _, p := range cfg.Peers {
		pids = append(pids, p)
	}

	heartbeat := cfg.HeartbeatTick
	if heartbeat == 0 {
		heartbeat = 1
	}
	election := cfg.ElectionTick
	if election == 0 {
		election = 10
	}
	tickInterval := cfg.TickInterval
	if tickInterval == 0 {
		tickInterval = 100 * time.Millisecond
	}

	// Read the compaction boundary so raft replays committed-but-unapplied
	// entries on restart. See 037g-the-replay.md.
	snap, err := cfg.Storage.Snapshot()
	if err != nil {
		return nil, fmt.Errorf("reading snapshot for applied index: %w", err)
	}

	n := newRaftNode(raft.Config{
		ID:            cfg.ID,
		Storage:       cfg.Storage,
		Peers:         pids,
		Applied:       snap.LastIncludedIndex,
		HeartbeatTick: heartbeat,
		ElectionTick:  election,
	})

	peers := make([]uint64, len(cfg.Peers))
	copy(peers, cfg.Peers)

	return &raftHost{
		n:            n,
		peers:        peers,
		storage:      cfg.Storage,
		transport:    cfg.Transport,
		tickInterval: tickInterval,
		applyc:       make(chan toApply, 1),
		errc:         make(chan error, 1),
		stopc:        make(chan struct{}),
		done:         make(chan struct{}),
	}, nil
}

func (r *raftHost) Start() {
	if r.started.CompareAndSwap(false, true) {
		go r.start()
	}
}

func (r *raftHost) start() {
	defer close(r.done)
	ticker := time.NewTicker(r.tickInterval)
	defer ticker.Stop()
	for {
		select {
		case <-r.stopc:
			return
		case <-ticker.C:
			r.n.Tick()
		case rd := <-r.n.Ready():
			r.lead.Store(rd.Lead)
			if err := r.handleBatch(rd); err == nil {
				r.n.Advance()
			} else {
				r.errc <- err
				return
			}
		}
	}
}

func (r *raftHost) Stop() {
	r.stopOnce.Do(func() {
		close(r.stopc)
	})
	<-r.done
	r.n.Stop()
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
		total := 0
		for _, e := range rd.CommittedEntries {
			total += len(e.Data)
		}
		// single contiguous allocation — one make instead of N, cache-friendly iteration
		buf := make([]byte, total)
		data := make([][]byte, len(rd.CommittedEntries))
		for i, e := range rd.CommittedEntries {
			n := copy(buf, e.Data)
			data[i] = buf[:n]
			buf = buf[n:]
		}
		lastIndex := rd.CommittedEntries[len(rd.CommittedEntries)-1].Index
		select {
		case r.applyc <- toApply{data: data, appliedThru: lastIndex}:
		case <-r.stopc:
			return nil
		}
	}
	return nil
}

func (r *raftHost) Propose(ctx context.Context, data []byte) error {
	return r.n.Propose(ctx, data)
}

func (r *raftHost) Step(ctx context.Context, m *raftpb.Message) error {
	return r.n.Step(ctx, m)
}

func (r *raftHost) Campaign(ctx context.Context) error {
	return r.n.Campaign(ctx)
}

func (r *raftHost) Apply() <-chan toApply {
	return r.applyc
}

func (r *raftHost) Errors() <-chan error {
	return r.errc
}

func (r *raftHost) LeaderID() uint64 {
	return r.lead.Load()
}
