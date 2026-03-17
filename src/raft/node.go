package raft

import "context"

type Config struct {
	ID      uint64
	Peers   []uint64
	Storage Storage
}

type Node interface {
	Ready() <-chan Ready
	Propose(ctx context.Context, data []byte) error
	Step(ctx context.Context, m Message) error
	Campaign(ctx context.Context) error
	Advance()
}

type node struct {
	r         *Raft
	propc     chan proposeRequest
	stepc     chan stepRequest
	campaignc chan campaignRequest
	readyc    chan Ready
	advancec  chan struct{}
}

type proposeRequest struct {
	data []byte
	resp chan error
}

type stepRequest struct {
	m    Message
	resp chan error
}

type campaignRequest struct {
	resp chan error
}

func NewNode(ctx context.Context, cfg Config) Node {
	n := setupNode(cfg)
	go n.run(ctx)
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
	}
}

func (n *node) run(ctx context.Context) {
	var rd Ready
	var readyc chan Ready
	var advancec chan struct{}

	for {
		if n.r.HasReady() {
			readyc = n.readyc
			rd = n.r.Ready()
		}
		select {
		case readyc <- rd:
			readyc = nil
			rd = Ready{}
			advancec = n.advancec
		case req := <-n.propc:
			req.resp <- n.r.Propose(req.data)
		case req := <-n.stepc:
			req.resp <- n.r.Step(req.m)
		case req := <-n.campaignc:
			req.resp <- n.r.Campaign()
		case <-advancec:
			n.r.Advance()
			advancec = nil
		case <-ctx.Done():
			return
		}
	}
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

func (n *node) Step(ctx context.Context, m Message) error {
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
