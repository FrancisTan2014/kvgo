package raft

import (
	"crypto/rand"
	"errors"
	"kvgo/raftpb"
	"log/slog"
	"math/big"
	"sort"
	"sync"
)

const (
	None uint64 = 0
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

	lead                      uint64
	heartbeatTimeout          int
	electionTimeout           int
	electionElapsed           int
	heartbeatElapsed          int
	randomizedElectionTimeout int
	randMu                    sync.Mutex

	tick func()
	step stepFunc

	logger *slog.Logger
}

func validate(c *Config) error {
	if c.HeartbeatTick <= 0 {
		return errors.New("heartbeat tick must be greater than 0")
	}

	if c.ElectionTick <= c.HeartbeatTick {
		return errors.New("election tick must be greater than heartbeat tick")
	}

	if c.Storage == nil {
		return errors.New("storage cannot be nil")
	}

	return nil
}

func newRaft(cfg Config) *Raft {
	if err := validate(&cfg); err != nil {
		panic(err.Error())
	}

	peers := make([]uint64, len(cfg.Peers))
	copy(peers, cfg.Peers)

	r := &Raft{
		id:               cfg.ID,
		log:              make([]*raftpb.Entry, 0),
		peers:            peers,
		messages:         make([]*raftpb.Message, 0),
		storage:          cfg.Storage,
		heartbeatTimeout: cfg.HeartbeatTick,
		electionTimeout:  cfg.ElectionTick,
		logger:           cfg.Logger,
	}

	hs, err := cfg.Storage.InitialState()
	if err != nil {
		panic(err)
	}
	if !IsEmptyHardState(hs) {
		r.term = hs.Term
		r.votedFor = hs.VotedFor
		r.commitIndex = hs.CommittedIndex
	}

	r.becomeFollower(r.term, None)
	return r
}

func (r *Raft) Propose(data []byte) error {
	return r.Step(&raftpb.Message{
		Type:    raftpb.MessageType_MsgProp,
		Entries: []*raftpb.Entry{{Data: data}},
	})
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

func (r *Raft) Tick() {
	r.tick()
}

func (r *Raft) CommitTo(index uint64) {
	if index < r.commitIndex || index > r.lastLogIndex {
		return
	}
	r.commitIndex = index
}

func (r *Raft) Step(m *raftpb.Message) error {
	if m.Term > r.term {
		// TODO: branch on m.Term vs r.term like etcd (greater, equal, lesser)
		r.becomeFollower(m.Term, m.From)
	}

	switch m.Type {
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

	case raftpb.MessageType_MsgHup:
		if r.state == Leader {
			r.logger.Debug("ignoring MsgHup because already leader", "ID", r.id)
			return nil
		}

		r.becomeCandidate()

		lastLog := r.lastLog()
		for _, pid := range r.peers {
			r.messages = append(r.messages, &raftpb.Message{
				Type:    raftpb.MessageType_MsgVote,
				From:    r.id,
				To:      pid,
				Term:    r.term,
				Index:   lastLog.Index,
				LogTerm: lastLog.Term,
			})
		}

	default:
		return r.step(r, m)
	}

	return nil
}

type stepFunc func(r *Raft, m *raftpb.Message) error

func stepLeader(r *Raft, m *raftpb.Message) error {
	switch m.Type {
	case raftpb.MessageType_MsgAppResp:
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

	case raftpb.MessageType_MsgProp:
		prev := r.lastLog()
		e := &raftpb.Entry{
			Index: prev.Index + 1,
			Term:  r.term,
			Data:  m.Entries[0].Data,
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

	case raftpb.MessageType_MsgBeat:
		for _, pid := range r.peers {
			r.messages = append(r.messages, &raftpb.Message{
				From:   r.id,
				To:     pid,
				Type:   raftpb.MessageType_MsgApp,
				Term:   r.term,
				Commit: r.commitIndex,
			})
		}

	default:
		// TODO: deal with the remaining message types
	}

	return nil
}

func stepCandidate(r *Raft, m *raftpb.Message) error {
	switch m.Type {
	case raftpb.MessageType_MsgVoteResp:
		if m.Term < r.term {
			return nil
		}
		if _, exists := r.votes[m.From]; !exists {
			return nil
		}
		r.votes[m.From] = !m.Reject
		if r.voteQuorumReached() {
			r.becomeLeader()
		}

	default:
		// TODO: deal with the remaining message types
	}

	return nil
}

func stepFollower(r *Raft, m *raftpb.Message) error {
	switch m.Type {
	case raftpb.MessageType_MsgApp:
		r.electionElapsed = 0
		r.lead = m.From

		if len(m.Entries) == 0 {
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

	case raftpb.MessageType_MsgSnap:
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

	default:
		// TODO: deal with the remaining message types
	}

	return nil
}

func (r *Raft) intN(n int) int {
	r.randMu.Lock()
	v, _ := rand.Int(rand.Reader, big.NewInt(int64(n)))
	r.randMu.Unlock()
	return int(v.Int64())
}

func (r *Raft) resetRandomizedElectionTimeout() {
	r.randomizedElectionTimeout = r.electionTimeout + r.intN(r.electionTimeout)
}

// pastElectionTimeout returns true if r.electionElapsed is greater
// than or equal to the randomized election timeout in
// [electiontimeout, 2 * electiontimeout - 1].
func (r *Raft) pastElectionTimeout() bool {
	return r.electionElapsed >= r.randomizedElectionTimeout
}

func (r *Raft) tickElection() {
	r.electionElapsed++

	if r.pastElectionTimeout() {
		r.electionElapsed = 0
		if err := r.Step(&raftpb.Message{From: r.id, Type: raftpb.MessageType_MsgHup}); err != nil {
			r.logger.Debug("error occurred during election", "error", err)
		}
	}
}

func (r *Raft) tickHeartbeat() {
	r.heartbeatElapsed++
	if r.heartbeatElapsed >= r.heartbeatTimeout {
		r.heartbeatElapsed = 0
		if err := r.Step(&raftpb.Message{From: r.id, Type: raftpb.MessageType_MsgBeat}); err != nil {
			r.logger.Debug("error occurred during checking sending heartbeat", "error", err)
		}
	}
}

func (r *Raft) becomeLeader() {
	r.state = Leader
	r.step = stepLeader
	r.tick = r.tickHeartbeat
	r.reset(r.term)
	r.lead = r.id

	r.progress = make(map[uint64]*Progress, len(r.peers))
	for _, pid := range r.peers {
		r.progress[pid] = &Progress{
			MatchIndex: 0,
			NextIndex:  r.lastLogIndex + 1,
		}
	}
}

func (r *Raft) becomeCandidate() {
	r.state = Candidate
	r.step = stepCandidate
	r.tick = r.tickElection
	r.reset(r.term + 1)
	r.votedFor = r.id
	r.votes[r.id] = true
	for _, pid := range r.peers {
		r.votes[pid] = false
	}
}

func (r *Raft) becomeFollower(term uint64, lead uint64) {
	r.state = Follower
	r.reset(term)
	r.lead = lead
	r.step = stepFollower
	r.tick = r.tickElection
}

func (r *Raft) reset(term uint64) {
	if r.term != term {
		r.term = term
		r.votedFor = None
	}
	r.lead = None

	r.electionElapsed = 0
	r.heartbeatElapsed = 0
	r.resetRandomizedElectionTimeout()

	r.votes = make(map[uint64]bool)
	r.progress = nil
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
	return r.Step(&raftpb.Message{
		Type: raftpb.MessageType_MsgHup,
	})
}
