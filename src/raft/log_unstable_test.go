package raft

import (
	"kvgo/raftpb"
	"testing"

	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// 040 — stableTo
// ---------------------------------------------------------------------------

func TestStableToWithMatchingTerm_040(t *testing.T) {
	u := &unstable{
		offset: 5,
		entries: []*raftpb.Entry{
			{Index: 5, Term: 1},
			{Index: 6, Term: 1},
			{Index: 7, Term: 1},
		},
	}

	u.stableTo(6, 1)

	require.Equal(t, uint64(7), u.offset)
	require.Len(t, u.entries, 1)
	require.Equal(t, uint64(7), u.entries[0].Index)
}

func TestStableToWithMismatchedTerm_040(t *testing.T) {
	u := &unstable{
		offset: 5,
		entries: []*raftpb.Entry{
			{Index: 5, Term: 1},
			{Index: 6, Term: 1},
			{Index: 7, Term: 1},
		},
	}

	u.stableTo(6, 2) // wrong term

	// No-op — entries unchanged.
	require.Equal(t, uint64(5), u.offset)
	require.Len(t, u.entries, 3)
}

func TestStableToAll_040(t *testing.T) {
	u := &unstable{
		offset: 5,
		entries: []*raftpb.Entry{
			{Index: 5, Term: 1},
			{Index: 6, Term: 1},
		},
	}

	u.stableTo(6, 1)

	require.Equal(t, uint64(7), u.offset)
	require.Empty(t, u.entries)
}

func TestStableToBelowOffset_040(t *testing.T) {
	u := &unstable{
		offset: 5,
		entries: []*raftpb.Entry{
			{Index: 5, Term: 1},
		},
	}

	u.stableTo(3, 1) // below offset

	require.Equal(t, uint64(5), u.offset)
	require.Len(t, u.entries, 1)
}

func TestStableToBeyondEntries_040(t *testing.T) {
	u := &unstable{
		offset: 5,
		entries: []*raftpb.Entry{
			{Index: 5, Term: 1},
		},
	}

	u.stableTo(10, 1) // beyond entries

	require.Equal(t, uint64(5), u.offset)
	require.Len(t, u.entries, 1)
}

// ---------------------------------------------------------------------------
// 040 — nextUnstableEnts excludes stable
// ---------------------------------------------------------------------------

func TestNextUnstableEntsExcludesStable_040(t *testing.T) {
	u := &unstable{
		offset: 5,
		entries: []*raftpb.Entry{
			{Index: 5, Term: 1},
			{Index: 6, Term: 1},
			{Index: 7, Term: 1},
		},
	}

	u.stableTo(6, 1)

	// Only index 7 remains.
	require.Len(t, u.entries, 1)
	require.Equal(t, uint64(7), u.entries[0].Index)
}

// ---------------------------------------------------------------------------
// 040 — truncateAndAppend
// ---------------------------------------------------------------------------

func TestTruncateAndAppendExtends_040(t *testing.T) {
	u := &unstable{
		offset: 5,
		entries: []*raftpb.Entry{
			{Index: 5, Term: 1},
			{Index: 6, Term: 1},
		},
	}

	u.truncateAndAppend([]*raftpb.Entry{
		{Index: 7, Term: 1},
		{Index: 8, Term: 1},
	})

	require.Len(t, u.entries, 4)
	require.Equal(t, uint64(8), u.entries[3].Index)
}

func TestTruncateAndAppendReplacesAll_040(t *testing.T) {
	u := &unstable{
		offset: 5,
		entries: []*raftpb.Entry{
			{Index: 5, Term: 1},
			{Index: 6, Term: 1},
		},
	}

	u.truncateAndAppend([]*raftpb.Entry{
		{Index: 3, Term: 2},
		{Index: 4, Term: 2},
	})

	require.Equal(t, uint64(3), u.offset)
	require.Len(t, u.entries, 2)
	require.Equal(t, uint64(2), u.entries[0].Term)
}

func TestTruncateAndAppendTruncatesMiddle_040(t *testing.T) {
	u := &unstable{
		offset: 5,
		entries: []*raftpb.Entry{
			{Index: 5, Term: 1},
			{Index: 6, Term: 1},
			{Index: 7, Term: 1},
		},
	}

	u.truncateAndAppend([]*raftpb.Entry{
		{Index: 6, Term: 2},
		{Index: 7, Term: 2},
		{Index: 8, Term: 2},
	})

	require.Equal(t, uint64(5), u.offset)
	require.Len(t, u.entries, 4)
	require.Equal(t, uint64(1), u.entries[0].Term) // index 5 kept
	require.Equal(t, uint64(2), u.entries[1].Term) // index 6 replaced
	require.Equal(t, uint64(8), u.entries[3].Index)
}
