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

type entryID struct {
	index uint64
	term  uint64
}

func (e *Entry) EntryId() entryID {
	return entryID{index: e.Index, term: e.Term}
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
	acks     map[entryID]map[uint64]bool
	peers    []uint64
	votes    map[uint64]bool
	votedFor uint64
}

func NewRaft(id uint64) *Raft {
	return &Raft{
		id:       id,
		state:    Follower,
		log:      make([]Entry, 0),
		acks:     make(map[entryID]map[uint64]bool),
		peers:    make([]uint64, 0),
		messages: make([]Message, 0),
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

	id := e.EntryId()
	r.acks[id] = make(map[uint64]bool)

	// ack leader itself
	r.acks[id][r.id] = true

	for _, pid := range r.peers {
		r.messages = append(r.messages, Message{
			From:    r.id,
			To:      pid,
			Type:    MsgApp,
			Entries: []Entry{e},
		})
		r.acks[id][pid] = false
	}

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
	r.messages = make([]Message, 0)
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
		last := m.Entries[len(m.Entries)-1]
		resp := Message{
			Type:  MsgAppResp,
			From:  r.id,
			To:    m.From,
			Index: last.Index,
			Term:  last.Term,
		}
		r.messages = append(r.messages, resp)

	case MsgAppResp:
		if r.state != Leader {
			return nil
		}

		id := entryID{index: m.Index, term: m.Term}
		tracker, ok := r.acks[id]
		if !ok {
			return nil
		}

		if _, exists := tracker[m.From]; exists {
			tracker[m.From] = true
			if r.appendQuorumReached(id) {
				r.CommitTo(id.index)
			}
		}

	case MsgVote:
		rejected := m.Term < r.term ||
			(m.Term == r.term && r.votedFor != 0 && r.votedFor != m.From)
		if !rejected {
			r.term = m.Term
			r.votedFor = m.From
			r.state = Follower
		}
		resp := Message{
			Type:   MsgVoteResp,
			From:   r.id,
			To:     m.From,
			Term:   m.Term,
			Reject: rejected,
		}
		r.messages = append(r.messages, resp)

	case MsgVoteResp:
		if r.state != Candidate {
			return nil
		}
		if m.Term < r.term {
			return nil
		}
		if _, exists := r.votes[m.From]; !exists {
			return nil
		}
		r.votes[m.From] = !m.Reject
		if r.voteQuorumReached() {
			r.state = Leader
		}
	}

	return nil
}

func (r *Raft) appendQuorumReached(id entryID) bool {
	tracker, ok := r.acks[id]
	if !ok {
		return false
	}

	var cnt int
	for _, acked := range tracker {
		if acked {
			cnt++
		}
	}

	quorum := len(tracker)/2 + 1
	return cnt >= quorum
}

func (r *Raft) voteQuorumReached() bool {
	var cnt int
	for _, granted := range r.votes {
		if granted {
			cnt++
		}
	}

	quorum := len(r.votes)/2 + 1
	return cnt >= quorum
}

func (r *Raft) appendEntries(entries []Entry) {
	if len(entries) == 0 {
		return
	}

	r.log = append(r.log, entries...)
	r.lastLogIndex = entries[len(entries)-1].Index
}

func (r *Raft) Campaign() error {
	if r.state == Leader {
		return nil
	}

	r.term++
	r.state = Candidate
	r.votedFor = r.id

	r.votes = make(map[uint64]bool)
	r.votes[r.id] = true

	for _, pid := range r.peers {
		r.votes[pid] = false
		r.messages = append(r.messages, Message{
			Type: MsgVote,
			From: r.id,
			To:   pid,
			Term: r.term,
		})
	}

	return nil
}
