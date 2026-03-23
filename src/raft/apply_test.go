package raft

import (
	"errors"
	"testing"

	"kvgo/raftpb"

	"github.com/stretchr/testify/require"
)

type fakeApplyTarget struct {
	applied []*raftpb.Entry
	err     error
}

func (f *fakeApplyTarget) Apply(entries []*raftpb.Entry) error {
	if f.err != nil {
		return f.err
	}
	f.applied = append(f.applied, entries...)
	return nil
}

func TestApplierDoesNotApplyUncommittedEntries_036d(t *testing.T) {
	r := NewRaft(1, &mockStorage{})
	r.state = Leader
	target := &fakeApplyTarget{}
	a := NewApplier(r, target)

	require.NoError(t, r.Propose([]byte("foo")))
	rd, err := a.ConsumeReady()
	require.NoError(t, err)
	require.Len(t, rd.Entries, 1)
	require.Len(t, rd.CommittedEntries, 0)
	require.Empty(t, target.applied)
	require.False(t, r.HasReady())
}

func TestApplierAppliesCommittedEntriesBeforeAdvance_036d(t *testing.T) {
	r := NewRaft(1, &mockStorage{})
	r.state = Leader
	target := &fakeApplyTarget{}
	a := NewApplier(r, target)

	// Before 036f, Propose returned the created Entry directly.
	// After 036f, Propose only drives the state machine and the created work is
	// observed through Ready. This test still protects the 036d invariant: once
	// an entry is committed, the Applier must apply exactly that committed entry
	// before calling Advance.
	err := r.Propose([]byte("foo"))
	require.NoError(t, err)

	unstable, err := a.ConsumeReady()
	require.NoError(t, err)
	require.Len(t, unstable.Entries, 1)
	require.Len(t, unstable.CommittedEntries, 0)

	r.CommitTo(1)
	rd, err := a.ConsumeReady()
	require.NoError(t, err)
	require.Len(t, rd.Entries, 0)
	require.Len(t, rd.CommittedEntries, 1)
	require.Equal(t, rd.CommittedEntries, target.applied)
	require.False(t, r.HasReady())
}

func TestApplierBlocksAdvanceOnApplyFailure_036d(t *testing.T) {
	r := NewRaft(1, &mockStorage{})
	r.state = Leader
	target := &fakeApplyTarget{err: errors.New("apply failed")}
	a := NewApplier(r, target)

	require.NoError(t, r.Propose([]byte("foo")))
	_, err := a.ConsumeReady()
	require.NoError(t, err)

	r.CommitTo(1)
	rd, err := a.ConsumeReady()
	require.Error(t, err)
	require.Len(t, rd.CommittedEntries, 1)
	require.Empty(t, target.applied)
	require.True(t, r.HasReady())

	stillReady := r.Ready()
	require.Len(t, stillReady.CommittedEntries, 1)
	require.Equal(t, rd.CommittedEntries, stillReady.CommittedEntries)
}
