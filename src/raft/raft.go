package raft

import (
	"encoding/binary"
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

func (e *Entry) Size() int {
	return 8 + 8 + len(e.Data)
}

func (e *Entry) EncodeTo(buf []byte) {
	binary.LittleEndian.PutUint64(buf, e.Index)
	binary.LittleEndian.PutUint64(buf[8:], e.Term)
	copy(buf[16:], e.Data)
}

func DecodeEntry(data []byte) (Entry, error) {
	if len(data) < 16 {
		return Entry{}, ErrUnexpectedBytes
	}

	return Entry{
		Index: binary.LittleEndian.Uint64(data),
		Term:  binary.LittleEndian.Uint64(data[8:]),
		Data:  data[16:],
	}, nil
}

func (e *HardState) EncodeTo(buf []byte) {
	binary.LittleEndian.PutUint64(buf[0:], e.Term)
	binary.LittleEndian.PutUint64(buf[8:], e.VotedFor)
	binary.LittleEndian.PutUint64(buf[16:], e.CommittedIndex)
}

func DecodeHardState(data []byte) (HardState, error) {
	if len(data) != HardStateBytes {
		return HardState{}, ErrUnexpectedBytes
	}

	return HardState{
		Term:           binary.LittleEndian.Uint64(data[0:8]),
		VotedFor:       binary.LittleEndian.Uint64(data[8:16]),
		CommittedIndex: binary.LittleEndian.Uint64(data[16:]),
	}, nil
}
