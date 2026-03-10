package raft

import (
	"errors"
	"fmt"
	"io"
	"sync"
)

var ErrCompacted = errors.New("requested index is unavailable due to compaction")
var ErrNonContiguous = errors.New("non-contiguous entry index")

type SnapshotMeta struct {
	LastIncludedIndex uint64
	LastIncludedTerm  uint64
}

type Storage interface {
	InitialState() (HardState, error)
	Save(entries []Entry, hard HardState) error
	Entries(lo, hi uint64) ([]Entry, error)
	FirstIndex() uint64
	LastIndex() uint64
	Compact(index uint64) error
	Close() error
}

const logFilename = "replication.log"
const snapFilename = "snapshot.log"

type DurableStorage struct {
	rw      *wal
	sw      *wal
	mu      sync.RWMutex
	hd      HardState
	entries []Entry
	snap    SnapshotMeta
}

func NewDurableStorage(dir string) (*DurableStorage, error) {
	rw, err := newWAL(dir, logFilename)
	if err != nil {
		return nil, err
	}

	sw, err := newWAL(dir, snapFilename)
	if err != nil {
		return nil, err
	}

	d := &DurableStorage{
		rw:      rw,
		sw:      sw,
		entries: make([]Entry, 0),
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

func (s *DurableStorage) InitialState() (HardState, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.hd, nil
}

func computeTotalSize(entries []Entry) int {
	var total int
	for _, e := range entries {
		total += frameHeaderSize + e.Size()
	}
	return total
}

// Save appends `entries` and `hard` together as a single batch at the end of the log.
func (s *DurableStorage) Save(entries []Entry, hard HardState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.validateAppend(entries); err != nil {
		return err
	}

	buf := encodeSaveBatch(hard, entries)
	we := s.rw.write(buf)
	var se error
	if we == nil {
		se = s.rw.sync()
	}
	if err := errors.Join(we, se); err != nil {
		return err
	}

	s.hd = hard
	s.appendEntries(entries)
	return nil
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

func (s *DurableStorage) Entries(lo, hi uint64) ([]Entry, error) {
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
		return []Entry{}, nil
	}

	start := int(lo - firstIndex)
	end := int(hi - firstIndex)
	ents := make([]Entry, end-start)
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

func (s *DurableStorage) replayLog() (err error) {
	var position int64
	defer func() {
		if err == ErrTailCorruption {
			if truncErr := s.rw.truncate(position); truncErr != nil {
				e := fmt.Errorf("truncation failed after corruption: %w", truncErr)
				err = errors.Join(err, e)
				return
			}
			err = nil
		}
	}()

	s.entries = s.entries[:0]

	for {
		batch, err := s.rw.read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}

		hd, ents, err := decodeSaveBatch(batch)
		if err != nil {
			return err
		}
		ents = filterRetainedEntries(ents, s.snap.LastIncludedIndex)

		s.hd = hd
		if err := s.appendEntries(ents); err != nil {
			return err
		}
		position += int64(frameHeaderSize + len(batch))
	}

	return nil
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

	s.snap = SnapshotMeta{}

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

func (s *DurableStorage) appendEntries(entries []Entry) error {
	if len(entries) == 0 {
		return nil
	}
	if err := s.validateAppend(entries); err != nil {
		return err
	}
	s.entries = append(s.entries, entries...)
	return nil
}

func (s *DurableStorage) validateAppend(entries []Entry) error {
	if len(entries) == 0 {
		return nil
	}

	expected := entries[0].Index
	if len(s.entries) > 0 {
		expected = s.lastIndexWithoutLock() + 1
	} else if s.snap.LastIncludedIndex > 0 {
		expected = s.snap.LastIncludedIndex + 1
	}

	for _, entry := range entries {
		if entry.Index != expected {
			return fmt.Errorf("%w: got %d want %d", ErrNonContiguous, entry.Index, expected)
		}
		expected++
	}

	return nil
}

func (s *DurableStorage) Compact(index uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

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

	snap := SnapshotMeta{LastIncludedIndex: index, LastIncludedTerm: term}
	if err := s.persistSnapshotMeta(snap); err != nil {
		return err
	}

	cut := 0
	for cut < len(s.entries) && s.entries[cut].Index <= index {
		cut++
	}
	retained := append([]Entry(nil), s.entries[cut:]...)
	s.entries = retained
	s.snap = snap

	return nil
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

func (s *DurableStorage) persistSnapshotMeta(snap SnapshotMeta) error {
	buf := make([]byte, SnapshotMetaBytes)
	snap.EncodeTo(buf)
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

func filterRetainedEntries(entries []Entry, lastIncludedIndex uint64) []Entry {
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
