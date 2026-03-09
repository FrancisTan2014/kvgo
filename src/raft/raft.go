package raft

import (
	"errors"
)

type State uint8

const (
	Follower State = iota
	Candidate
	Leader
)

type Entry struct {
	Index uint64
	Term  uint64
	Data  []byte
}

var ErrUnexpectedBytes = errors.New("unexpected bytes")

const HardStateBytes = 24

type HardState struct {
	Term           uint64
	VotedFor       uint64
	CommittedIndex uint64
}

type Ready struct {
	Entries          []Entry
	CommittedEntries []Entry
}

/*
Raft presents the pure state machine of the RAFT algorithm.
Invariants:

	#1: Only committed entries may be applied to state machine.
	#2: appliedIndex <= commitIndex
*/
type Raft struct {
	id           uint64
	term         uint64
	state        State
	log          []Entry
	commitIndex  uint64
	appliedIndex uint64

	// volatile local states
	lastLogIndex uint64
	stableIndex  uint64
}

func NewRaft(id uint64) *Raft {
	return &Raft{
		id:    id,
		state: Follower,
		log:   make([]Entry, 0),
	}
}

func (r *Raft) Propose(data []byte) Entry {
	e := Entry{
		Index: r.lastLogIndex + 1,
		Term:  r.term,
		Data:  data,
	}
	r.log = append(r.log, e)
	r.lastLogIndex++
	return e
}

func (r *Raft) Ready() Ready {
	return Ready{
		Entries:          r.log[r.stableIndex:r.lastLogIndex],
		CommittedEntries: r.log[r.appliedIndex:r.commitIndex],
	}
}

func (r *Raft) HasReady() bool {
	return r.stableIndex < r.lastLogIndex ||
		r.appliedIndex < r.commitIndex
}

func (r *Raft) Advance() {
	r.stableIndex = r.lastLogIndex
	r.appliedIndex = r.commitIndex
}

func (r *Raft) CommitTo(index uint64) {
	if index < r.commitIndex || index > r.lastLogIndex {
		return
	}
	r.commitIndex = index
}
