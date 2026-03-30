package raft

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"kvgo/raftpb"

	"github.com/stretchr/testify/require"
)

func TestPersistentStateSurvivesRestart_036c(t *testing.T) {
	expectedHard := &raftpb.HardState{
		Term:           1,
		CommittedIndex: 1,
		VotedFor:       1,
	}
	expectedEntries := []*raftpb.Entry{{
		Term:  1,
		Index: 1,
		Data:  []byte("foo"),
	}}

	dir := t.TempDir()
	s1, err := NewDurableStorage(dir)
	require.NoError(t, err)
	require.NoError(t, s1.Save(expectedEntries, expectedHard))
	require.NoError(t, s1.Close())

	s2, err := NewDurableStorage(dir)
	require.NoError(t, err)
	defer func() {
		require.NoError(t, s2.Close())
	}()

	actualHard, err := s2.InitialState()
	require.NoError(t, err)
	require.Equal(t, expectedHard, actualHard)

	actualEntries, err := s2.Entries(1, 2)
	require.NoError(t, err)
	require.Equal(t, expectedEntries, actualEntries)
}

func TestEntriesUsesHalfOpenRange_036c(t *testing.T) {
	s := &DurableStorage{
		entries: []*raftpb.Entry{
			{Index: 1, Term: 1, Data: []byte("one")},
			{Index: 2, Term: 1, Data: []byte("two")},
			{Index: 3, Term: 2, Data: []byte("three")},
		},
		snap: &raftpb.SnapshotMeta{},
	}

	entries, err := s.Entries(2, 4)
	require.NoError(t, err)
	require.Equal(t, []*raftpb.Entry{
		{Index: 2, Term: 1, Data: []byte("two")},
		{Index: 3, Term: 2, Data: []byte("three")},
	}, entries)
}

func TestEntriesRejectsOutOfRange_036c(t *testing.T) {
	s := &DurableStorage{
		entries: []*raftpb.Entry{
			{Index: 1, Term: 1, Data: []byte("one")},
			{Index: 2, Term: 1, Data: []byte("two")},
		},
		snap: &raftpb.SnapshotMeta{},
	}

	_, err := s.Entries(0, 1)
	require.Error(t, err)

	_, err = s.Entries(1, 4)
	require.Error(t, err)
}

func TestEntriesAllowsEmptyRange_036c(t *testing.T) {
	s := &DurableStorage{snap: &raftpb.SnapshotMeta{}}

	entries, err := s.Entries(0, 0)
	require.NoError(t, err)
	require.Empty(t, entries)
}

func TestReplayTruncatesCorruptedTail_036c(t *testing.T) {
	expectedHard := &raftpb.HardState{Term: 2, VotedFor: 1, CommittedIndex: 1}
	expectedEntries := []*raftpb.Entry{{Index: 1, Term: 2, Data: []byte("ok")}}

	dir := t.TempDir()
	s1, err := NewDurableStorage(dir)
	require.NoError(t, err)
	require.NoError(t, s1.Save(expectedEntries, expectedHard))
	require.NoError(t, s1.Close())

	path := filepath.Join(dir, logFilename)
	infoBefore, err := os.Stat(path)
	require.NoError(t, err)

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0644)
	require.NoError(t, err)
	_, err = f.Write([]byte{0x05, 0x00})
	require.NoError(t, err)
	require.NoError(t, f.Close())

	s2, err := NewDurableStorage(dir)
	require.NoError(t, err)
	defer func() {
		require.NoError(t, s2.Close())
	}()

	actualHard, err := s2.InitialState()
	require.NoError(t, err)
	require.Equal(t, expectedHard, actualHard)

	actualEntries, err := s2.Entries(1, 2)
	require.NoError(t, err)
	require.Equal(t, expectedEntries, actualEntries)

	infoAfter, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, infoBefore.Size(), infoAfter.Size())
}

func TestSaveWithEmptyHardStatePreservesExistingHardState_036m(t *testing.T) {
	dir := t.TempDir()
	s1, err := NewDurableStorage(dir)
	require.NoError(t, err)

	initial := &raftpb.HardState{Term: 2, VotedFor: 1, CommittedIndex: 1}
	require.NoError(t, s1.Save([]*raftpb.Entry{{Index: 1, Term: 2, Data: []byte("one")}}, initial))
	require.NoError(t, s1.Save([]*raftpb.Entry{{Index: 2, Term: 2, Data: []byte("two")}}, &raftpb.HardState{}))
	require.NoError(t, s1.Close())

	s2, err := NewDurableStorage(dir)
	require.NoError(t, err)
	defer func() {
		require.NoError(t, s2.Close())
	}()

	actualHard, err := s2.InitialState()
	require.NoError(t, err)
	require.Equal(t, initial, actualHard)

	actualEntries, err := s2.Entries(1, 3)
	require.NoError(t, err)
	require.Equal(t, []*raftpb.Entry{
		{Index: 1, Term: 2, Data: []byte("one")},
		{Index: 2, Term: 2, Data: []byte("two")},
	}, actualEntries)
}

func TestSaveRejectsNonContiguousEntries_036c(t *testing.T) {
	dir := t.TempDir()
	s, err := NewDurableStorage(dir)
	require.NoError(t, err)
	defer func() {
		require.NoError(t, s.Close())
	}()

	require.NoError(t, s.Save([]*raftpb.Entry{{Index: 1, Term: 1, Data: []byte("one")}}, &raftpb.HardState{Term: 1}))

	err = s.Save([]*raftpb.Entry{{Index: 3, Term: 1, Data: []byte("gap")}}, &raftpb.HardState{Term: 1})
	require.ErrorIs(t, err, ErrNonContiguous)

	entries, err := s.Entries(1, 2)
	require.NoError(t, err)
	require.Equal(t, []*raftpb.Entry{{Index: 1, Term: 1, Data: []byte("one")}}, entries)

	_, err = s.Entries(1, 3)
	require.Error(t, err)
}

func TestReplayFailsOnMalformedFullBatch_036c(t *testing.T) {
	dir := t.TempDir()
	s1, err := NewDurableStorage(dir)
	require.NoError(t, err)
	require.NoError(t, s1.Save([]*raftpb.Entry{{Index: 1, Term: 1, Data: []byte("ok")}}, &raftpb.HardState{Term: 1}))
	require.NoError(t, s1.Close())

	path := filepath.Join(dir, logFilename)
	infoBefore, err := os.Stat(path)
	require.NoError(t, err)

	badBatch := make([]byte, frameHeaderSize+HardStateBytes+frameHeaderSize)
	binary.LittleEndian.PutUint32(badBatch[0:], uint32(HardStateBytes+frameHeaderSize))
	badHard := raftpb.HardState{Term: 2, VotedFor: 1, CommittedIndex: 1}
	encodeHardState(&badHard, badBatch[frameHeaderSize:])
	binary.LittleEndian.PutUint32(badBatch[frameHeaderSize+HardStateBytes:], 1)

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0644)
	require.NoError(t, err)
	_, err = f.Write(badBatch)
	require.NoError(t, err)
	require.NoError(t, f.Close())

	_, err = NewDurableStorage(dir)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrCorruption)

	infoAfter, statErr := os.Stat(path)
	require.NoError(t, statErr)
	require.Equal(t, infoBefore.Size()+int64(len(badBatch)), infoAfter.Size())
}

func TestComputeTotalSizeReturnsExpectedNumber_036c(t *testing.T) {
	batch := []*raftpb.Entry{
		{Data: []byte("foo")},
		{Data: []byte("ba")},
	}

	total := computeTotalSize(batch)
	require.Equal(t, 45, total)
}

func TestEntriesBeforeBoundaryReturnErrCompacted_036e(t *testing.T) {
	dir := t.TempDir()
	s, err := NewDurableStorage(dir)
	require.NoError(t, err)
	defer func() {
		require.NoError(t, s.Close())
	}()

	require.NoError(t, s.Save([]*raftpb.Entry{{Index: 1, Term: 1, Data: []byte("one")}}, &raftpb.HardState{Term: 1}))
	require.NoError(t, s.Save([]*raftpb.Entry{{Index: 2, Term: 1, Data: []byte("two")}}, &raftpb.HardState{Term: 1}))
	require.NoError(t, s.Save([]*raftpb.Entry{{Index: 3, Term: 2, Data: []byte("three")}}, &raftpb.HardState{Term: 2}))

	require.NoError(t, s.Compact(2))
	_, err = s.Entries(1, 3)
	require.ErrorIs(t, err, ErrCompacted)

	ents, err := s.Entries(3, 4)
	require.NoError(t, err)
	require.Len(t, ents, 1)
	require.Equal(t, &raftpb.Entry{Index: 3, Term: 2, Data: []byte("three")}, ents[0])
}

func TestCompactionSurvivesRestart_036e(t *testing.T) {
	dir := t.TempDir()
	s1, err := NewDurableStorage(dir)
	require.NoError(t, err)

	require.NoError(t, s1.Save([]*raftpb.Entry{{Index: 1, Term: 1, Data: []byte("one")}}, &raftpb.HardState{Term: 1}))
	require.NoError(t, s1.Save([]*raftpb.Entry{{Index: 2, Term: 1, Data: []byte("two")}}, &raftpb.HardState{Term: 1}))
	require.NoError(t, s1.Save([]*raftpb.Entry{{Index: 3, Term: 2, Data: []byte("three")}}, &raftpb.HardState{Term: 2}))

	require.NoError(t, s1.Compact(2))
	require.NoError(t, s1.Close())

	s2, err := NewDurableStorage(dir)
	defer require.NoError(t, s2.Close())
	require.NoError(t, err)
	_, err = s2.Entries(1, 3)
	require.ErrorIs(t, err, ErrCompacted)

	ents, err := s2.Entries(3, 4)
	require.NoError(t, err)
	require.Len(t, ents, 1)
	require.Equal(t, &raftpb.Entry{Index: 3, Term: 2, Data: []byte("three")}, ents[0])
}

func TestReplayFailsOnGapAfterSnapshotBoundary_036e(t *testing.T) {
	dir := t.TempDir()
	s, err := NewDurableStorage(dir)
	require.NoError(t, err)

	snap := raftpb.SnapshotMeta{LastIncludedIndex: 2, LastIncludedTerm: 1}
	buf := make([]byte, SnapshotMetaBytes)
	encodeSnapshotMeta(&snap, buf)
	require.NoError(t, s.sw.write(buf))
	require.NoError(t, s.sw.sync())

	hard := raftpb.HardState{Term: 2, VotedFor: 1, CommittedIndex: 2}
	gapEntries := []*raftpb.Entry{{Index: 4, Term: 2, Data: []byte("four")}}
	require.NoError(t, s.rw.write(encodeSaveBatch(&hard, gapEntries)))
	require.NoError(t, s.rw.sync())
	require.NoError(t, s.Close())

	_, err = NewDurableStorage(dir)
	require.ErrorIs(t, err, ErrNonContiguous)
}

func TestStorageApplySnapshotDropsCoveredEntries_036k(t *testing.T) {
	dir := t.TempDir()
	s1, err := NewDurableStorage(dir)
	require.NoError(t, err)

	entries := []*raftpb.Entry{
		{Index: 1, Term: 1, Data: []byte("one")},
		{Index: 2, Term: 1, Data: []byte("two")},
		{Index: 3, Term: 2, Data: []byte("three")},
		{Index: 4, Term: 2, Data: []byte("four")},
	}
	require.NoError(t, s1.Save(entries, &raftpb.HardState{Term: 2, CommittedIndex: 3}))

	snap := &raftpb.SnapshotMeta{LastIncludedIndex: 3, LastIncludedTerm: 2}
	require.NoError(t, s1.ApplySnapshot(snap))

	actualSnap, err := s1.Snapshot()
	require.NoError(t, err)
	require.Equal(t, snap, actualSnap)
	require.Equal(t, uint64(4), s1.FirstIndex())
	require.Equal(t, uint64(4), s1.LastIndex())

	_, err = s1.Entries(1, 4)
	require.ErrorIs(t, err, ErrCompacted)

	retained, err := s1.Entries(4, 5)
	require.NoError(t, err)
	require.Equal(t, []*raftpb.Entry{{Index: 4, Term: 2, Data: []byte("four")}}, retained)
	require.NoError(t, s1.Close())

	s2, err := NewDurableStorage(dir)
	require.NoError(t, err)
	defer func() {
		require.NoError(t, s2.Close())
	}()

	actualSnap, err = s2.Snapshot()
	require.NoError(t, err)
	require.Equal(t, snap, actualSnap)
	_, err = s2.Entries(1, 4)
	require.ErrorIs(t, err, ErrCompacted)

	retained, err = s2.Entries(4, 5)
	require.NoError(t, err)
	require.Equal(t, []*raftpb.Entry{{Index: 4, Term: 2, Data: []byte("four")}}, retained)
}

func TestApplySnapshotRejectsStaleBoundary_036k(t *testing.T) {
	dir := t.TempDir()
	s, err := NewDurableStorage(dir)
	require.NoError(t, err)
	defer func() {
		require.NoError(t, s.Close())
	}()

	require.NoError(t, s.ApplySnapshot(&raftpb.SnapshotMeta{LastIncludedIndex: 4, LastIncludedTerm: 2}))
	err = s.ApplySnapshot(&raftpb.SnapshotMeta{LastIncludedIndex: 3, LastIncludedTerm: 2})
	require.ErrorIs(t, err, ErrSnapOutOfDate)

	snap, err := s.Snapshot()
	require.NoError(t, err)
	require.Equal(t, &raftpb.SnapshotMeta{LastIncludedIndex: 4, LastIncludedTerm: 2}, snap)
}

func TestSaveOverlappingEntriesTruncatesAndReplays_037e(t *testing.T) {
	dir := t.TempDir()

	// First session: save entries [1(t1), 2(t1), 3(t1)].
	s1, err := NewDurableStorage(dir)
	require.NoError(t, err)
	original := []*raftpb.Entry{
		{Index: 1, Term: 1, Data: []byte("a")},
		{Index: 2, Term: 1, Data: []byte("b")},
		{Index: 3, Term: 1, Data: []byte("c")},
	}
	require.NoError(t, s1.Save(original, &raftpb.HardState{Term: 1}))

	// Save overlapping replacement: [2(t2), 3(t2), 4(t2)].
	replacement := []*raftpb.Entry{
		{Index: 2, Term: 2, Data: []byte("B")},
		{Index: 3, Term: 2, Data: []byte("C")},
		{Index: 4, Term: 2, Data: []byte("D")},
	}
	require.NoError(t, s1.Save(replacement, &raftpb.HardState{Term: 2}))

	// In-memory state should reflect the truncation.
	require.Equal(t, uint64(4), s1.LastIndex())
	ents, err := s1.Entries(1, 5)
	require.NoError(t, err)
	require.Equal(t, uint64(1), ents[0].Term)
	require.Equal(t, uint64(2), ents[1].Term)
	require.NoError(t, s1.Close())

	// Second session: replay WAL and verify the same result.
	s2, err := NewDurableStorage(dir)
	require.NoError(t, err)
	defer func() { require.NoError(t, s2.Close()) }()

	require.Equal(t, uint64(4), s2.LastIndex())
	ents, err = s2.Entries(1, 5)
	require.NoError(t, err)
	require.Len(t, ents, 4)
	require.Equal(t, []byte("a"), ents[0].Data)
	require.Equal(t, []byte("B"), ents[1].Data)
	require.Equal(t, []byte("C"), ents[2].Data)
	require.Equal(t, []byte("D"), ents[3].Data)
}
