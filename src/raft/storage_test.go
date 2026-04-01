package raft

import (
	"encoding/binary"
	"log/slog"
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

	path := filepath.Join(dir, segmentName(0))
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

	path := filepath.Join(dir, segmentName(0))
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

// 037f supersedes: TestCompactPreservesEntriesAboveBoundary_037f covers this
// at larger scale with segmented WAL and segment deletion.
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

	// compactLocked is internal; production compaction runs through MaybeCompact.
	s.mu.Lock()
	require.NoError(t, s.compactLocked(2))
	s.mu.Unlock()
	_, err = s.Entries(1, 3)
	require.ErrorIs(t, err, ErrCompacted)

	ents, err := s.Entries(3, 4)
	require.NoError(t, err)
	require.Len(t, ents, 1)
	require.Equal(t, &raftpb.Entry{Index: 3, Term: 2, Data: []byte("three")}, ents[0])
}

// 037f supersedes: TestSegmentDeletionReclaimsDisk_037f covers compact +
// restart with segmented WAL and segment deletion.
func TestCompactionSurvivesRestart_036e(t *testing.T) {
	dir := t.TempDir()
	s1, err := NewDurableStorage(dir)
	require.NoError(t, err)

	require.NoError(t, s1.Save([]*raftpb.Entry{{Index: 1, Term: 1, Data: []byte("one")}}, &raftpb.HardState{Term: 1}))
	require.NoError(t, s1.Save([]*raftpb.Entry{{Index: 2, Term: 1, Data: []byte("two")}}, &raftpb.HardState{Term: 1}))
	require.NoError(t, s1.Save([]*raftpb.Entry{{Index: 3, Term: 2, Data: []byte("three")}}, &raftpb.HardState{Term: 2}))

	// compactLocked is internal; production compaction runs through MaybeCompact.
	s1.mu.Lock()
	require.NoError(t, s1.compactLocked(2))
	s1.mu.Unlock()
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

// 037f rewrites: original 036e test wrote directly to the single-file WAL;
// segmented WAL makes that approach fragile. Rewritten to use Save() with a
// pre-existing snapshot boundary so the gap is detected during Save, not replay.
func TestReplayFailsOnGapAfterSnapshotBoundary_036e(t *testing.T) {
	dir := t.TempDir()

	// Set up a snapshot boundary at index 2 by writing the snapshot WAL directly.
	sw, err := newWAL(dir, snapFilename)
	require.NoError(t, err)
	snap := raftpb.SnapshotMeta{LastIncludedIndex: 2, LastIncludedTerm: 1}
	buf := make([]byte, SnapshotMetaBytes)
	encodeSnapshotMeta(&snap, buf)
	require.NoError(t, sw.write(buf))
	require.NoError(t, sw.sync())
	require.NoError(t, sw.close())

	// Write a replication log entry with a gap (index 4 when boundary is 2,
	// so expected next is 3).
	rw, err := newSegmentedWAL(dir, 0)
	require.NoError(t, err)
	hard := raftpb.HardState{Term: 2, VotedFor: 1, CommittedIndex: 2}
	gapEntries := []*raftpb.Entry{{Index: 4, Term: 2, Data: []byte("four")}}
	require.NoError(t, rw.write(encodeSaveBatch(&hard, gapEntries)))
	require.NoError(t, rw.sync())
	require.NoError(t, rw.close())

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

func TestSegmentDeletionReclaimsDisk_037f(t *testing.T) {
	dir := t.TempDir()
	s, err := NewDurableStorageWithOptions(dir, DurableStorageOptions{
		SegmentLimit: 50, // tiny segments to force rotation
	})
	require.NoError(t, err)

	// Write 10 entries; small segment limit forces multiple segments.
	for i := uint64(1); i <= 10; i++ {
		require.NoError(t, s.Save(
			[]*raftpb.Entry{{Index: i, Term: 1, Data: []byte("data")}},
			&raftpb.HardState{Term: 1, CommittedIndex: i},
		))
	}
	segsBefore := s.rw.segmentCount()
	require.Greater(t, segsBefore, 1, "expected multiple segments")

	// compactLocked is internal; production compaction runs through MaybeCompact.
	s.mu.Lock()
	require.NoError(t, s.compactLocked(5))
	s.mu.Unlock()

	segsAfter := s.rw.segmentCount()
	require.Less(t, segsAfter, segsBefore, "expected segments to be deleted")

	// Retained entries are correct.
	ents, err := s.Entries(6, 11)
	require.NoError(t, err)
	require.Len(t, ents, 5)
	require.Equal(t, uint64(6), ents[0].Index)
	require.NoError(t, s.Close())

	// Reopen — verify retained entries survive.
	s2, err := NewDurableStorageWithOptions(dir, DurableStorageOptions{SegmentLimit: 50})
	require.NoError(t, err)
	defer func() { require.NoError(t, s2.Close()) }()

	_, err = s2.Entries(1, 6)
	require.ErrorIs(t, err, ErrCompacted)

	ents, err = s2.Entries(6, 11)
	require.NoError(t, err)
	require.Len(t, ents, 5)
}

func TestSegmentDeletionCrashSafe_037f(t *testing.T) {
	dir := t.TempDir()
	s, err := NewDurableStorageWithOptions(dir, DurableStorageOptions{
		SegmentLimit: 50,
	})
	require.NoError(t, err)

	// Write entries across segments.
	for i := uint64(1); i <= 10; i++ {
		require.NoError(t, s.Save(
			[]*raftpb.Entry{{Index: i, Term: 1, Data: []byte("data")}},
			&raftpb.HardState{Term: 1, CommittedIndex: i},
		))
	}

	// Persist snapshot boundary manually without deleting segments
	// (simulating crash between snapshot persist and segment deletion).
	snap := &raftpb.SnapshotMeta{LastIncludedIndex: 5, LastIncludedTerm: 1}
	buf := make([]byte, SnapshotMetaBytes)
	encodeSnapshotMeta(snap, buf)
	require.NoError(t, s.sw.truncate(0))
	require.NoError(t, s.sw.write(buf))
	require.NoError(t, s.sw.sync())
	require.NoError(t, s.Close())

	// Old segments still exist — this simulates a crash before deletion.
	// Reopen: replay should skip compacted entries.
	s2, err := NewDurableStorageWithOptions(dir, DurableStorageOptions{SegmentLimit: 50})
	require.NoError(t, err)
	defer func() { require.NoError(t, s2.Close()) }()

	_, err = s2.Entries(1, 6)
	require.ErrorIs(t, err, ErrCompacted)

	ents, err := s2.Entries(6, 11)
	require.NoError(t, err)
	require.Len(t, ents, 5)
	require.Equal(t, uint64(6), ents[0].Index)
}

func TestMaybeCompactSkipsBelowThreshold_037f(t *testing.T) {
	dir := t.TempDir()
	s, err := NewDurableStorageWithOptions(dir, DurableStorageOptions{
		CompactThreshold: 10,
	})
	require.NoError(t, err)
	defer func() { require.NoError(t, s.Close()) }()

	// Write 15 entries.
	for i := uint64(1); i <= 15; i++ {
		require.NoError(t, s.Save(
			[]*raftpb.Entry{{Index: i, Term: 1, Data: []byte("x")}},
			&raftpb.HardState{Term: 1, CommittedIndex: i},
		))
	}

	// Below threshold — no compaction.
	require.NoError(t, s.MaybeCompact(5))
	require.Equal(t, uint64(1), s.FirstIndex())

	// At threshold — compaction runs.
	require.NoError(t, s.MaybeCompact(10))
	require.Equal(t, uint64(11), s.FirstIndex())
}

func TestCompactPreservesEntriesAboveBoundary_037f(t *testing.T) {
	dir := t.TempDir()
	s, err := NewDurableStorage(dir)
	require.NoError(t, err)
	defer func() { require.NoError(t, s.Close()) }()

	entries := make([]*raftpb.Entry, 100)
	for i := range entries {
		entries[i] = &raftpb.Entry{Index: uint64(i + 1), Term: 1, Data: []byte("v")}
	}
	require.NoError(t, s.Save(entries, &raftpb.HardState{Term: 1, CommittedIndex: 100}))

	// compactLocked is internal; production compaction runs through MaybeCompact.
	s.mu.Lock()
	require.NoError(t, s.compactLocked(80))
	s.mu.Unlock()

	_, err = s.Entries(1, 81)
	require.ErrorIs(t, err, ErrCompacted)

	ents, err := s.Entries(81, 101)
	require.NoError(t, err)
	require.Len(t, ents, 20)
	require.Equal(t, uint64(81), ents[0].Index)
	require.Equal(t, uint64(100), ents[19].Index)
}

func TestMultiSegmentReplayAfterRestart_037f(t *testing.T) {
	dir := t.TempDir()
	s, err := NewDurableStorageWithOptions(dir, DurableStorageOptions{
		SegmentLimit: 50,
	})
	require.NoError(t, err)

	// Write enough to create 3+ segments.
	for i := uint64(1); i <= 20; i++ {
		require.NoError(t, s.Save(
			[]*raftpb.Entry{{Index: i, Term: 1, Data: []byte("data")}},
			&raftpb.HardState{Term: 1, CommittedIndex: i},
		))
	}
	require.GreaterOrEqual(t, s.rw.segmentCount(), 3)
	require.NoError(t, s.Close())

	// Reopen and verify all entries recovered.
	s2, err := NewDurableStorageWithOptions(dir, DurableStorageOptions{SegmentLimit: 50})
	require.NoError(t, err)
	defer func() { require.NoError(t, s2.Close()) }()

	require.Equal(t, uint64(1), s2.FirstIndex())
	require.Equal(t, uint64(20), s2.LastIndex())

	ents, err := s2.Entries(1, 21)
	require.NoError(t, err)
	require.Len(t, ents, 20)
	for i, e := range ents {
		require.Equal(t, uint64(i+1), e.Index)
	}
}

// 037g — The Replay
// ---------------------------------------------------------------------------

func TestCrashMidApplyRecoversEntries_037g(t *testing.T) {
	dir := t.TempDir()
	s, err := NewDurableStorageWithOptions(dir, DurableStorageOptions{CompactThreshold: 80})
	require.NoError(t, err)

	// Persist 100 entries with commitIndex = 100.
	entries := make([]*raftpb.Entry, 100)
	for i := range entries {
		entries[i] = &raftpb.Entry{Index: uint64(i + 1), Term: 1, Data: []byte{byte(i + 1)}}
	}
	require.NoError(t, s.Save(entries, &raftpb.HardState{Term: 1, CommittedIndex: 100}))
	require.NoError(t, s.Close())

	// First "restart": construct Raft with Applied=0, get all committed entries.
	s1, err := NewDurableStorageWithOptions(dir, DurableStorageOptions{CompactThreshold: 80})
	require.NoError(t, err)

	r1 := newRaft(Config{
		ID:            1,
		Storage:       s1,
		Applied:       0,
		ElectionTick:  10,
		HeartbeatTick: 1,
		Logger:        slog.Default(),
	})

	rd1 := r1.Ready()
	require.Len(t, rd1.CommittedEntries, 100)

	// Simulate applying only 95 entries to a fake state machine.
	applied := make(map[uint64][]byte)
	for _, e := range rd1.CommittedEntries[:95] {
		applied[e.Index] = e.Data
	}
	require.Len(t, applied, 95)

	// Compact at 80 — the server calls MaybeCompact after each apply batch.
	require.NoError(t, s1.MaybeCompact(80))
	snap, err := s1.Snapshot()
	require.NoError(t, err)
	require.Equal(t, uint64(80), snap.LastIncludedIndex)

	// "Crash": discard raft, close storage.
	require.NoError(t, s1.Close())

	// Second "restart": realistic server path — Applied from snapshot boundary.
	s2, err := NewDurableStorageWithOptions(dir, DurableStorageOptions{CompactThreshold: 80})
	require.NoError(t, err)
	defer func() { require.NoError(t, s2.Close()) }()

	snap2, err := s2.Snapshot()
	require.NoError(t, err)
	require.Equal(t, uint64(80), snap2.LastIncludedIndex)

	r2 := newRaft(Config{
		ID:            1,
		Storage:       s2,
		Applied:       snap2.LastIncludedIndex,
		ElectionTick:  10,
		HeartbeatTick: 1,
		Logger:        slog.Default(),
	})

	rd2 := r2.Ready()
	// Entries 1–80 are compacted. Applied=80 means replay 81–100.
	require.Len(t, rd2.CommittedEntries, 20)
	require.Equal(t, uint64(81), rd2.CommittedEntries[0].Index)
	require.Equal(t, uint64(100), rd2.CommittedEntries[19].Index)

	// Apply all replayed entries — gap from crash (96–100) is now filled.
	for _, e := range rd2.CommittedEntries {
		applied[e.Index] = e.Data
	}
	require.Len(t, applied, 100)
	for i := uint64(1); i <= 100; i++ {
		require.Equal(t, []byte{byte(i)}, applied[i])
	}
}
