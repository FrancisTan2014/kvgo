package raft

import (
	"crypto/rand"
	"errors"
	"fmt"
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
	PreCandidate
	Candidate
	Leader
)

var (
	ErrUnexpectedBytes = errors.New("unexpected bytes")
	ErrProposalDropped = errors.New("raft: proposal dropped")
)

type Ready struct {
	Lead             uint64
	Entries          []*raftpb.Entry
	CommittedEntries []*raftpb.Entry
	Messages         []*raftpb.Message
	HardState        *raftpb.HardState
	ReadStates       []ReadState
}

type Progress struct {
	MatchIndex   uint64
	NextIndex    uint64
	RecentActive bool
}

type VoteResult uint8

const (
	VotePending VoteResult = iota
	VoteWon
	VoteLost
)

type CampaignType string

const (
	CampaignPreElection CampaignType = "PreElection"
	CampaignElection    CampaignType = "Election"
	CampaignTransfer    CampaignType = "Transfer"
)

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

	readStates               []ReadState
	readOnly                 *readOnly
	pendingReadIndexRequests []*raftpb.Message

	storage Storage

	lead                      uint64
	heartbeatTimeout          int
	electionTimeout           int
	electionElapsed           int
	heartbeatElapsed          int
	randomizedElectionTimeout int
	randMu                    sync.Mutex

	leadTransferee uint64

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

	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}

	peers := make([]uint64, len(cfg.Peers))
	copy(peers, cfg.Peers)

	r := &Raft{
		id:               cfg.ID,
		log:              make([]*raftpb.Entry, 0),
		peers:            peers,
		messages:         make([]*raftpb.Message, 0),
		readOnly:         newReadOnly(),
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

	if cfg.Applied > r.commitIndex {
		panic(fmt.Sprintf("applied(%d) is greater than committed(%d)", cfg.Applied, r.commitIndex))
	}

	// Load persisted log entries from storage (restart path).
	first := cfg.Storage.FirstIndex()
	last := cfg.Storage.LastIndex()
	if last > 0 && last >= first {
		entries, err := cfg.Storage.Entries(first, last+1)
		if err != nil {
			panic(err)
		}
		r.log = entries
		r.lastLogIndex = entries[len(entries)-1].Index
		r.stableIndex = r.lastLogIndex
	}

	// Applied=0 means "no state machine position" — replay from beginning of
	// available log. Any other value must be reachable: at or above the first
	// log entry minus one (the snapshot boundary). Values between 1 and
	// firstIndex-2 indicate a caller bug — the position was compacted away.
	if cfg.Applied > 0 && len(r.log) > 0 && cfg.Applied < r.log[0].Index-1 {
		panic(fmt.Sprintf("applied(%d) is before the first log entry(%d); compacted entries cannot be replayed",
			cfg.Applied, r.log[0].Index))
	}

	r.appliedIndex = cfg.Applied

	r.becomeFollower(r.term, None)
	return r
}

func (r *Raft) Propose(data []byte) error {
	return r.Step(&raftpb.Message{
		Type:    raftpb.MessageType_MsgProp,
		Entries: []*raftpb.Entry{{Data: data}},
	})
}

func (r *Raft) send(m *raftpb.Message) {
	if m.From == None {
		m.From = r.id
	}
	switch m.Type {
	case raftpb.MessageType_MsgPreVote, raftpb.MessageType_MsgPreVoteResp, raftpb.MessageType_MsgVote, raftpb.MessageType_MsgVoteResp:
		if m.Term == 0 {
			panic("term should be set when sending vote message")
		}
	case raftpb.MessageType_MsgProp:
		// Proposals carry no term — they are forwarded as-is.
	default:
		m.Term = r.term
	}
	r.messages = append(r.messages, m)
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
		Lead:             r.lead,
		Entries:          r.entriesBetween(r.stableIndex, r.lastLogIndex),
		CommittedEntries: r.entriesBetween(r.appliedIndex, r.commitIndex),
		Messages:         r.messages,
		HardState:        r.HardState(),
		ReadStates:       r.readStates,
	}
}

func (r *Raft) HasReady() bool {
	return len(r.messages) > 0 ||
		len(r.readStates) > 0 ||
		r.stableIndex < r.lastLogIndex ||
		r.appliedIndex < r.commitIndex
}

func (r *Raft) Advance() {
	r.stableIndex = r.lastLogIndex
	r.appliedIndex = r.commitIndex
	r.messages = make([]*raftpb.Message, 0)
	r.readStates = make([]ReadState, 0)
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
	switch {
	case m.Term == 0:
		// Local message — skip term checks.
	case m.Term > r.term:
		switch {
		case m.Type == raftpb.MessageType_MsgPreVote:
			// Never change our term in response to MsgPreVote
		case m.Type == raftpb.MessageType_MsgPreVoteResp:
			// PreVoteResp carries the proposed future term (granted) or
			// the responder's term (rejected). Neither should trigger
			// becomeFollower — let stepCandidate tally the result.
		default:
			r.becomeFollower(m.Term, m.From)
		}

	case m.Term < r.term:
		switch {
		case m.Type == raftpb.MessageType_MsgPreVote:
			r.respondVote(m.From, false, raftpb.MessageType_MsgPreVoteResp, r.term)
		}
	}

	switch m.Type {
	case raftpb.MessageType_MsgPreVote:
		granted := r.canVoteFor(m) && r.isCandidateUpToDate(m)
		r.respondVote(m.From, granted, raftpb.MessageType_MsgPreVoteResp, m.Term)

	case raftpb.MessageType_MsgVote:
		granted := r.canVoteFor(m) && r.isCandidateUpToDate(m)
		if granted {
			r.votedFor = m.From
		}
		r.respondVote(m.From, granted, raftpb.MessageType_MsgVoteResp, r.term)

	case raftpb.MessageType_MsgHup:
		r.hup(CampaignPreElection)

	default:
		return r.step(r, m)
	}

	return nil
}

type stepFunc func(r *Raft, m *raftpb.Message) error

func (r *Raft) sendAppend(to uint64) {
	prs, exists := r.progress[to]
	if !exists {
		r.logger.Warn("sendAppend to unknown peer", "to", to)
		return
	}

	prevIndex := prs.NextIndex - 1
	var prevTerm uint64
	for _, e := range r.log {
		if e.Index == prevIndex {
			prevTerm = e.Term
			break
		}
	}

	// Entry not in log — check snapshot or fall back to snapshot transfer.
	if prevIndex > 0 && prevTerm == 0 {
		snap, err := r.storage.Snapshot()
		if err != nil {
			r.logger.Warn("sendAppend failed to load snapshot", "to", to, "error", err)
			return
		}
		if snap.LastIncludedIndex == prevIndex {
			prevTerm = snap.LastIncludedTerm
		} else {
			// Anchor is compacted beyond snapshot boundary — send snapshot.
			r.logger.Info("sendAppend falling back to snapshot", "to", to, "prevIndex", prevIndex, "snapIndex", snap.LastIncludedIndex)
			r.send(&raftpb.Message{
				Type:     raftpb.MessageType_MsgSnap,
				To:       to,
				Snapshot: snap,
			})
			return
		}
	}

	r.send(&raftpb.Message{
		To:      to,
		Type:    raftpb.MessageType_MsgApp,
		Index:   prevIndex,
		LogTerm: prevTerm,
		Commit:  r.commitIndex,
		Entries: r.entriesBetween(prs.NextIndex-1, r.lastLogIndex),
	})
}

func stepLeader(r *Raft, m *raftpb.Message) error {
	switch m.Type {
	case raftpb.MessageType_MsgAppResp:
		prs, exists := r.progress[m.From]
		if !exists {
			return errors.New("TODO: will be replace with real coordination code later")
		}
		if m.Term == r.term {
			prs.RecentActive = true
		}

		if m.Reject {
			firstIndex := r.storage.FirstIndex()
			if firstIndex > 0 && m.RejectHint < firstIndex-1 {
				if snap, err := r.storage.Snapshot(); err == nil {
					r.send(&raftpb.Message{
						Type:     raftpb.MessageType_MsgSnap,
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

		// 037o: if a leader transfer is in progress and the target just caught up,
		// send MsgTimeoutNow to trigger the election.
		if m.From == r.leadTransferee && prs.MatchIndex == r.lastLogIndex {
			r.sendTimeoutNow(m.From)
			r.logger.Info("target caught up, sent MsgTimeoutNow",
				"id", r.id, "transferee", m.From)
		}

		median := r.getMedian()
		if r.maybeCommit(median) {
			// maybeCommit advanced commitIndex, and matchTerm confirmed the newly
			// committed entry is from the current term. Per Raft §5.4.2, this is the
			// moment stale commitIndex is safe to serve: the leader has proven its log
			// is up-to-date with the quorum in this term, so commitIndex now reflects
			// the linearized order. Pending ReadIndex requests can be answered.
			r.drainPendingReadIndexRequests()
		}

	case raftpb.MessageType_MsgProp:
		if r.leadTransferee != None {
			r.logger.Debug("proposal dropped: leader transfer in progress",
				"id", r.id, "term", r.term, "transferee", r.leadTransferee)
			return ErrProposalDropped
		}
		if len(m.Entries) == 0 {
			r.logger.Error("stepped empty MsgProp", "node", r.id)
			panic("stepped empty MsgProp")
		}
		r.appendEntry(m.Entries[0].Data)
		r.bcastAppend()

	case raftpb.MessageType_MsgBeat:
		r.bcastHeartbeat(nil)

	case raftpb.MessageType_MsgHeartbeatResp:
		prs, exists := r.progress[m.From]
		if !exists {
			r.logger.Warn("MsgHeartbeatResp from unknown peer", "from", m.From)
			return nil
		}
		if m.Term == r.term {
			prs.RecentActive = true
		}

		if prs.MatchIndex < r.lastLogIndex {
			r.sendAppend(m.From)
		}
		if len(m.Context) > 0 {
			acks := r.readOnly.recvAck(m.From, m.Context)
			if r.hasQuorum(acks) {
				rss := r.readOnly.advance(m)
				for _, rs := range rss {
					r.respondReadIndexReq(rs.req, rs.index)
				}
			}
		}

	case raftpb.MessageType_MsgReadIndex:
		if r.singletonCluster() {
			r.respondReadIndexReq(m, r.commitIndex)
			return nil
		}
		if !r.committedEntryInCurrentTerm() {
			r.pendingReadIndexRequests = append(r.pendingReadIndexRequests, m)
			return nil
		}

		r.readOnly.addRequest(r.commitIndex, m)
		r.readOnly.recvAck(r.id, m.Context)
		r.bcastHeartbeat(m.Context)

	case raftpb.MessageType_MsgCheckQuorum:
		if !r.hasQuorumActive() {
			r.logger.Info("leader lost quorum, stepping down",
				"term", r.term, "id", r.id)
			r.becomeFollower(r.term, None)
			return nil
		}
		r.resetRecentActive()

	case raftpb.MessageType_MsgTransferLeader:
		leadTransferee := m.From
		lastLeadTransferee := r.leadTransferee
		if lastLeadTransferee != None {
			if lastLeadTransferee == leadTransferee {
				r.logger.Info("transfer leadership already in progress to same node",
					"id", r.id, "term", r.term, "transferee", leadTransferee)
				return nil
			}
			r.abortLeaderTransfer()
			r.logger.Info("abort previous leader transfer",
				"id", r.id, "term", r.term, "prev", lastLeadTransferee)
		}
		if leadTransferee == r.id {
			r.logger.Debug("ignored transfer leadership to self", "id", r.id)
			return nil
		}
		r.logger.Info("starting leader transfer",
			"id", r.id, "term", r.term, "transferee", leadTransferee)
		// Transfer leadership should be finished in one electionTimeout, so reset r.electionElapsed.
		r.electionElapsed = 0
		r.leadTransferee = leadTransferee
		prs := r.progress[leadTransferee]
		if prs.MatchIndex == r.lastLogIndex {
			r.sendTimeoutNow(leadTransferee)
			r.logger.Info("sent MsgTimeoutNow immediately, target log is up-to-date",
				"id", r.id, "transferee", leadTransferee)
		} else {
			r.sendAppend(leadTransferee)
		}

	default:
		// TODO: deal with the remaining message types
	}

	return nil
}

func stepCandidate(r *Raft, m *raftpb.Message) error {
	voteRespType := raftpb.MessageType_MsgPreVoteResp
	if r.state == Candidate {
		voteRespType = raftpb.MessageType_MsgVoteResp
	}

	switch m.Type {
	case raftpb.MessageType_MsgProp:
		r.logger.Info("proposal dropped: no leader during election", "node", r.id)
		return ErrProposalDropped

	case voteRespType:
		if m.Term < r.term {
			return nil
		}
		r.votes[m.From] = !m.Reject

		vr := r.voteResult()
		switch vr {
		case VoteWon:
			if r.state == PreCandidate {
				r.becomeCandidate()
				r.bcastVote(raftpb.MessageType_MsgVote, r.term)
			} else {
				r.becomeLeader()
			}
		case VoteLost:
			r.becomeFollower(r.term, None)
		case VotePending:
		}

	case raftpb.MessageType_MsgApp:
		r.becomeFollower(m.Term, m.From)
		r.handleAppendEntries(m)

	case raftpb.MessageType_MsgHeartbeat:
		r.becomeFollower(m.Term, m.From)
		r.handleHeartbeat(m)

	case raftpb.MessageType_MsgSnap:
		r.becomeFollower(m.Term, m.From)
		r.handleSnapshot(m)

	case raftpb.MessageType_MsgReadIndex:
		// No leader during election — fast-fail so the caller doesn't wait
		// for its deadline.
		return ErrProposalDropped

	default:
		// TODO: deal with the remaining message types
	}

	return nil
}

func stepFollower(r *Raft, m *raftpb.Message) error {
	switch m.Type {
	case raftpb.MessageType_MsgProp:
		if r.lead == None {
			return ErrProposalDropped
		}
		m.To = r.lead
		r.send(m)

	case raftpb.MessageType_MsgApp:
		r.electionElapsed = 0
		r.lead = m.From
		r.handleAppendEntries(m)

	case raftpb.MessageType_MsgSnap:
		r.electionElapsed = 0
		r.lead = m.From
		r.handleSnapshot(m)

	case raftpb.MessageType_MsgHeartbeat:
		r.electionElapsed = 0
		r.lead = m.From
		r.handleHeartbeat(m)

	case raftpb.MessageType_MsgReadIndex:
		if r.lead == None {
			// No known leader — fast-fail so the caller doesn't wait for its
			// deadline. Mid-flight leader loss (leader existed at step time,
			// steps down before the response arrives) still relies on the
			// caller's context deadline; bounding that requires a server-side
			// leader-change notifier (see 037l open threads).
			return ErrProposalDropped
		}
		// Only rewrite To; leave From alone so send() fills it on first hop and
		// preserves the original requester's ID across re-forwards (same as MsgProp).
		m.To = r.lead
		r.send(m)

	case raftpb.MessageType_MsgReadIndexResp:
		r.readStates = append(r.readStates, ReadState{
			Index:      m.Index,
			RequestCtx: m.Context,
		})

	case raftpb.MessageType_MsgTimeoutNow:
		r.hup(CampaignTransfer)

	case raftpb.MessageType_MsgTransferLeader:
		if r.lead == None {
			r.logger.Info("no leader, dropping MsgTransferLeader", "id", r.id)
			return nil
		}
		m.To = r.lead
		r.send(m)

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
	r.electionElapsed++

	if r.electionElapsed >= r.electionTimeout {
		r.electionElapsed = 0
		if err := r.Step(&raftpb.Message{Type: raftpb.MessageType_MsgCheckQuorum}); err != nil {
			r.logger.Debug("check quorum step failed", "error", err)
		}
		// If current leader cannot transfer leadership in electionTimeout, it becomes leader again.
		if r.state == Leader && r.leadTransferee != None {
			r.abortLeaderTransfer()
		}
	}
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
			MatchIndex:   0,
			NextIndex:    r.lastLogIndex + 1,
			RecentActive: false,
		}
	}

	// Append a no-op entry in the new term. Per Raft §5.4.2, a leader cannot
	// commit entries from prior terms directly; it must commit something in
	// its own term first, which then transitively commits the tail. The no-op
	// also unblocks ReadIndex — `committedEntryInCurrentTerm` stays false
	// until this entry commits.
	r.appendEntry(nil)
	r.bcastAppend()
}

func (r *Raft) becomePreCandidate() {
	r.state = PreCandidate
	r.step = stepCandidate
	r.tick = r.tickElection
	r.lead = None
	r.resetVotes()
}

func (r *Raft) becomeCandidate() {
	r.state = Candidate
	r.step = stepCandidate
	r.tick = r.tickElection
	r.reset(r.term + 1)
	r.resetVotes()
	r.votedFor = r.id
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

	r.abortLeaderTransfer()

	r.votes = make(map[uint64]bool)
	r.progress = nil
	r.pendingReadIndexRequests = nil
	r.readOnly = newReadOnly()
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
	if err != nil {
		r.logger.Warn("anchorExists failed to load snapshot", "error", err)
		snap = nil
	}

	if snap != nil && index == snap.LastIncludedIndex && term == snap.LastIncludedTerm {
		return true
	}

	for _, e := range r.log {
		if snap != nil && e.Index <= snap.LastIncludedIndex {
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

// findConflict returns the index of the first entry in entries that conflicts
// with the existing log (same index, different term). Returns 0 if all entries
// match or the log is empty.
func (r *Raft) findConflict(entries []*raftpb.Entry) uint64 {
	if len(r.log) == 0 {
		return 0
	}
	first := r.log[0].Index
	for _, e := range entries {
		if e.Index < first {
			continue
		}
		offset := int(e.Index - first)
		if offset >= len(r.log) {
			return 0 // extends beyond — no conflict
		}
		if r.log[offset].Term != e.Term {
			return e.Index
		}
	}
	return 0 // all match
}

func (r *Raft) appendEntries(entries []*raftpb.Entry) {
	if len(entries) == 0 {
		return
	}

	if len(r.log) > 0 {
		first := r.log[0].Index
		ci := r.findConflict(entries)
		if ci > 0 {
			// Truncate at the conflict point and append from there.
			offset := int(ci - first)
			r.log = r.log[:offset]
			if truncIdx := first + uint64(offset) - 1; r.stableIndex > truncIdx {
				r.stableIndex = truncIdx
			}
			entries = entries[ci-entries[0].Index:]
		} else {
			// No conflict — skip entries we already have, keep only the tail
			// that extends beyond our log.
			lastIdx := r.log[len(r.log)-1].Index
			var tail []*raftpb.Entry
			for i, e := range entries {
				if e.Index > lastIdx {
					tail = entries[i:]
					break
				}
			}
			entries = tail
		}
		if len(entries) == 0 {
			return
		}
	}

	r.log = append(r.log, entries...)
	r.lastLogIndex = r.log[len(r.log)-1].Index
}

func (r *Raft) Campaign() error {
	return r.Step(&raftpb.Message{
		Type: raftpb.MessageType_MsgHup,
	})
}

func (r *Raft) ReadIndex(ctx []byte) error {
	return r.Step(&raftpb.Message{
		Type:    raftpb.MessageType_MsgReadIndex,
		Context: ctx,
	})
}

func (r *Raft) singletonCluster() bool {
	return len(r.peers) == 0
}

func (r *Raft) matchTerm(i uint64) bool {
	if t, err := r.storage.Term(i); err != nil {
		r.logger.Warn("matchTerm: storage.Term failed", "commitIndex", r.commitIndex, "error", err)
		return false
	} else {
		return t == r.term
	}
}

func (r *Raft) committedEntryInCurrentTerm() bool {
	return r.matchTerm(r.commitIndex)
}

func (r *Raft) maybeCommit(i uint64) bool {
	if i > r.commitIndex && r.matchTerm(i) {
		r.CommitTo(i)
		return true
	}
	return false
}

// respondReadIndexReq answers a ReadIndex request with readIndex — the
// commitIndex captured when leadership was proven for this request, not the
// current one. Serving the current commitIndex would still be linearizable
// (any later index is a legal ordering), but makes the client wait for apply
// to catch up to a newer index than necessary.
func (r *Raft) respondReadIndexReq(m *raftpb.Message, readIndex uint64) {
	if m.From == None || m.From == r.id {
		r.readStates = append(r.readStates, ReadState{
			Index:      readIndex,
			RequestCtx: m.Context,
		})
		return
	}

	r.send(&raftpb.Message{
		To:      m.From,
		Type:    raftpb.MessageType_MsgReadIndexResp,
		Context: m.Context,
		Index:   readIndex,
	})
}

func (r *Raft) bcastHeartbeat(ctx []byte) {
	for _, pid := range r.peers {
		prs := r.progress[pid]
		r.send(&raftpb.Message{
			To:      pid,
			Type:    raftpb.MessageType_MsgHeartbeat,
			Commit:  min(prs.MatchIndex, r.commitIndex),
			Context: ctx,
		})
	}
}

func (r *Raft) drainPendingReadIndexRequests() {
	if len(r.pendingReadIndexRequests) == 0 {
		return
	}

	reqs := r.pendingReadIndexRequests
	r.pendingReadIndexRequests = nil

	for _, req := range reqs {
		r.respondReadIndexReq(req, r.commitIndex)
	}
}

func (r *Raft) hasQuorum(acks map[uint64]bool) bool {
	// peers excludes self, so total voting nodes = len(peers) + 1.
	return len(acks) >= (len(r.peers)+1)/2+1
}

// appendEntry wraps data into a new Entry at the next index/current term and
// appends it to the local log. It does not broadcast.
func (r *Raft) appendEntry(data []byte) {
	prev := r.lastLog()
	e := &raftpb.Entry{
		Index: prev.Index + 1,
		Term:  r.term,
		Data:  data,
	}
	r.appendEntries([]*raftpb.Entry{e})
}

// bcastAppend sends MsgApp to every peer. Callers are responsible for having
// appended the entry locally first.
func (r *Raft) bcastAppend() {
	for _, pid := range r.peers {
		r.sendAppend(pid)
	}
}

func (r *Raft) hasQuorumActive() bool {
	active := 1 // self
	for _, pr := range r.progress {
		if pr.RecentActive {
			active++
		}
	}
	return active >= (len(r.peers)+1)/2+1
}

func (r *Raft) resetRecentActive() {
	for _, pr := range r.progress {
		pr.RecentActive = false
	}
}

func (r *Raft) resetVotes() {
	r.votes = make(map[uint64]bool)
	r.votes[r.id] = true
}

func (r *Raft) canVoteFor(m *raftpb.Message) bool {
	if m.Term < r.term {
		return false
	}
	if m.Term == r.term && r.votedFor != 0 && r.votedFor != m.From {
		return false
	}
	return true
}

func (r *Raft) bcastVote(msgType raftpb.MessageType, term uint64) {
	lastLog := r.lastLog()
	for _, pid := range r.peers {
		r.send(&raftpb.Message{
			Type:    msgType,
			To:      pid,
			Term:    term,
			Index:   lastLog.Index,
			LogTerm: lastLog.Term,
		})
	}
}

func (r *Raft) respondVote(to uint64, granted bool, respType raftpb.MessageType, term uint64) {
	r.send(&raftpb.Message{
		Type:   respType,
		To:     to,
		Term:   term,
		Reject: !granted,
	})
}

func (r *Raft) voteResult() VoteResult {
	granted, rejected := 0, 0
	for _, v := range r.votes {
		if v {
			granted++
		} else {
			rejected++
		}
	}
	quorum := (len(r.peers)+1)/2 + 1
	if granted >= quorum {
		return VoteWon
	}
	total := len(r.peers) + 1
	if granted+(total-len(r.votes)) < quorum {
		return VoteLost
	}
	return VotePending
}

func (r *Raft) handleAppendEntries(m *raftpb.Message) {
	if m.Commit > r.commitIndex {
		r.CommitTo(m.Commit)
	}

	if len(m.Entries) == 0 {
		return
	}

	resp := &raftpb.Message{
		Type: raftpb.MessageType_MsgAppResp,
		To:   m.From,
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
	r.send(resp)
}

func (r *Raft) handleHeartbeat(m *raftpb.Message) {
	if m.Commit > r.commitIndex {
		r.CommitTo(m.Commit)
	}
	r.send(&raftpb.Message{
		To:      m.From,
		Type:    raftpb.MessageType_MsgHeartbeatResp,
		Context: m.Context,
	})
}

func (r *Raft) handleSnapshot(m *raftpb.Message) {
	if err := r.storage.ApplySnapshot(m.Snapshot); err != nil {
		r.logger.Warn("failed to apply snapshot", "error", err)
		return
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

func (r *Raft) abortLeaderTransfer() {
	r.leadTransferee = None
}

func (r *Raft) sendTimeoutNow(to uint64) {
	r.send(&raftpb.Message{To: to, Type: raftpb.MessageType_MsgTimeoutNow})
}

func (r *Raft) hup(t CampaignType) {
	if r.state == Leader {
		r.logger.Debug("ignoring MsgHup because already leader", "ID", r.id)
		return
	}

	var voteMsgType raftpb.MessageType
	var term uint64
	if t == CampaignPreElection {
		r.becomePreCandidate()
		voteMsgType = raftpb.MessageType_MsgPreVote
		term = r.term + 1
	} else {
		r.becomeCandidate()
		voteMsgType = raftpb.MessageType_MsgVote
		term = r.term
	}
	r.bcastVote(voteMsgType, term)
}
