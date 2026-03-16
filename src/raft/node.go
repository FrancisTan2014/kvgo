package raft

import "context"

type Node interface {
	Ready() <-chan Ready
	Propose(ctx context.Context, data []byte) error
	Advance()
}

type node struct {
	r        *Raft
	recvc    chan []byte
	readyc   chan Ready
	advancec chan struct{}
}

func NewNode(id uint64, storage Storage) Node {
	return setupNode(id, storage)
}

func setupNode(id uint64, storage Storage) *node {
	return &node{
		r:        NewRaft(id, storage),
		recvc:    make(chan []byte),
		readyc:   make(chan Ready),
		advancec: make(chan struct{}),
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
		case d := <-n.recvc:
			n.r.Propose(d)
		case <-advancec:
			n.r.Advance()
			advancec = nil
		case <-ctx.Done():
			return
		}
	}
}

func (n *node) Propose(ctx context.Context, data []byte) error {
	n.recvc <- data
	return nil
}

func (n *node) Ready() <-chan Ready { return n.readyc }

func (n *node) Advance() {
	n.advancec <- struct{}{}
}
