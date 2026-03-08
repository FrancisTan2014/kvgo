package raft

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPersistentStateSurvivesRestart(t *testing.T) {
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

func TestEntriesUsesHalfOpenRange(t *testing.T) {
	s := &DurableStorage{
		firstIndex: 1,
		lastIndex:  3,
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

func TestEntriesRejectsOutOfRange(t *testing.T) {
	s := &DurableStorage{
		firstIndex: 1,
		lastIndex:  2,
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

func TestEntriesAllowsEmptyRange(t *testing.T) {
	s := &DurableStorage{}

	entries, err := s.Entries(0, 0)
	require.NoError(t, err)
	require.Empty(t, entries)
}

func TestReplayTruncatesCorruptedTail(t *testing.T) {
	expectedHard := HardState{Term: 2, VotedFor: 1, CommittedIndex: 1}
	expectedEntries := []Entry{{Index: 1, Term: 2, Data: []byte("ok")}}

	dir := t.TempDir()
	s1, err := NewDurableStorage(dir)
	require.NoError(t, err)
	require.NoError(t, s1.Save(expectedEntries, expectedHard))
	require.NoError(t, s1.Close())

	path := filepath.Join(dir, filename)
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

func TestSaveRejectsNonContiguousEntries(t *testing.T) {
	dir := t.TempDir()
	s, err := NewDurableStorage(dir)
	require.NoError(t, err)
	defer func() {
		require.NoError(t, s.Close())
	}()

	require.NoError(t, s.Save([]Entry{{Index: 1, Term: 1, Data: []byte("one")}}, HardState{Term: 1}))

	err = s.Save([]Entry{{Index: 3, Term: 1, Data: []byte("gap")}}, HardState{Term: 1})
	require.Error(t, err)
	require.ErrorContains(t, err, "non-contiguous")

	entries, err := s.Entries(1, 2)
	require.NoError(t, err)
	require.Equal(t, []Entry{{Index: 1, Term: 1, Data: []byte("one")}}, entries)

	_, err = s.Entries(1, 3)
	require.Error(t, err)
}

func TestReplayFailsOnMalformedFullBatch(t *testing.T) {
	dir := t.TempDir()
	s1, err := NewDurableStorage(dir)
	require.NoError(t, err)
	require.NoError(t, s1.Save([]Entry{{Index: 1, Term: 1, Data: []byte("ok")}}, HardState{Term: 1}))
	require.NoError(t, s1.Close())

	path := filepath.Join(dir, filename)
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

func TestComputeTotalSizeReturnsExpectedNumber(t *testing.T) {
	batch := []Entry{
		{Data: []byte("foo")},
		{Data: []byte("ba")},
	}

	total := computeTotalSize(batch)
	require.Equal(t, 45, total)
}
