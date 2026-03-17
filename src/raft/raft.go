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

	storage Storage
}

func NewRaft(id uint64, storage Storage) *Raft {
	return newRaft(Config{ID: id, Storage: storage})
}

func newRaft(cfg Config) *Raft {
	if cfg.Storage == nil {
		panic("raft: nil storage")
	}

	peers := make([]uint64, len(cfg.Peers))
	copy(peers, cfg.Peers)

	return &Raft{
		id:       cfg.ID,
		state:    Follower,
		log:      make([]Entry, 0),
		acks:     make(map[entryID]map[uint64]bool),
		peers:    peers,
		messages: make([]Message, 0),
		storage:  cfg.Storage,
	}
}

func (r *Raft) Propose(data []byte) error {
	if r.state != Leader {
		return nil
	}

	prev := r.lastLog()
	e := Entry{
		Index: prev.Index + 1,
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
			Term:    r.term,
			Index:   prev.Index,
			LogTerm: prev.Term,
			Entries: []Entry{e},
		})
		r.acks[id][pid] = false
	}

	return nil
}

func (r *Raft) Ready() Ready {
	return Ready{
		Entries:          r.entriesBetween(r.stableIndex, r.lastLogIndex),
		CommittedEntries: r.entriesBetween(r.appliedIndex, r.commitIndex),
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
	if m.Term > r.term {
		r.term = m.Term
		r.state = Follower
		r.votedFor = 0
		r.votes = nil
	}

	switch m.Type {
	case MsgApp:
		if r.state != Follower || len(m.Entries) == 0 {
			return nil
		}

		resp := Message{
			Type: MsgAppResp,
			From: r.id,
			To:   m.From,
			Term: r.term,
		}
		if r.anchorExists(m.Index, m.LogTerm) {
			r.appendEntries(m.Entries)
			last := m.Entries[len(m.Entries)-1]
			resp.Index = last.Index
			resp.LogTerm = last.Term
		} else {
			resp.Reject = true
			resp.Index = m.Index
			if snap, err := r.storage.Snapshot(); err == nil {
				resp.RejectHint = snap.LastIncludedIndex
			}
		}
		r.messages = append(r.messages, resp)

	case MsgAppResp:
		if r.state != Leader {
			return nil
		}

		if m.Reject {
			firstIndex := r.storage.FirstIndex()
			if firstIndex > 0 && m.RejectHint < firstIndex-1 {
				if snap, err := r.storage.Snapshot(); err == nil {
					r.messages = append(r.messages, Message{
						Type:     MsgSnap,
						From:     r.id,
						Term:     r.term,
						To:       m.From,
						Snapshot: snap,
					})
				}
			}
		} else {
			id := entryID{index: m.Index, term: m.LogTerm}
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
		}

	case MsgVote:
		rejected := m.Term < r.term ||
			(m.Term == r.term && r.votedFor != 0 && r.votedFor != m.From) ||
			!r.isCandidateUpToDate(m)
		if !rejected {
			r.votedFor = m.From
		}
		resp := Message{
			Type:   MsgVoteResp,
			From:   r.id,
			To:     m.From,
			Term:   r.term,
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

	case MsgSnap:
		if r.state != Follower {
			return nil
		}

		if err := r.storage.ApplySnapshot(m.Snapshot); err != nil {
			// TODO: log the failure
			return nil
		}

		r.log = filterRetainedEntries(r.log, m.Snapshot.LastIncludedIndex)
		if len(r.log) > 0 {
			r.lastLogIndex = r.log[len(r.log)-1].Index
		} else {
			r.lastLogIndex = m.Snapshot.LastIncludedIndex
		}
		if r.stableIndex < m.Snapshot.LastIncludedIndex {
			r.stableIndex = m.Snapshot.LastIncludedIndex
		}
		if r.commitIndex < m.Snapshot.LastIncludedIndex {
			r.commitIndex = m.Snapshot.LastIncludedIndex
		}
		if r.appliedIndex < m.Snapshot.LastIncludedIndex {
			r.appliedIndex = m.Snapshot.LastIncludedIndex
		}
	}

	return nil
}

// anchorExists proves if the specified anchor exists locally
func (r *Raft) anchorExists(index uint64, term uint64) bool {
	if index == 0 && term == 0 {
		return true
	}

	snap, err := r.storage.Snapshot()
	if err == nil {
		// followers should reject anchors before the compaction boundary
		// because they are no longer verifiable anymore
		if index == snap.LastIncludedIndex && term == snap.LastIncludedTerm {
			return true
		}
	}

	for _, e := range r.log {
		if err == nil && e.Index <= snap.LastIncludedIndex {
			continue
		}
		if e.Index == index && e.Term == term {
			return true
		}
	}

	return false
}

func (r *Raft) lastLog() Entry {
	lastLog := Entry{}
	if snap, err := r.storage.Snapshot(); err == nil && snap.LastIncludedIndex > 0 {
		lastLog = Entry{Index: snap.LastIncludedIndex, Term: snap.LastIncludedTerm}
	}
	size := len(r.log)
	if size > 0 {
		if r.log[size-1].Index >= lastLog.Index {
			lastLog = r.log[size-1]
		}
	}
	return lastLog
}

func (r *Raft) entriesBetween(lo, hi uint64) []Entry {
	if hi <= lo || len(r.log) == 0 {
		return nil
	}

	first := r.log[0].Index
	startIndex := lo + 1
	if startIndex < first {
		startIndex = first
	}
	if hi < startIndex {
		return nil
	}

	start := int(startIndex - first)
	end := int(hi-first) + 1
	ents := make([]Entry, end-start)
	copy(ents, r.log[start:end])
	return ents
}

func (r *Raft) isCandidateUpToDate(candidate Message) bool {
	lastLog := r.lastLog()
	if candidate.LogTerm != lastLog.Term {
		return candidate.LogTerm > lastLog.Term
	}
	return candidate.Index >= lastLog.Index
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

	lastLog := r.lastLog()
	for _, pid := range r.peers {
		r.votes[pid] = false
		r.messages = append(r.messages, Message{
			Type:    MsgVote,
			From:    r.id,
			To:      pid,
			Term:    r.term,
			Index:   lastLog.Index,
			LogTerm: lastLog.Term,
		})
	}

	return nil
}
