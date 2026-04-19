package raft

import (
	"errors"
	"fmt"
	"io"
	"kvgo/raftpb"
	"sync"
)

var ErrCompacted = errors.New("requested index is unavailable due to compaction")
var ErrNonContiguous = errors.New("non-contiguous raftpb.Entry index")
var ErrSnapOutOfDate = errors.New("requested snapshot is older than existing snapshot")
var ErrUnavailable = errors.New("requested index is unavailable")

type Storage interface {
	InitialState() (*raftpb.HardState, error)
	Save(entries []*raftpb.Entry, hard *raftpb.HardState) error
	Entries(lo, hi uint64) ([]*raftpb.Entry, error)
	FirstIndex() uint64
	LastIndex() uint64
	Close() error
	Snapshot() (*raftpb.SnapshotMeta, error)
	ApplySnapshot(snap *raftpb.SnapshotMeta) error
	Term(i uint64) (uint64, error)
}

const snapFilename = "snapshot.log"
const DefaultCompactThreshold = uint64(10000)

type DurableStorageOptions struct {
	SegmentLimit     int64
	CompactThreshold uint64
}

type DurableStorage struct {
	rw               *segmentedWAL
	sw               *wal
	mu               sync.RWMutex
	hd               *raftpb.HardState
	entries          []*raftpb.Entry
	snap             *raftpb.SnapshotMeta
	segLastIdx       map[string]uint64
	compactThreshold uint64
}

func NewDurableStorage(dir string) (*DurableStorage, error) {
	return NewDurableStorageWithOptions(dir, DurableStorageOptions{})
}

func NewDurableStorageWithOptions(dir string, opts DurableStorageOptions) (*DurableStorage, error) {
	threshold := opts.CompactThreshold
	if threshold == 0 {
		threshold = DefaultCompactThreshold
	}

	rw, err := newSegmentedWAL(dir, opts.SegmentLimit)
	if err != nil {
		return nil, err
	}

	sw, err := newWAL(dir, snapFilename)
	if err != nil {
		_ = rw.close()
		return nil, err
	}

	d := &DurableStorage{
		rw:               rw,
		sw:               sw,
		hd:               &raftpb.HardState{},
		entries:          make([]*raftpb.Entry, 0),
		snap:             &raftpb.SnapshotMeta{},
		segLastIdx:       make(map[string]uint64),
		compactThreshold: threshold,
	}

	if err := d.replay(); err != nil {
		e := errors.Join(rw.close(), sw.close())
		return nil, errors.Join(err, e)
	}

	return d, nil
}

func (s *DurableStorage) Close() error {
	return errors.Join(s.rw.close(), s.sw.close())
}

func (s *DurableStorage) InitialState() (*raftpb.HardState, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.hd, nil
}

func computeTotalSize(entries []*raftpb.Entry) int {
	var total int
	for _, e := range entries {
		total += frameHeaderSize + entrySize(e)
	}
	return total
}

// Save appends `entries` and `hard` together as a single batch at the end of the log.
// Entries may overlap with existing entries (log repair after truncation).
func (s *DurableStorage) Save(entries []*raftpb.Entry, hard *raftpb.HardState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(entries) == 0 && IsEmptyHardState(hard) {
		return nil
	}
	effectiveHard := hard
	if IsEmptyHardState(effectiveHard) {
		effectiveHard = s.hd
	}

	buf := encodeSaveBatch(effectiveHard, entries)
	we := s.rw.write(buf)
	var se error
	if we == nil {
		se = s.rw.sync()
	}
	if err := errors.Join(we, se); err != nil {
		return err
	}

	if len(entries) > 0 {
		s.segLastIdx[s.rw.currentSegment()] = entries[len(entries)-1].Index
	}

	if err := s.rw.maybeCut(); err != nil {
		return err
	}

	s.hd = effectiveHard
	return s.truncateAndAppend(entries)
}

func (s *DurableStorage) firstIndexWithoutLock() uint64 {
	if len(s.entries) > 0 {
		return s.entries[0].Index
	}
	if s.snap.LastIncludedIndex == 0 {
		return 0
	}
	return s.snap.LastIncludedIndex + 1
}

func (s *DurableStorage) lastIndexWithoutLock() uint64 {
	size := len(s.entries)
	if size > 0 {
		return s.entries[size-1].Index
	}
	if s.snap.LastIncludedIndex == 0 {
		return 0
	}
	return s.snap.LastIncludedIndex
}

func (s *DurableStorage) FirstIndex() uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.firstIndexWithoutLock()
}

func (s *DurableStorage) LastIndex() uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.lastIndexWithoutLock()
}

func (s *DurableStorage) Entries(lo, hi uint64) ([]*raftpb.Entry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if lo > hi {
		return nil, fmt.Errorf("invalid range: [%d,%d)", lo, hi)
	}
	if s.snap.LastIncludedIndex > 0 && lo <= s.snap.LastIncludedIndex {
		return nil, ErrCompacted
	}

	firstIndex := s.firstIndexWithoutLock()
	lastIndex := s.lastIndexWithoutLock()

	availableLo := firstIndex
	availableHi := firstIndex
	if len(s.entries) > 0 {
		availableHi = lastIndex + 1
	}

	if lo < availableLo || hi > availableHi {
		return nil, fmt.Errorf("requested range [%d,%d) is not inside [%d, %d)",
			lo, hi, availableLo, availableHi)
	}

	if lo == hi {
		return []*raftpb.Entry{}, nil
	}

	start := int(lo - firstIndex)
	end := int(hi - firstIndex)
	ents := make([]*raftpb.Entry, end-start)
	copy(ents, s.entries[start:end])
	return ents, nil
}

func (s *DurableStorage) replay() (err error) {
	if err := s.replaySnapshotMeta(); err != nil {
		return err
	}
	if err := s.replayLog(); err != nil {
		return err
	}
	return nil
}

func (s *DurableStorage) replayLog() error {
	s.entries = s.entries[:0]

	return s.rw.readAll(func(seg string, batch []byte) error {
		hd, ents, err := decodeSaveBatch(batch)
		if err != nil {
			return err
		}

		if len(ents) > 0 {
			s.segLastIdx[seg] = ents[len(ents)-1].Index
		}

		ents = filterRetainedEntries(ents, s.snap.LastIncludedIndex)

		s.hd = hd
		return s.truncateAndAppend(ents)
	})
}

func (s *DurableStorage) replaySnapshotMeta() (err error) {
	var position int64
	defer func() {
		if err == ErrTailCorruption {
			if truncErr := s.sw.truncate(position); truncErr != nil {
				e := fmt.Errorf("truncation failed after corruption: %w", truncErr)
				err = errors.Join(err, e)
				return
			}
			err = nil
		}
	}()

	s.snap = &raftpb.SnapshotMeta{}

	for {
		batch, err := s.sw.read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}

		snap, err := DecodeSnapshotMeta(batch)
		if err != nil {
			return ErrCorruption
		}
		s.snap = snap
		position += int64(frameHeaderSize + len(batch))
	}

	return nil
}

// truncateAndAppend handles overlapping entries by truncating the in-memory
// log at the overlap point before appending. This is needed because the WAL
// is append-only: a leader truncation writes replacement entries as a new
// batch, and on replay those entries overlap with previously replayed ones.
func (s *DurableStorage) truncateAndAppend(entries []*raftpb.Entry) error {
	if len(entries) == 0 {
		return nil
	}
	if len(s.entries) == 0 {
		// With no in-memory entries, validate against snapshot boundary.
		expected := uint64(1)
		if s.snap.LastIncludedIndex > 0 {
			expected = s.snap.LastIncludedIndex + 1
		}
		if entries[0].Index != expected {
			return fmt.Errorf("%w: got %d want %d", ErrNonContiguous, entries[0].Index, expected)
		}
		s.entries = append(s.entries, entries...)
		return nil
	}
	first := entries[0].Index
	last := s.lastIndexWithoutLock()
	switch {
	case first == last+1:
		// Normal contiguous append.
		s.entries = append(s.entries, entries...)
	case first > last+1:
		// Gap — entries skip ahead, which is never valid.
		return fmt.Errorf("%w: got %d want %d", ErrNonContiguous, first, last+1)
	case first <= s.firstIndexWithoutLock():
		// Complete replacement — the leader's log is authoritative after truncation.
		s.entries = make([]*raftpb.Entry, len(entries))
		copy(s.entries, entries)
	default:
		// Partial overlap — keep entries before the overlap point.
		keep := first - s.firstIndexWithoutLock()
		s.entries = append(s.entries[:keep], entries...)
	}
	return nil
}

func (s *DurableStorage) compactLocked(index uint64) error {
	if s.snap.LastIncludedIndex > 0 && index <= s.snap.LastIncludedIndex {
		return ErrCompacted
	}
	if len(s.entries) == 0 {
		return fmt.Errorf("compact index %d is out of range", index)
	}
	if index > s.lastIndexWithoutLock() {
		return fmt.Errorf("compact index %d is out of range", index)
	}

	term, err := s.termWithoutLock(index)
	if err != nil {
		return err
	}

	snap := &raftpb.SnapshotMeta{LastIncludedIndex: index, LastIncludedTerm: term}
	if err := s.persistSnapshotMeta(snap); err != nil {
		return err
	}

	cut := 0
	for cut < len(s.entries) && s.entries[cut].Index <= index {
		cut++
	}
	retained := append([]*raftpb.Entry(nil), s.entries[cut:]...)
	s.entries = retained
	s.snap = snap

	return s.deleteOldSegments()
}

func (s *DurableStorage) MaybeCompact(appliedIndex uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if appliedIndex <= s.snap.LastIncludedIndex {
		return nil
	}
	if appliedIndex-s.snap.LastIncludedIndex < s.compactThreshold {
		return nil
	}
	return s.compactLocked(appliedIndex)
}

// deleteOldSegments removes sealed WAL segments whose entries have all been
// compacted (i.e. every entry index ≤ the snapshot boundary). Segments are
// walked oldest-first and we stop at the first segment that still holds
// entries above the boundary — later segments are guaranteed to be higher.
func (s *DurableStorage) deleteOldSegments() error {
	boundary := s.snap.LastIncludedIndex
	canDelete := make(map[string]bool)
	for _, name := range s.rw.segments {
		// Never delete the segment currently being written to.
		if name == s.rw.currentSegment() {
			break
		}
		lastIdx, ok := s.segLastIdx[name]
		if !ok {
			// Empty segment (e.g. created by cut() then crashed before
			// any writes). Safe to remove — it contains no entries.
			canDelete[name] = true
			continue
		}
		// This segment has entries above the compaction boundary, so it
		// (and all later segments) must be retained.
		if lastIdx > boundary {
			break
		}
		canDelete[name] = true
	}
	if len(canDelete) == 0 {
		return nil
	}
	for name := range canDelete {
		delete(s.segLastIdx, name)
	}
	return s.rw.deleteSegments(func(name string) bool {
		return canDelete[name]
	})
}

func (s *DurableStorage) termWithoutLock(index uint64) (uint64, error) {
	if s.snap.LastIncludedIndex > 0 && index == s.snap.LastIncludedIndex {
		return s.snap.LastIncludedTerm, nil
	}
	firstIndex := s.firstIndexWithoutLock()
	lastIndex := s.lastIndexWithoutLock()
	if len(s.entries) == 0 || index < firstIndex || index > lastIndex {
		return 0, fmt.Errorf("requested index %d is not available", index)
	}
	return s.entries[index-firstIndex].Term, nil
}

func (s *DurableStorage) persistSnapshotMeta(snap *raftpb.SnapshotMeta) error {
	buf := make([]byte, SnapshotMetaBytes)
	encodeSnapshotMeta(snap, buf)
	if err := s.sw.truncate(0); err != nil {
		return err
	}
	we := s.sw.write(buf)
	var se error
	if we == nil {
		se = s.sw.sync()
	}
	return errors.Join(we, se)
}

func filterRetainedEntries(entries []*raftpb.Entry, lastIncludedIndex uint64) []*raftpb.Entry {
	if lastIncludedIndex == 0 {
		return entries
	}
	start := 0
	for start < len(entries) && entries[start].Index <= lastIncludedIndex {
		start++
	}
	if start == 0 {
		return entries
	}
	if start == len(entries) {
		return nil
	}
	return entries[start:]
}

func (s *DurableStorage) Snapshot() (*raftpb.SnapshotMeta, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.snap, nil
}

func (s *DurableStorage) ApplySnapshot(snap *raftpb.SnapshotMeta) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.snap.LastIncludedIndex > 0 && snap.LastIncludedIndex <= s.snap.LastIncludedIndex {
		return ErrSnapOutOfDate
	}

	if err := s.persistSnapshotMeta(snap); err != nil {
		return err
	}

	s.snap = snap
	s.entries = filterRetainedEntries(s.entries, snap.LastIncludedIndex)
	return nil
}

func (s *DurableStorage) Term(i uint64) (uint64, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if i < s.snap.LastIncludedIndex {
		return 0, ErrCompacted
	}
	if i > s.LastIndex() {
		return 0, ErrUnavailable
	}

	if i == s.snap.LastIncludedIndex {
		return s.snap.LastIncludedTerm, nil
	}

	return s.entries[i-s.firstIndexWithoutLock()].Term, nil
}
