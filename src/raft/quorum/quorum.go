package quorum

type VoteResult uint8

const (
	VotePending VoteResult = 1 + iota
	VoteLost
	VoteWon
)

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
