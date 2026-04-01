package raft

import (
	"context"
	"fmt"
	"kvgo/raftpb"
	"log/slog"
	"sync"
)

type Config struct {
	ID      uint64
	Peers   []uint64
	Storage Storage

	// Applied is the last applied index. It should only be set when restarting
	// raft. raft will not return entries to the application smaller or equal to
	// Applied. If Applied is unset when restarting, raft might return previous
	// applied entries. This is a very application dependent configuration.
	Applied uint64

	ElectionTick  int
	HeartbeatTick int

	Logger *slog.Logger
}

type Node interface {
	Ready() <-chan Ready
	Propose(ctx context.Context, data []byte) error
	Step(ctx context.Context, m *raftpb.Message) error
	Campaign(ctx context.Context) error
	Advance()
	Tick()
	Stop()
}

type node struct {
	r         *Raft
	propc     chan proposeRequest
	stepc     chan stepRequest
	campaignc chan campaignRequest
	readyc    chan Ready
	advancec  chan struct{}
	tickc     chan struct{}
	stopc     chan struct{}
	stopOnce  sync.Once
	prevHard  *raftpb.HardState

	// TODO: define a logger interface instead of relying on a concrete logger
	lg *slog.Logger
}

type proposeRequest struct {
	data []byte
	resp chan error
}

type stepRequest struct {
	m    *raftpb.Message
	resp chan error
}

type campaignRequest struct {
	resp chan error
}

func NewNode(cfg Config) Node {
	n := setupNode(cfg)
	go n.run()
	return n
}

func setupNode(cfg Config) *node {
	return &node{
		r:         newRaft(cfg),
		propc:     make(chan proposeRequest),
		stepc:     make(chan stepRequest),
		campaignc: make(chan campaignRequest),
		readyc:    make(chan Ready),
		advancec:  make(chan struct{}),
		tickc:     make(chan struct{}, 128),
		stopc:     make(chan struct{}),
		lg:        cfg.Logger,
	}
}

func (n *node) run() {
	var rd Ready
	var readyc chan Ready
	var advancec chan struct{}

	for {
		if advancec == nil && n.hasReady() {
			readyc = n.readyc
			rd = n.ready()
		}
		select {
		case readyc <- rd:
			readyc = nil
			advancec = n.advancec
		case req := <-n.propc:
			req.resp <- n.r.Propose(req.data)
		case req := <-n.stepc:
			req.resp <- n.r.Step(req.m)
		case req := <-n.campaignc:
			req.resp <- n.r.Campaign()
		case <-advancec:
			if !IsEmptyHardState(rd.HardState) {
				n.prevHard = rd.HardState
			}
			n.r.Advance()
			rd = Ready{}
			advancec = nil
		case <-n.tickc:
			n.r.Tick()
		case <-n.stopc:
			return
		}
	}
}

func (n *node) hasReady() bool {
	hard := n.r.HardState()
	if !IsEmptyHardState(hard) && !hardStatesEqual(hard, n.prevHard) {
		return true
	}

	return n.r.HasReady()
}

func (n *node) ready() Ready {
	rd := n.r.Ready()
	if hardStatesEqual(rd.HardState, n.prevHard) {
		rd.HardState = nil
	}
	return rd
}

func (n *node) Propose(ctx context.Context, data []byte) error {
	resp := make(chan error, 1)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case n.propc <- proposeRequest{data: data, resp: resp}:
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-resp:
		return err
	}
}

func (n *node) Step(ctx context.Context, m *raftpb.Message) error {
	if IsLocalMsg(m.Type) {
		return fmt.Errorf("cannot step local message type %v through Node.Step", m.Type)
	}

	resp := make(chan error, 1)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case n.stepc <- stepRequest{m: m, resp: resp}:
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-resp:
		return err
	}
}

func hardStatesEqual(a, b *raftpb.HardState) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return a.Term == b.Term && a.VotedFor == b.VotedFor && a.CommittedIndex == b.CommittedIndex
}

func (n *node) Campaign(ctx context.Context) error {
	resp := make(chan error, 1)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case n.campaignc <- campaignRequest{resp: resp}:
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-resp:
		return err
	}
}

func (n *node) Ready() <-chan Ready { return n.readyc }

func (n *node) Advance() {
	n.advancec <- struct{}{}
}

func (n *node) Tick() {
	select {
	case n.tickc <- struct{}{}:
	default:
		n.lg.Warn("tick channel full, dropping tick")
	}
}

func (n *node) Stop() {
	n.stopOnce.Do(func() {
		close(n.stopc)
	})
}
