package raft

import (
	"kvgo/raftpb"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"
)

func newTestRaftLog(entries []*raftpb.Entry) *raftLog {
	ms := &mockStorage{entries: entries}
	var lastIdx uint64
	if len(entries) > 0 {
		lastIdx = entries[len(entries)-1].Index
	}
	return &raftLog{
		storage: ms,
		unstable: unstable{
			offset: lastIdx + 1,
		},
		logger: slog.Default(),
	}
}

// ---------------------------------------------------------------------------
// 040 — term() checks unstable first
// ---------------------------------------------------------------------------

func TestTermChecksUnstableFirst_040(t *testing.T) {
	l := newTestRaftLog(nil)
	// Append to unstable — not in storage.
	l.appendEntries([]*raftpb.Entry{
		{Index: 1, Term: 3},
	})

	term, err := l.term(1)
	require.NoError(t, err)
	require.Equal(t, uint64(3), term)
}

func TestTermFallsBackToStorage_040(t *testing.T) {
	l := newTestRaftLog([]*raftpb.Entry{
		{Index: 1, Term: 2},
		{Index: 2, Term: 2},
	})

	term, err := l.term(1)
	require.NoError(t, err)
	require.Equal(t, uint64(2), term)
}

func TestTermZeroSentinel_040(t *testing.T) {
	l := newTestRaftLog(nil)

	term, err := l.term(0)
	require.NoError(t, err)
	require.Equal(t, uint64(0), term)
}

// ---------------------------------------------------------------------------
// 040 — matchTerm succeeds for unstable entries
// ---------------------------------------------------------------------------

func TestMatchTermSucceedsForUnstableEntries_040(t *testing.T) {
	l := newTestRaftLog(nil)
	l.appendEntries([]*raftpb.Entry{
		{Index: 1, Term: 5},
	})

	require.True(t, l.matchTerm(1, 5))
	require.False(t, l.matchTerm(1, 4))
}

func TestMatchTermSucceedsForStorageEntries_040(t *testing.T) {
	l := newTestRaftLog([]*raftpb.Entry{
		{Index: 1, Term: 2},
	})

	require.True(t, l.matchTerm(1, 2))
	require.False(t, l.matchTerm(1, 3))
}

// ---------------------------------------------------------------------------
// 040 — lastIndex spans both layers
// ---------------------------------------------------------------------------

func TestLastIndexFromUnstable_040(t *testing.T) {
	l := newTestRaftLog([]*raftpb.Entry{
		{Index: 1, Term: 1},
	})
	l.appendEntries([]*raftpb.Entry{
		{Index: 2, Term: 1},
		{Index: 3, Term: 1},
	})

	require.Equal(t, uint64(3), l.lastIndex())
}

func TestLastIndexFromStorageWhenUnstableEmpty_040(t *testing.T) {
	l := newTestRaftLog([]*raftpb.Entry{
		{Index: 1, Term: 1},
		{Index: 2, Term: 1},
	})

	require.Equal(t, uint64(2), l.lastIndex())
}

func TestLastIndexZeroWhenEmpty_040(t *testing.T) {
	l := newTestRaftLog(nil)

	require.Equal(t, uint64(0), l.lastIndex())
}

// ---------------------------------------------------------------------------
// 040 — slice cross-layer read
// ---------------------------------------------------------------------------

func TestSliceFromStorageOnly_040(t *testing.T) {
	l := newTestRaftLog([]*raftpb.Entry{
		{Index: 1, Term: 1, Data: []byte("a")},
		{Index: 2, Term: 1, Data: []byte("b")},
		{Index: 3, Term: 1, Data: []byte("c")},
	})

	ents := l.slice(1, 4)
	require.Len(t, ents, 3)
	require.Equal(t, []byte("a"), ents[0].Data)
	require.Equal(t, []byte("c"), ents[2].Data)
}

func TestSliceFromUnstableOnly_040(t *testing.T) {
	l := newTestRaftLog(nil)
	l.appendEntries([]*raftpb.Entry{
		{Index: 1, Term: 1, Data: []byte("x")},
		{Index: 2, Term: 1, Data: []byte("y")},
	})

	ents := l.slice(1, 3)
	require.Len(t, ents, 2)
	require.Equal(t, []byte("x"), ents[0].Data)
	require.Equal(t, []byte("y"), ents[1].Data)
}

func TestSliceCrossBothLayers_040(t *testing.T) {
	l := newTestRaftLog([]*raftpb.Entry{
		{Index: 1, Term: 1, Data: []byte("a")},
		{Index: 2, Term: 1, Data: []byte("b")},
	})
	l.appendEntries([]*raftpb.Entry{
		{Index: 3, Term: 1, Data: []byte("c")},
		{Index: 4, Term: 1, Data: []byte("d")},
	})

	ents := l.slice(1, 5)
	require.Len(t, ents, 4)
	require.Equal(t, []byte("a"), ents[0].Data)
	require.Equal(t, []byte("d"), ents[3].Data)
}

// ---------------------------------------------------------------------------
// 040 — nextCommittedEnts
// ---------------------------------------------------------------------------

func TestNextCommittedEntsReturnsCommittedRange_040(t *testing.T) {
	l := newTestRaftLog([]*raftpb.Entry{
		{Index: 1, Term: 1, Data: []byte("a")},
		{Index: 2, Term: 1, Data: []byte("b")},
		{Index: 3, Term: 1, Data: []byte("c")},
	})
	l.commitTo(3)

	ents := l.nextCommittedEnts()
	require.Len(t, ents, 3)
	require.Equal(t, []byte("a"), ents[0].Data)
	require.Equal(t, []byte("c"), ents[2].Data)
}

func TestNextCommittedEntsReturnsNilWhenAppliedEqualsCommitted_040(t *testing.T) {
	l := newTestRaftLog([]*raftpb.Entry{
		{Index: 1, Term: 1},
	})
	l.commitTo(1)
	l.appliedTo(1)

	require.Nil(t, l.nextCommittedEnts())
}

func TestNextCommittedEntsCrossesLayers_040(t *testing.T) {
	l := newTestRaftLog([]*raftpb.Entry{
		{Index: 1, Term: 1, Data: []byte("a")},
	})
	l.appendEntries([]*raftpb.Entry{
		{Index: 2, Term: 1, Data: []byte("b")},
	})
	l.commitTo(2)

	ents := l.nextCommittedEnts()
	require.Len(t, ents, 2)
	require.Equal(t, []byte("a"), ents[0].Data)
	require.Equal(t, []byte("b"), ents[1].Data)
}

// ---------------------------------------------------------------------------
// 040 — commitTo guards
// ---------------------------------------------------------------------------

func TestCommitToNeverDecreases_040(t *testing.T) {
	l := newTestRaftLog([]*raftpb.Entry{
		{Index: 1, Term: 1},
		{Index: 2, Term: 1},
		{Index: 3, Term: 1},
	})
	l.commitTo(3)
	l.commitTo(1) // should be no-op

	require.Equal(t, uint64(3), l.committed)
}

func TestCommitToPanicsIfBeyondLastIndex_040(t *testing.T) {
	l := newTestRaftLog([]*raftpb.Entry{
		{Index: 1, Term: 1},
	})

	require.Panics(t, func() {
		l.commitTo(5)
	})
}

// ---------------------------------------------------------------------------
// 040 — appendEntries guards
// ---------------------------------------------------------------------------

func TestAppendEntriesPanicsIfOverwritingCommitted_040(t *testing.T) {
	l := newTestRaftLog([]*raftpb.Entry{
		{Index: 1, Term: 1},
		{Index: 2, Term: 1},
	})
	l.commitTo(2)

	require.Panics(t, func() {
		l.appendEntries([]*raftpb.Entry{
			{Index: 1, Term: 2}, // overwrites committed entry
		})
	})
}

// ---------------------------------------------------------------------------
// 040 — isUpToDate
// ---------------------------------------------------------------------------

func TestIsUpToDateHigherTermWins_040(t *testing.T) {
	l := newTestRaftLog([]*raftpb.Entry{
		{Index: 1, Term: 1},
		{Index: 2, Term: 1},
	})

	require.True(t, l.isUpToDate(1, 2))  // higher term, shorter log
	require.False(t, l.isUpToDate(3, 0)) // lower term, longer log
}

func TestIsUpToDateSameTermLongerWins_040(t *testing.T) {
	l := newTestRaftLog([]*raftpb.Entry{
		{Index: 1, Term: 1},
		{Index: 2, Term: 1},
	})

	require.True(t, l.isUpToDate(3, 1))  // same term, longer
	require.True(t, l.isUpToDate(2, 1))  // same term, equal
	require.False(t, l.isUpToDate(1, 1)) // same term, shorter
}

// ---------------------------------------------------------------------------
// 040 — restore
// ---------------------------------------------------------------------------

func TestRestoreResetsUnstableAndAdvancesPointers_040(t *testing.T) {
	l := newTestRaftLog([]*raftpb.Entry{
		{Index: 1, Term: 1},
		{Index: 2, Term: 1},
	})
	l.appendEntries([]*raftpb.Entry{
		{Index: 3, Term: 1},
	})
	l.commitTo(2)
	l.appliedTo(1)

	l.restore(5)

	require.Equal(t, uint64(6), l.unstable.offset)
	require.Empty(t, l.unstable.entries)
	require.Equal(t, uint64(5), l.committed)
	require.Equal(t, uint64(5), l.applied)
}
