package raft

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestProposeIncreasesLastLogIndex(t *testing.T) {
	r := Raft{}
	e1 := r.Propose([]byte("foo"))
	require.Equal(t, e1.Index, uint64(1))
	e2 := r.Propose([]byte("foo"))
	require.Equal(t, e2.Index, uint64(2))
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

	ready := r.Ready()
	require.Len(t, ready.Entries, 1)

	r.Advance()
	ready = r.Ready()
	require.Len(t, ready.Entries, 0)
}
