package tracker

import "kvgo/raft/quorum"

type Progress struct {
	MatchIndex   uint64
	NextIndex    uint64
	RecentActive bool
}

type ProgressTracker struct {
	selfID   uint64
	Voters   quorum.MajorityConfig
	progress map[uint64]*Progress
	votes    map[uint64]bool
}

type matchAckIndexer map[uint64]*Progress

func (m matchAckIndexer) AckedIndex(id uint64) (uint64, bool) {
	pr, ok := m[id]
	if !ok {
		return 0, false
	}
	return pr.MatchIndex, true
}

func NewTracker(selfID uint64, config quorum.MajorityConfig) *ProgressTracker {
	t := &ProgressTracker{
		selfID:   selfID,
		Voters:   config,
		progress: make(map[uint64]*Progress),
		votes:    make(map[uint64]bool),
	}
	t.progress[selfID] = &Progress{}
	return t
}

func (t *ProgressTracker) InitProgress(id uint64, match, next uint64) {
	t.progress[id] = &Progress{MatchIndex: match, NextIndex: next}
}

func (p *ProgressTracker) IsSingleton() bool {
	return len(p.progress) == 1
}

func (t *ProgressTracker) ResetVotes() {
	t.votes = make(map[uint64]bool)
}

func (t *ProgressTracker) RecordVote(id uint64, v bool) {
	t.votes[id] = v
}

func (t *ProgressTracker) TallyVotes() quorum.VoteResult {
	return t.Voters.VoteResult(t.votes)
}

func (t *ProgressTracker) Committed() uint64 {
	return uint64(t.Voters.CommittedIndex(matchAckIndexer(t.progress)))
}

func (t *ProgressTracker) QuorumActive() bool {
	votes := make(map[uint64]bool)
	t.Visit(func(id uint64, pr *Progress) {
		votes[id] = pr.RecentActive
	})
	return t.Voters.VoteResult(votes) == quorum.VoteWon
}

func (t *ProgressTracker) ResetRecentActive() {
	t.Visit(func(_ uint64, pr *Progress) {
		pr.RecentActive = false
	})
}

func (t *ProgressTracker) Progress(id uint64) *Progress {
	return t.progress[id]
}

func (t *ProgressTracker) Visit(f func(id uint64, pr *Progress)) {
	for id, pr := range t.progress {
		f(id, pr)
	}
}
