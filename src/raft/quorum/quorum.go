package quorum

import (
	"math"
	"sort"
)

type VoteResult uint8

const (
	VotePending VoteResult = 1 + iota
	VoteLost
	VoteWon
)

type AckedIndexer interface {
	AckedIndex(voterID uint64) (idx uint64, found bool)
}

type MajorityConfig map[uint64]struct{}

func (c MajorityConfig) VoteResult(votes map[uint64]bool) VoteResult {
	if len(c) == 0 {
		return VoteWon
	}
	var granted, missing int
	for id := range c {
		v, ok := votes[id]
		if !ok {
			missing++
			continue
		}
		if v {
			granted++
		}
	}
	q := len(c)/2 + 1
	if granted >= q {
		return VoteWon
	}
	if granted+missing >= q {
		return VotePending
	}
	return VoteLost
}

func (c MajorityConfig) CommittedIndex(l AckedIndexer) uint64 {
	n := len(c)
	if n == 0 {
		return math.MaxUint64
	}
	srt := make([]uint64, n)
	i := n - 1
	for id := range c {
		if idx, ok := l.AckedIndex(id); ok {
			srt[i] = idx
			i--
		}
	}
	sort.Slice(srt, func(a, b int) bool { return srt[a] < srt[b] })
	pos := n - (n/2 + 1)
	return srt[pos]
}
