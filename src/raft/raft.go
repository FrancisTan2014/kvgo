package raft

import (
	"errors"
	"kvgo/raftpb"
	"sort"
)

type State uint8

const (
	Follower State = iota
	Candidate
	Leader
)

var ErrUnexpectedBytes = errors.New("unexpected bytes")

type Ready struct {
	Entries          []*raftpb.Entry
	CommittedEntries []*raftpb.Entry
	Messages         []*raftpb.Message
	HardState        *raftpb.HardState
}

type Progress struct {
	MatchIndex uint64
	NextIndex  uint64
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
	log          []*raftpb.Entry
	commitIndex  uint64
	appliedIndex uint64

	// volatile local states
	lastLogIndex uint64
	stableIndex  uint64

	messages []*raftpb.Message
	progress map[uint64]*Progress
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
		log:      make([]*raftpb.Entry, 0),
		peers:    peers,
		messages: make([]*raftpb.Message, 0),
		storage:  cfg.Storage,
	}
}

func (r *Raft) Propose(data []byte) error {
	if r.state != Leader {
		return nil
	}

	prev := r.lastLog()
	e := &raftpb.Entry{
		Index: prev.Index + 1,
		Term:  r.term,
		Data:  data,
	}
	r.appendEntries([]*raftpb.Entry{e})

	for _, pid := range r.peers {
		prs, exists := r.progress[pid]
		if !exists {
			return errors.New("TODO: will be replace with real coordination code later")
		}
		r.messages = append(r.messages, &raftpb.Message{
			From:    r.id,
			To:      pid,
			Type:    raftpb.MessageType_MsgApp,
			Term:    r.term,
			Index:   prev.Index,
			LogTerm: prev.Term,
			Commit:  r.commitIndex,
			Entries: r.entriesBetween(prs.NextIndex-1, r.lastLogIndex),
		})
	}

	return nil
}

func (r *Raft) HardState() *raftpb.HardState {
	return &raftpb.HardState{
		Term:           r.term,
		VotedFor:       r.votedFor,
		CommittedIndex: r.commitIndex,
	}
}

// IsEmptyHardState reports whether a HardState is zero-valued.
// Field-by-field comparison because proto types carry internal state.
func IsEmptyHardState(hard *raftpb.HardState) bool {
	return hard == nil || (hard.Term == 0 && hard.VotedFor == 0 && hard.CommittedIndex == 0)
}

func (r *Raft) Ready() Ready {
	return Ready{
		Entries:          r.entriesBetween(r.stableIndex, r.lastLogIndex),
		CommittedEntries: r.entriesBetween(r.appliedIndex, r.commitIndex),
		Messages:         r.messages,
		HardState:        r.HardState(),
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
	r.messages = make([]*raftpb.Message, 0)
}

func (r *Raft) CommitTo(index uint64) {
	if index < r.commitIndex || index > r.lastLogIndex {
		return
	}
	r.commitIndex = index
}

func (r *Raft) Step(m *raftpb.Message) error {
	if m.Term > r.term {
		r.term = m.Term
		r.state = Follower
		r.votedFor = 0
		r.votes = nil
	}

	switch m.Type {
	case raftpb.MessageType_MsgApp:
		if r.state != Follower || len(m.Entries) == 0 {
			return nil
		}

		resp := &raftpb.Message{
			Type: raftpb.MessageType_MsgAppResp,
			From: r.id,
			To:   m.From,
			Term: r.term,
		}
		if r.anchorExists(m.Index, m.LogTerm) {
			r.appendEntries(m.Entries)
			last := m.Entries[len(m.Entries)-1]
			resp.Index = last.Index
			resp.LogTerm = last.Term

			if m.Commit > r.commitIndex {
				r.CommitTo(m.Commit)
			}
		} else {
			resp.Reject = true
			resp.Index = m.Index
			if snap, err := r.storage.Snapshot(); err == nil {
				resp.RejectHint = snap.LastIncludedIndex
			}
		}
		r.messages = append(r.messages, resp)

	case raftpb.MessageType_MsgAppResp:
		if r.state != Leader {
			return nil
		}

		prs, exists := r.progress[m.From]
		if !exists {
			return errors.New("TODO: will be replace with real coordination code later")
		}

		if m.Reject {
			firstIndex := r.storage.FirstIndex()
			if firstIndex > 0 && m.RejectHint < firstIndex-1 {
				if snap, err := r.storage.Snapshot(); err == nil {
					r.messages = append(r.messages, &raftpb.Message{
						Type:     raftpb.MessageType_MsgSnap,
						From:     r.id,
						Term:     r.term,
						To:       m.From,
						Snapshot: snap,
					})
				}
			}

			prs.NextIndex--
		} else {
			prs.MatchIndex = m.Index
			prs.NextIndex = m.Index + 1
		}

		median := r.getMedian()
		if median > r.commitIndex {
			r.CommitTo(median)
		}

	case raftpb.MessageType_MsgVote:
		rejected := m.Term < r.term ||
			(m.Term == r.term && r.votedFor != 0 && r.votedFor != m.From) ||
			!r.isCandidateUpToDate(m)
		if !rejected {
			r.votedFor = m.From
		}
		r.messages = append(r.messages, &raftpb.Message{
			Type:   raftpb.MessageType_MsgVoteResp,
			From:   r.id,
			To:     m.From,
			Term:   r.term,
			Reject: rejected,
		})

	case raftpb.MessageType_MsgVoteResp:
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
			r.progress = make(map[uint64]*Progress, len(r.peers))
			for _, pid := range r.peers {
				r.progress[pid] = &Progress{
					MatchIndex: 0,
					NextIndex:  r.lastLogIndex + 1,
				}
			}
		}

	case raftpb.MessageType_MsgSnap:
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

func (r *Raft) getMedian() uint64 {
	matches := []uint64{r.lastLogIndex} // leader's own match
	for _, p := range r.progress {
		matches = append(matches, p.MatchIndex)
	}
	sort.Slice(matches, func(i, j int) bool { return matches[i] > matches[j] })
	median := matches[len(matches)/2]
	return median
}

// anchorExists proves if the specified anchor exists locally
func (r *Raft) anchorExists(index uint64, term uint64) bool {
	if index == 0 && term == 0 {
		return true
	}

	snap, err := r.storage.Snapshot()
	if err == nil {
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

func (r *Raft) lastLog() *raftpb.Entry {
	lastLog := &raftpb.Entry{}
	if snap, err := r.storage.Snapshot(); err == nil && snap.LastIncludedIndex > 0 {
		lastLog = &raftpb.Entry{Index: snap.LastIncludedIndex, Term: snap.LastIncludedTerm}
	}
	size := len(r.log)
	if size > 0 {
		if r.log[size-1].Index >= lastLog.Index {
			lastLog = r.log[size-1]
		}
	}
	return lastLog
}

func (r *Raft) entriesBetween(lo, hi uint64) []*raftpb.Entry {
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
	ents := make([]*raftpb.Entry, end-start)
	for i, e := range r.log[start:end] {
		var data []byte
		if e.Data != nil {
			data = make([]byte, len(e.Data))
			copy(data, e.Data)
		}
		ents[i] = &raftpb.Entry{Index: e.Index, Term: e.Term, Data: data}
	}
	return ents
}

func (r *Raft) isCandidateUpToDate(candidate *raftpb.Message) bool {
	lastLog := r.lastLog()
	if candidate.LogTerm != lastLog.Term {
		return candidate.LogTerm > lastLog.Term
	}
	return candidate.Index >= lastLog.Index
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

func (r *Raft) appendEntries(entries []*raftpb.Entry) {
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
		r.messages = append(r.messages, &raftpb.Message{
			Type:    raftpb.MessageType_MsgVote,
			From:    r.id,
			To:      pid,
			Term:    r.term,
			Index:   lastLog.Index,
			LogTerm: lastLog.Term,
		})
	}

	return nil
}
