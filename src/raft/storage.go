package raft

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"
)

type Storage interface {
	InitialState() (HardState, error)
	Save(entries []Entry, hard HardState) error
	Entries(lo, hi uint64) ([]Entry, error)
	Close() error
}

const filename = "replication.log"

type DurableStorage struct {
	w          *wal
	mu         sync.RWMutex
	firstIndex uint64
	lastIndex  uint64
	hd         HardState
	entries    []Entry
}

func NewDurableStorage(dir string) (*DurableStorage, error) {
	w, err := newWAL(dir, filename)
	if err != nil {
		return nil, err
	}

	d := &DurableStorage{
		w:       w,
		entries: make([]Entry, 0),
	}

	if err := d.replay(); err != nil {
		e := w.close()
		return nil, errors.Join(err, e)
	}

	return d, nil
}

func (s *DurableStorage) Close() error {
	return s.w.close()
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

	entrySize := computeTotalSize(entries)
	// Batch layout: [hard state][entry bytes length][entry frame][entry data]...
	batchSize := entrySize + HardStateBytes + frameHeaderSize
	buf := make([]byte, batchSize)
	hard.EncodeTo(buf)
	binary.LittleEndian.PutUint32(buf[HardStateBytes:], uint32(entrySize))

	idx := HardStateBytes + frameHeaderSize
	for _, e := range entries {
		size := e.Size()
		binary.LittleEndian.PutUint32(buf[idx:], uint32(size))
		idx += 4
		e.EncodeTo(buf[idx:])
		idx += size
	}

	we := s.w.write(buf)
	fe := s.w.sync()
	if err := errors.Join(we, fe); err != nil {
		return err
	}

	s.hd = hard
	s.appendEntries(entries)
	return nil
}

func (s *DurableStorage) Entries(lo, hi uint64) ([]Entry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if lo > hi {
		return nil, fmt.Errorf("invalid range: [%d,%d)", lo, hi)
	}

	availableLo := s.firstIndex
	availableHi := s.firstIndex
	if len(s.entries) > 0 {
		availableHi = s.lastIndex + 1
	}

	if lo < availableLo || hi > availableHi {
		return nil, fmt.Errorf("requested range [%d,%d) is not inside [%d, %d)",
			lo, hi, availableLo, availableHi)
	}

	if lo == hi {
		return []Entry{}, nil
	}

	start := int(lo - s.firstIndex)
	end := int(hi - s.firstIndex)
	ents := make([]Entry, end-start)
	copy(ents, s.entries[start:end])
	return ents, nil
}

func (s *DurableStorage) replay() (err error) {
	var position int64
	defer func() {
		if err == ErrTailCorruption {
			if truncErr := s.w.truncate(position); truncErr != nil {
				e := fmt.Errorf("truncation failed after corruption: %w", truncErr)
				err = errors.Join(err, e)
				return
			}
			err = nil
		}
	}()

	s.entries = s.entries[:0]
	s.firstIndex = 0
	s.lastIndex = 0

	for {
		batch, err := s.w.read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}

		hd, ents, err := decodeBatch(batch)
		if err != nil {
			return err
		}

		s.hd = hd
		if err := s.appendEntries(ents); err != nil {
			return err
		}
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
	if len(s.entries) == 0 {
		s.firstIndex = entries[0].Index
	}
	s.entries = append(s.entries, entries...)
	s.lastIndex = s.entries[len(s.entries)-1].Index
	return nil
}

func (s *DurableStorage) validateAppend(entries []Entry) error {
	if len(entries) == 0 {
		return nil
	}

	expected := entries[0].Index
	if len(s.entries) > 0 {
		expected = s.lastIndex + 1
	}

	for _, entry := range entries {
		if entry.Index != expected {
			return fmt.Errorf("non-contiguous entry index: got %d want %d", entry.Index, expected)
		}
		expected++
	}

	return nil
}

func decodeBatch(batch []byte) (HardState, []Entry, error) {
	var hd HardState

	if len(batch) < HardStateBytes {
		return hd, nil, ErrCorruption
	}
	hd, _ = DecodeHardState(batch[0:HardStateBytes])

	batch = batch[HardStateBytes:]
	if len(batch) < 4 {
		return hd, nil, ErrCorruption
	}

	entryBytes := int(binary.LittleEndian.Uint32(batch))
	batch = batch[4:]
	if len(batch) != entryBytes {
		return hd, nil, ErrCorruption
	}

	ents, err := decodeEntries(batch)
	if err != nil {
		return hd, nil, ErrCorruption
	}

	return hd, ents, nil
}

func decodeEntries(data []byte) ([]Entry, error) {
	entries := make([]Entry, 0)
	idx := 0
	for {
		buf := data[idx:]
		if len(buf) == 0 {
			break
		}
		if len(buf) < frameHeaderSize {
			return nil, ErrCorruption
		}

		frame := int(binary.LittleEndian.Uint32(buf[0:frameHeaderSize]))
		idx += frameHeaderSize
		buf = buf[idx:]
		if len(buf) < frame {
			return nil, ErrCorruption
		}

		e, err := DecodeEntry(buf[:frame])
		if err != nil {
			return nil, ErrCorruption
		}

		entries = append(entries, e)
		idx += frame
	}

	return entries, nil
}
