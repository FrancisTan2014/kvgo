package raft

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPersistentStateSurvivesRestart_036c(t *testing.T) {
	expectedHard := HardState{
		Term:           1,
		CommittedIndex: 1,
		VotedFor:       1,
	}
	expectedEntries := []Entry{{
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
		entries: []Entry{
			{Index: 1, Term: 1, Data: []byte("one")},
			{Index: 2, Term: 1, Data: []byte("two")},
			{Index: 3, Term: 2, Data: []byte("three")},
		},
	}

	entries, err := s.Entries(2, 4)
	require.NoError(t, err)
	require.Equal(t, []Entry{
		{Index: 2, Term: 1, Data: []byte("two")},
		{Index: 3, Term: 2, Data: []byte("three")},
	}, entries)
}

func TestEntriesRejectsOutOfRange_036c(t *testing.T) {
	s := &DurableStorage{
		entries: []Entry{
			{Index: 1, Term: 1, Data: []byte("one")},
			{Index: 2, Term: 1, Data: []byte("two")},
		},
	}

	_, err := s.Entries(0, 1)
	require.Error(t, err)

	_, err = s.Entries(1, 4)
	require.Error(t, err)
}

func TestEntriesAllowsEmptyRange_036c(t *testing.T) {
	s := &DurableStorage{}

	entries, err := s.Entries(0, 0)
	require.NoError(t, err)
	require.Empty(t, entries)
}

func TestReplayTruncatesCorruptedTail_036c(t *testing.T) {
	expectedHard := HardState{Term: 2, VotedFor: 1, CommittedIndex: 1}
	expectedEntries := []Entry{{Index: 1, Term: 2, Data: []byte("ok")}}

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

func TestSaveRejectsNonContiguousEntries_036c(t *testing.T) {
	dir := t.TempDir()
	s, err := NewDurableStorage(dir)
	require.NoError(t, err)
	defer func() {
		require.NoError(t, s.Close())
	}()

	require.NoError(t, s.Save([]Entry{{Index: 1, Term: 1, Data: []byte("one")}}, HardState{Term: 1}))

	err = s.Save([]Entry{{Index: 3, Term: 1, Data: []byte("gap")}}, HardState{Term: 1})
	require.ErrorIs(t, err, ErrNonContiguous)

	entries, err := s.Entries(1, 2)
	require.NoError(t, err)
	require.Equal(t, []Entry{{Index: 1, Term: 1, Data: []byte("one")}}, entries)

	_, err = s.Entries(1, 3)
	require.Error(t, err)
}

func TestReplayFailsOnMalformedFullBatch_036c(t *testing.T) {
	dir := t.TempDir()
	s1, err := NewDurableStorage(dir)
	require.NoError(t, err)
	require.NoError(t, s1.Save([]Entry{{Index: 1, Term: 1, Data: []byte("ok")}}, HardState{Term: 1}))
	require.NoError(t, s1.Close())

	path := filepath.Join(dir, logFilename)
	infoBefore, err := os.Stat(path)
	require.NoError(t, err)

	badBatch := make([]byte, frameHeaderSize+HardStateBytes+frameHeaderSize)
	binary.LittleEndian.PutUint32(badBatch[0:], uint32(HardStateBytes+frameHeaderSize))
	badHard := HardState{Term: 2, VotedFor: 1, CommittedIndex: 1}
	badHard.EncodeTo(badBatch[frameHeaderSize:])
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
	batch := []Entry{
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

	require.NoError(t, s.Save([]Entry{{Index: 1, Term: 1, Data: []byte("one")}}, HardState{Term: 1}))
	require.NoError(t, s.Save([]Entry{{Index: 2, Term: 1, Data: []byte("two")}}, HardState{Term: 1}))
	require.NoError(t, s.Save([]Entry{{Index: 3, Term: 2, Data: []byte("three")}}, HardState{Term: 2}))

	require.NoError(t, s.Compact(2))
	_, err = s.Entries(1, 3)
	require.ErrorIs(t, err, ErrCompacted)

	ents, err := s.Entries(3, 4)
	require.NoError(t, err)
	require.Len(t, ents, 1)
	require.Equal(t, Entry{Index: 3, Term: 2, Data: []byte("three")}, ents[0])
}

func TestCompactionSurvivesRestart_036e(t *testing.T) {
	dir := t.TempDir()
	s1, err := NewDurableStorage(dir)
	require.NoError(t, err)

	require.NoError(t, s1.Save([]Entry{{Index: 1, Term: 1, Data: []byte("one")}}, HardState{Term: 1}))
	require.NoError(t, s1.Save([]Entry{{Index: 2, Term: 1, Data: []byte("two")}}, HardState{Term: 1}))
	require.NoError(t, s1.Save([]Entry{{Index: 3, Term: 2, Data: []byte("three")}}, HardState{Term: 2}))

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
	require.Equal(t, Entry{Index: 3, Term: 2, Data: []byte("three")}, ents[0])
}

func TestReplayFailsOnGapAfterSnapshotBoundary_036e(t *testing.T) {
	dir := t.TempDir()
	s, err := NewDurableStorage(dir)
	require.NoError(t, err)

	snap := SnapshotMeta{LastIncludedIndex: 2, LastIncludedTerm: 1}
	buf := make([]byte, SnapshotMetaBytes)
	snap.EncodeTo(buf)
	require.NoError(t, s.sw.write(buf))
	require.NoError(t, s.sw.sync())

	hard := HardState{Term: 2, VotedFor: 1, CommittedIndex: 2}
	gapEntries := []Entry{{Index: 4, Term: 2, Data: []byte("four")}}
	require.NoError(t, s.rw.write(encodeSaveBatch(hard, gapEntries)))
	require.NoError(t, s.rw.sync())
	require.NoError(t, s.Close())

	_, err = NewDurableStorage(dir)
	require.ErrorIs(t, err, ErrNonContiguous)
}
