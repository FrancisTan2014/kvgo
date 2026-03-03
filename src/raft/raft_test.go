package raft

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestProposeIncreasesLastLogIndex(t *testing.T) {
	r := Raft{}
	e1 := r.Propose([]byte("foo"))
	require.Equal(t, uint64(1), e1.Index)
	e2 := r.Propose([]byte("foo"))
	require.Equal(t, uint64(2), e2.Index)
}

func TestProposeNotChangeCommitIndex(t *testing.T) {
	r := Raft{}
	r.Propose([]byte(""))
	require.Zero(t, r.commitIndex)
}

func TestReadyReturnsProposedEntry(t *testing.T) {
	r := Raft{}
	r.Propose([]byte(""))
	ready := r.Ready()
	require.Len(t, ready.Entries, 1)
	require.Len(t, ready.CommittedEntries, 0)
}

func TestAdvanceClearReadyEntries(t *testing.T) {
	r := Raft{}
	r.Propose([]byte("foo"))
	r.CommitTo(1)

	ready := r.Ready()
	require.Len(t, ready.Entries, 1)
	require.Len(t, ready.CommittedEntries, 1)

	r.Advance()
	ready = r.Ready()
	require.Len(t, ready.Entries, 0)
	require.Len(t, ready.CommittedEntries, 0)
}

func TestCommitToIncreasesCommitIndex(t *testing.T) {
	r := Raft{}
	r.Propose([]byte("foo"))

	r.CommitTo(1)
	require.Equal(t, uint64(1), r.commitIndex)
}

func TestCommitToNeverDecreasesCommitIndex(t *testing.T) {
	r := Raft{}
	r.Propose([]byte("foo"))

	r.CommitTo(1)
	require.Equal(t, uint64(1), r.commitIndex)

	r.CommitTo(0)
	require.Equal(t, uint64(1), r.commitIndex)
}

func TestReadyExposesCommittedEntsOnlyAfterCommitTo(t *testing.T) {
	r := Raft{}
	r.Propose([]byte("foo"))

	ready := r.Ready()
	require.Len(t, ready.CommittedEntries, 0)

	r.CommitTo(1)
	ready = r.Ready()
	require.Len(t, ready.CommittedEntries, 1)
}

func TestFullLifecycle(t *testing.T) {
	r := Raft{}
	r.Propose([]byte("x"))

	// Phase 1: unstable only
	rd := r.Ready()
	require.Len(t, rd.Entries, 1)
	require.Len(t, rd.CommittedEntries, 0) // ← invariant: not applied yet
	r.Advance()

	// Phase 2: committed
	r.CommitTo(1)
	rd = r.Ready()
	require.Len(t, rd.Entries, 0)
	require.Len(t, rd.CommittedEntries, 1) // ← now safe to apply
	r.Advance()

	// Phase 3: idle
	require.False(t, r.HasReady())
}
