package raft

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

type fakeApplyTarget struct {
	applied []Entry
	err     error
}

func (f *fakeApplyTarget) Apply(entries []Entry) error {
	if f.err != nil {
		return f.err
	}
	f.applied = append(f.applied, entries...)
	return nil
}

func TestApplierDoesNotApplyUncommittedEntries(t *testing.T) {
	r := NewRaft(1)
	target := &fakeApplyTarget{}
	a := NewApplier(r, target)

	r.Propose([]byte("foo"))
	rd, err := a.ConsumeReady()
	require.NoError(t, err)
	require.Len(t, rd.Entries, 1)
	require.Len(t, rd.CommittedEntries, 0)
	require.Empty(t, target.applied)
	require.False(t, r.HasReady())
}

func TestApplierAppliesCommittedEntriesBeforeAdvance(t *testing.T) {
	r := NewRaft(1)
	target := &fakeApplyTarget{}
	a := NewApplier(r, target)

	proposed := r.Propose([]byte("foo"))
	_, err := a.ConsumeReady()
	require.NoError(t, err)

	r.CommitTo(1)
	rd, err := a.ConsumeReady()
	require.NoError(t, err)
	require.Len(t, rd.Entries, 0)
	require.Len(t, rd.CommittedEntries, 1)
	require.Equal(t, []Entry{proposed}, target.applied)
	require.False(t, r.HasReady())
}

func TestApplierBlocksAdvanceOnApplyFailure(t *testing.T) {
	r := NewRaft(1)
	target := &fakeApplyTarget{err: errors.New("apply failed")}
	a := NewApplier(r, target)

	r.Propose([]byte("foo"))
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
