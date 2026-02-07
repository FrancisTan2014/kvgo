package engine

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

type valueEntry struct {
	payload   []byte
	size      int
	offset    int64
	requested bool // indicator for background loader to load value from the disk
}

type shard struct {
	data   map[string]*valueEntry
	mu     sync.RWMutex
	ctx    context.Context
	cancel context.CancelFunc

	wal              *wal
	committer        *groupCommitter
	compactionWorker *compactWorker
	keyBytesInRAM    atomic.Int64 // for compaction amplification calculation
	numKeys          atomic.Int64 // number of unique keys
	indexBytesOnDisk atomic.Int64
	valueOffset      atomic.Int64
}

func newShard(path string, index int, syncInterval time.Duration, ctx context.Context) (*shard, error) {
	indexFile := fmt.Sprintf("shar%d.index", index)
	valueFile := fmt.Sprintf("shar%d.value", index)

	w, err := newWAL(path, indexFile, valueFile)
	if err != nil {
		return nil, err
	}

	committer := newGroupCommitter(syncInterval)
	compactionWorker, err := newCompactionWorker(defaultCompactionPolicy)
	if err != nil {
		return nil, err
	}

	s := &shard{
		data:             make(map[string]*valueEntry),
		wal:              w,
		committer:        committer,
		compactionWorker: compactionWorker,
	}

	if f, err := s.wal.valueFile.Stat(); err != nil {
		return nil, err
	} else {
		s.valueOffset.Store(f.Size())
	}

	s.ctx, s.cancel = context.WithCancel(ctx)

	return s, nil
}

func (s *shard) startBackgroundWorker() {
	go s.startGroupCommitter()
	go s.startCompactionWorker()
}

func (s *shard) stopBackgroundWorker() error {
	close(s.committer.stopCh)
	<-s.committer.doneCh
	s.committer.ticker.Stop()

	if err := s.wal.close(); err != nil {
		return err
	}

	return nil
}

func (s *shard) replay() error {
	err := s.wal.readIndex(func(ie indexEntry) error {
		s.data[ie.Key] = &valueEntry{
			size:   ie.ValueSize,
			offset: ie.ValueOffset,
		}
		return nil
	})

	if err != nil {
		return err
	}

	// After replay, count unique keys and their sizes
	var keysInRAM int64
	for key := range s.data {
		keysInRAM += int64(len(key))
	}

	s.keyBytesInRAM.Store(keysInRAM)
	s.numKeys.Store(int64(len(s.data)))
	return nil
}

func (s *shard) get(key string) ([]byte, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	ve, exists := s.data[key]
	if !exists {
		return nil, false, nil
	}

	val, err := s.getValue(ve)
	if err != nil {
		return nil, false, err
	}

	return val, true, nil
}

func (s *shard) getValue(ve *valueEntry) ([]byte, error) {
	if ve.payload != nil {
		return ve.payload, nil
	}

	if ve.size == 0 {
		ve.payload = make([]byte, 0)
		return ve.payload, nil
	}

	val, err := s.wal.readValue(ve.offset, ve.size)
	if err != nil {
		return nil, err
	}

	ve.payload = val

	return val, nil
}

func (s *shard) put(key string, value []byte) error {
	if value == nil {
		value = make([]byte, 0)
	}

	select {
	case <-s.ctx.Done(): // reject writes on shutting down
		return s.ctx.Err()
	default:
	}

	respCh := make(chan error, 1)
	req := &writeRequest{key: key, value: value, respCh: respCh}

	// Avoid holding stateMu while potentially blocking.
	// If shutdown has started, stopCh is closed and we fail fast.
	select {
	case s.committer.reqCh <- req:
		// ok
	case <-s.committer.stopCh:
		return ErrClosed
	}

	return <-respCh
}

func (s *shard) putInternal(key string, value []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, exists := s.data[key]

	if !exists {
		s.keyBytesInRAM.Add(int64(len(key)))
		s.numKeys.Add(1)
	}

	s.data[key] = &valueEntry{
		payload: value,
		size:    len(value),
	}
}

func (s *shard) clear() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Close current WAL files and remove them
	if err := s.wal.clear(); err != nil {
		return err
	}

	// Reset metrics
	s.keyBytesInRAM.Store(0)
	s.numKeys.Store(0)
	s.indexBytesOnDisk.Store(0)
	s.valueOffset.Store(0)

	// Clear in-memory map
	s.data = make(map[string]*valueEntry)

	// Recreate WAL files
	w, err := newWAL(s.wal.path, s.wal.indexFilename, s.wal.valueFilename)
	if err != nil {
		return err
	}
	s.wal = w

	return nil
}

// forEach calls fn sequentially for each key-value pair in the database.
// If fn returns false, iteration stops.
func (s *shard) forEach(fn func(string, []byte) bool) error {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for k, v := range s.data {
		val, err := s.getValue(v)
		if err != nil {
			return err
		}
		if !fn(k, val) {
			return nil
		}
	}

	return nil
}
