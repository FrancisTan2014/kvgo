package raft

import (
	"crypto/rand"
	"errors"
	"fmt"
	"kvgo/raft/quorum"
	"kvgo/raft/tracker"
	"kvgo/raftpb"
	"log/slog"
	"math/big"
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
	id      uint64
	term    uint64
	state   State
	raftLog *raftLog

	messages        []*raftpb.Message
	msgsAfterAppend []*raftpb.Message
	trk             *tracker.ProgressTracker
	peers           []uint64
	votedFor        uint64

	readStates               []ReadState
	readOnly                 *readOnly
	pendingReadIndexRequests []*raftpb.Message

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

	trk := tracker.NewTracker(cfg.ID, buildMajorityConfig(cfg.ID, peers))
	for _, pid := range peers {
		trk.InitProgress(pid, 0, 1)
	}

	r := &Raft{
		id:               cfg.ID,
		peers:            peers,
		messages:         make([]*raftpb.Message, 0),
		msgsAfterAppend:  make([]*raftpb.Message, 0),
		trk:              trk,
		readOnly:         newReadOnly(),
		heartbeatTimeout: cfg.HeartbeatTick,
		electionTimeout:  cfg.ElectionTick,
		logger:           cfg.Logger,
		raftLog:          newRaftLog(cfg.Storage, cfg.Logger),
	}

	hs, err := cfg.Storage.InitialState()
	if err != nil {
		panic(err)
	}
	if !IsEmptyHardState(hs) {
		r.term = hs.Term
		r.votedFor = hs.VotedFor
		r.raftLog.committed = hs.CommittedIndex
	}

	if cfg.Applied > r.raftLog.committed {
		panic(fmt.Sprintf("applied(%d) is greater than committed(%d)", cfg.Applied, r.raftLog.committed))
	}

	// Applied=0 means "no state machine position" — replay from beginning of
	// available log. Any other value must be reachable: at or above the first
	// log entry minus one (the snapshot boundary). Values between 1 and
	// firstIndex-2 indicate a caller bug — the position was compacted away.
	firstIndex := cfg.Storage.FirstIndex()
	if cfg.Applied > 0 && firstIndex > 0 && cfg.Applied < firstIndex-1 {
		panic(fmt.Sprintf("applied(%d) is before the first log entry(%d); compacted entries cannot be replayed",
			cfg.Applied, firstIndex))
	}

	if cfg.Applied > 0 {
		r.raftLog.applied = cfg.Applied
	} else if firstIndex > 0 {
		// No explicit applied position — start from the compaction boundary.
		r.raftLog.applied = firstIndex - 1
	}

	r.becomeFollower(r.term, None)
	return r
}

func buildMajorityConfig(self uint64, peers []uint64) quorum.MajorityConfig {
	c := make(quorum.MajorityConfig)
	c[self] = struct{}{}
	for _, pid := range peers {
		c[pid] = struct{}{}
	}
	return c
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

	switch m.Type {
	case raftpb.MessageType_MsgAppResp,
		raftpb.MessageType_MsgVoteResp,
		raftpb.MessageType_MsgPreVoteResp:
		r.msgsAfterAppend = append(r.msgsAfterAppend, m)
	default:
		r.messages = append(r.messages, m)
	}
}

func (r *Raft) HardState() *raftpb.HardState {
	return &raftpb.HardState{
		Term:           r.term,
		VotedFor:       r.votedFor,
		CommittedIndex: r.raftLog.committed,
	}
}

// IsEmptyHardState reports whether a HardState is zero-valued.
// Field-by-field comparison because proto types carry internal state.
func IsEmptyHardState(hard *raftpb.HardState) bool {
	return hard == nil || (hard.Term == 0 && hard.VotedFor == 0 && hard.CommittedIndex == 0)
}

func (r *Raft) Ready() Ready {
	msgs := make([]*raftpb.Message, 0, len(r.messages)+len(r.msgsAfterAppend))
	msgs = append(msgs, r.messages...)
	for _, m := range r.msgsAfterAppend {
		if m.To != r.id {
			msgs = append(msgs, m)
		}
	}
	return Ready{
		Lead:             r.lead,
		Entries:          r.raftLog.nextUnstableEnts(),
		CommittedEntries: r.raftLog.nextCommittedEnts(),
		Messages:         msgs,
		HardState:        r.HardState(),
		ReadStates:       r.readStates,
	}
}

func (r *Raft) HasReady() bool {
	return len(r.messages) > 0 ||
		len(r.msgsAfterAppend) > 0 ||
		len(r.readStates) > 0 ||
		r.raftLog.hasNextUnstableEnts() ||
		r.raftLog.hasNextCommittedEnts()
}

func (r *Raft) Advance(rd Ready) {
	if len(rd.Entries) > 0 {
		e := rd.Entries[len(rd.Entries)-1]
		r.raftLog.stableTo(e.Index, e.Term)
	}
	if len(rd.CommittedEntries) > 0 {
		e := rd.CommittedEntries[len(rd.CommittedEntries)-1]
		r.raftLog.appliedTo(e.Index)
	}
	r.messages = r.messages[:0]
	r.readStates = r.readStates[:0]
}

func (r *Raft) Tick() {
	r.tick()
}

func (r *Raft) CommitTo(index uint64) {
	r.raftLog.commitTo(index)
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
	p := r.trk.Progress(to)
	if p == nil {
		r.logger.Warn("sendAppend to unknown peer", "to", to)
		return
	}

	prevIndex := p.NextIndex - 1
	prevTerm, err := r.raftLog.term(prevIndex)
	if err != nil {
		// Entry compacted or unavailable — send snapshot instead.
		snap, serr := r.raftLog.storage.Snapshot()
		if serr != nil {
			r.logger.Warn("sendAppend failed to load snapshot", "to", to, "error", serr)
			return
		}
		r.logger.Info("sendAppend falling back to snapshot", "to", to, "prevIndex", prevIndex, "snapIndex", snap.LastIncludedIndex)
		r.send(&raftpb.Message{
			Type:     raftpb.MessageType_MsgSnap,
			To:       to,
			Snapshot: snap,
		})
		return
	}

	lastIndex := r.raftLog.lastIndex()
	r.send(&raftpb.Message{
		To:      to,
		Type:    raftpb.MessageType_MsgApp,
		Index:   prevIndex,
		LogTerm: prevTerm,
		Commit:  r.raftLog.committed,
		Entries: r.raftLog.slice(p.NextIndex, lastIndex+1),
	})
}

func stepLeader(r *Raft, m *raftpb.Message) error {
	switch m.Type {
	case raftpb.MessageType_MsgAppResp:
		prs := r.trk.Progress(m.From)
		if prs == nil {
			return errors.New("TODO: will be replace with real coordination code later")
		}
		if m.Term == r.term {
			prs.RecentActive = true
		}

		if m.Reject {
			firstIndex := r.raftLog.storage.FirstIndex()
			if firstIndex > 0 && m.RejectHint < firstIndex-1 {
				if snap, err := r.raftLog.storage.Snapshot(); err == nil {
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
		if m.From == r.leadTransferee && prs.MatchIndex == r.raftLog.lastIndex() {
			r.sendTimeoutNow(m.From)
			r.logger.Info("target caught up, sent MsgTimeoutNow",
				"id", r.id, "transferee", m.From)
		}

		if r.maybeCommit(r.trk.Committed()) {
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
		prs := r.trk.Progress(m.From)
		if prs == nil {
			r.logger.Warn("MsgHeartbeatResp from unknown peer", "from", m.From)
			return nil
		}
		if m.Term == r.term {
			prs.RecentActive = true
		}

		if prs.MatchIndex < r.raftLog.lastIndex() {
			r.sendAppend(m.From)
		}
		if len(m.Context) > 0 {
			acks := r.readOnly.recvAck(m.From, m.Context)
			if r.trk.Voters.VoteResult(acks) == quorum.VoteWon {
				rss := r.readOnly.advance(m)
				for _, rs := range rss {
					r.respondReadIndexReq(rs.req, rs.index)
				}
			}
		}

	case raftpb.MessageType_MsgReadIndex:
		if r.trk.IsSingleton() {
			r.respondReadIndexReq(m, r.raftLog.committed)
			return nil
		}
		if !r.committedEntryInCurrentTerm() {
			r.pendingReadIndexRequests = append(r.pendingReadIndexRequests, m)
			return nil
		}

		r.readOnly.addRequest(r.raftLog.committed, m)
		r.readOnly.recvAck(r.id, m.Context)
		r.bcastHeartbeat(m.Context)

	case raftpb.MessageType_MsgCheckQuorum:
		if !r.trk.QuorumActive() {
			r.logger.Info("leader lost quorum, stepping down",
				"term", r.term, "id", r.id)
			r.becomeFollower(r.term, None)
			return nil
		}
		r.trk.Visit(func(id uint64, pr *tracker.Progress) {
			if id != r.id {
				pr.RecentActive = false
			}
		})

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
		prs := r.trk.Progress(leadTransferee)
		if prs.MatchIndex == r.raftLog.lastIndex() {
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

		r.trk.RecordVote(m.From, !m.Reject)
		vr := r.trk.TallyVotes()
		switch vr {
		case quorum.VoteWon:
			if r.state == PreCandidate {
				r.becomeCandidate()
				r.trk.RecordVote(r.id, true)
				r.bcastVote(raftpb.MessageType_MsgVote, r.term)
			} else {
				r.becomeLeader()
			}
		case quorum.VoteLost:
			r.becomeFollower(r.term, None)
		case quorum.VotePending:
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
	r.trk.Progress(r.id).RecentActive = true

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
}

func (r *Raft) becomeCandidate() {
	r.state = Candidate
	r.step = stepCandidate
	r.tick = r.tickElection
	r.reset(r.term + 1)
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

	r.trk.ResetVotes()
	r.trk.Visit(func(id uint64, pr *tracker.Progress) {
		pr.MatchIndex = 0
		pr.NextIndex = r.raftLog.lastIndex() + 1
		pr.RecentActive = false
		if id == r.id {
			pr.MatchIndex = r.raftLog.lastIndex()
		}
	})

	r.pendingReadIndexRequests = nil
	r.readOnly = newReadOnly()
}

func (r *Raft) isCandidateUpToDate(candidate *raftpb.Message) bool {
	return r.raftLog.isUpToDate(candidate.Index, candidate.LogTerm)
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

func (r *Raft) committedEntryInCurrentTerm() bool {
	return r.raftLog.matchTerm(r.raftLog.committed, r.term)
}

func (r *Raft) maybeCommit(i uint64) bool {
	if i > r.raftLog.committed && r.raftLog.matchTerm(i, r.term) {
		r.raftLog.commitTo(i)
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
	r.trk.Visit(func(id uint64, pr *tracker.Progress) {
		if id == r.id {
			return
		}
		r.send(&raftpb.Message{
			To:      id,
			Type:    raftpb.MessageType_MsgHeartbeat,
			Commit:  min(pr.MatchIndex, r.raftLog.committed),
			Context: ctx,
		})
	})
}

func (r *Raft) drainPendingReadIndexRequests() {
	if len(r.pendingReadIndexRequests) == 0 {
		return
	}

	reqs := r.pendingReadIndexRequests
	r.pendingReadIndexRequests = nil

	for _, req := range reqs {
		r.respondReadIndexReq(req, r.raftLog.committed)
	}
}

// appendEntry wraps data into a new Entry at the next index/current term and
// appends it to the local log. It does not broadcast.
func (r *Raft) appendEntry(data []byte) {
	lastIndex := r.raftLog.lastIndex()
	e := &raftpb.Entry{
		Index: lastIndex + 1,
		Term:  r.term,
		Data:  data,
	}
	r.raftLog.appendEntries([]*raftpb.Entry{e})
	r.send(&raftpb.Message{To: r.id, Type: raftpb.MessageType_MsgAppResp, Index: r.raftLog.lastIndex()})
}

// bcastAppend sends MsgApp to every peer. Callers are responsible for having
// appended the entry locally first.
func (r *Raft) bcastAppend() {
	r.trk.Visit(func(id uint64, pr *tracker.Progress) {
		if id == r.id {
			return
		}
		r.sendAppend(id)
	})
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
	lastIndex, lastTerm := r.raftLog.lastEntryID()
	for _, pid := range r.peers {
		r.send(&raftpb.Message{
			Type:    msgType,
			To:      pid,
			Term:    term,
			Index:   lastIndex,
			LogTerm: lastTerm,
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

func (r *Raft) handleAppendEntries(m *raftpb.Message) {
	resp := &raftpb.Message{
		Type: raftpb.MessageType_MsgAppResp,
		To:   m.From,
	}

	if r.raftLog.matchTerm(m.Index, m.LogTerm) {
		if len(m.Entries) > 0 {
			r.raftLog.appendEntries(m.Entries)
			last := m.Entries[len(m.Entries)-1]
			resp.Index = last.Index
			resp.LogTerm = last.Term
		}
		if m.Commit > r.raftLog.committed {
			r.raftLog.commitTo(min(m.Commit, r.raftLog.lastIndex()))
		}
	} else {
		resp.Reject = true
		resp.Index = m.Index
		if snap, err := r.raftLog.storage.Snapshot(); err == nil {
			resp.RejectHint = snap.LastIncludedIndex
		}
	}
	r.send(resp)
}

func (r *Raft) handleHeartbeat(m *raftpb.Message) {
	r.raftLog.commitTo(m.Commit)
	r.send(&raftpb.Message{
		To:      m.From,
		Type:    raftpb.MessageType_MsgHeartbeatResp,
		Context: m.Context,
	})
}

func (r *Raft) handleSnapshot(m *raftpb.Message) {
	if err := r.raftLog.storage.ApplySnapshot(m.Snapshot); err != nil {
		r.logger.Warn("failed to apply snapshot", "error", err)
		return
	}
	r.raftLog.restore(m.Snapshot.LastIncludedIndex)
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
	// Record self-vote and check quorum immediately. For a single-node
	// cluster this completes the election without any network messages.
	r.trk.RecordVote(r.id, true)
	if vr := r.trk.TallyVotes(); vr == quorum.VoteWon {
		if t == CampaignPreElection {
			// PreVote won — advance to real election.
			r.hup(CampaignElection)
		} else {
			r.becomeLeader()
		}
		return
	}
	r.bcastVote(voteMsgType, term)
}
