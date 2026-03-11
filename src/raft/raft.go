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
	Messages         []Message
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

	messages []Message
}

func NewRaft(id uint64) *Raft {
	return &Raft{
		id:    id,
		state: Follower,
		log:   make([]Entry, 0),
	}
}

func (r *Raft) Propose(data []byte) error {
	if r.state != Leader {
		return nil
	}

	e := Entry{
		Index: r.lastLogIndex + 1,
		Term:  r.term,
		Data:  data,
	}
	r.appendEntries([]Entry{e})
	r.messages = append(r.messages, Message{
		Type:    MsgApp,
		Entries: []Entry{e},
	})

	return nil
}

func (r *Raft) Ready() Ready {
	return Ready{
		Entries:          r.log[r.stableIndex:r.lastLogIndex],
		CommittedEntries: r.log[r.appliedIndex:r.commitIndex],
		Messages:         r.messages,
	}
}

func (r *Raft) HasReady() bool {
	return len(r.messages) > 0 ||
		r.stableIndex < r.lastLogIndex ||
		r.appliedIndex < r.commitIndex
}

func (r *Raft) Advance() {
	r.stableIndex = r.lastLogIndex
	r.appliedIndex = r.commitIndex
	r.messages = nil
}

func (r *Raft) CommitTo(index uint64) {
	if index < r.commitIndex || index > r.lastLogIndex {
		return
	}
	r.commitIndex = index
}

func (r *Raft) Step(m Message) error {
	switch m.Type {
	case MsgApp:
		if r.state != Follower || len(m.Entries) == 0 {
			return nil
		}

		r.appendEntries(m.Entries)
	}

	return nil
}

func (r *Raft) appendEntries(entries []Entry) {
	if len(entries) == 0 {
		return
	}

	r.log = append(r.log, entries...)
	r.lastLogIndex = entries[len(entries)-1].Index
}
