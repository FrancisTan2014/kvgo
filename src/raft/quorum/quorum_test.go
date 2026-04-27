package quorum

import "testing"

func TestSingleVoterWinsOnSelfVote_038(t *testing.T) {
	c := MajorityConfig{1: {}}
	votes := map[uint64]bool{1: true}
	if got := c.VoteResult(votes); got != VoteWon {
		t.Fatalf("got %v, want VoteWon", got)
	}
}

func TestEmptyConfigWinsUnconditionally_038(t *testing.T) {
	c := MajorityConfig{}
	if got := c.VoteResult(nil); got != VoteWon {
		t.Fatalf("got %v, want VoteWon", got)
	}
	if got := c.VoteResult(map[uint64]bool{1: false}); got != VoteWon {
		t.Fatalf("got %v, want VoteWon with votes for unknown IDs", got)
	}
}

func TestThreeVoterMajority_038(t *testing.T) {
	c := MajorityConfig{1: {}, 2: {}, 3: {}}

	tests := []struct {
		name  string
		votes map[uint64]bool
		want  VoteResult
	}{
		{
			name:  "two grants",
			votes: map[uint64]bool{1: true, 2: true},
			want:  VoteWon,
		},
		{
			name:  "all three grant",
			votes: map[uint64]bool{1: true, 2: true, 3: true},
			want:  VoteWon,
		},
		{
			name:  "one grant one reject",
			votes: map[uint64]bool{1: true, 2: false},
			want:  VotePending,
		},
		{
			name:  "one grant two rejects",
			votes: map[uint64]bool{1: true, 2: false, 3: false},
			want:  VoteLost,
		},
		{
			name:  "all reject",
			votes: map[uint64]bool{1: false, 2: false, 3: false},
			want:  VoteLost,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := c.VoteResult(tt.votes); got != tt.want {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestMissingVotersArePendingNotLost_038(t *testing.T) {
	c := MajorityConfig{1: {}, 2: {}, 3: {}}
	votes := map[uint64]bool{1: true}
	if got := c.VoteResult(votes); got != VotePending {
		t.Fatalf("got %v, want VotePending", got)
	}
}

func TestFiveVoterQuorum_038(t *testing.T) {
	c := MajorityConfig{1: {}, 2: {}, 3: {}, 4: {}, 5: {}}

	tests := []struct {
		name  string
		votes map[uint64]bool
		want  VoteResult
	}{
		{
			name:  "three grants is quorum",
			votes: map[uint64]bool{1: true, 2: true, 3: true},
			want:  VoteWon,
		},
		{
			name:  "two grants two missing is pending",
			votes: map[uint64]bool{1: true, 2: true, 3: false},
			want:  VotePending,
		},
		{
			name:  "two grants three rejects is lost",
			votes: map[uint64]bool{1: true, 2: true, 3: false, 4: false, 5: false},
			want:  VoteLost,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := c.VoteResult(tt.votes); got != tt.want {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestVotesFromNonVotersIgnored_038(t *testing.T) {
	c := MajorityConfig{1: {}, 2: {}, 3: {}}
	// Node 99 is not a voter — its vote should not count.
	votes := map[uint64]bool{1: true, 99: true}
	if got := c.VoteResult(votes); got != VotePending {
		t.Fatalf("got %v, want VotePending (non-voter vote must be ignored)", got)
	}
}
