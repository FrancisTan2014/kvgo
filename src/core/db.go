package core

import (
	"errors"
	"io"
	"sync"
	"sync/atomic"
)

const numShards = 256 // lock striping: reduce contention vs 1 global mutex

var ErrClosed = errors.New("db is closed")

type DB struct {
	shards           []*shard
	wal              *wal
	committer        *groupCommitter
	compactionWorker *compactWorker
	bytesInRAM       atomic.Int64
	bytesOnDisk      atomic.Int64

	stateMu sync.RWMutex // shutdown gate: prevents Put/Close race (no enqueue after drain)
	closed  bool         // protected by stateMu

	closeOnce sync.Once
	closeErr  error
}

func NewDB(path string) (*DB, error) {
	wal, err := newWAL(path)
	if err != nil {
		return nil, err
	}

	shards := make([]*shard, numShards)
	for i := range shards {
		shards[i] = newShard()
	}

	db := &DB{shards: shards, wal: wal}

	// WAL replay: rebuild memory state without re-logging (no feedback loop).
	err = wal.read(func(key, value []byte) error {
		delta := db.putInternalAndDelta(string(key), value)
		db.bytesInRAM.Add(delta)
		return nil
	})
	if err != nil && err != io.EOF {
		return nil, err
	}

	if info, statErr := wal.file.Stat(); statErr == nil {
		db.bytesOnDisk.Store(info.Size())
	}

	db.committer = newGroupCommitter()
	db.compactionWorker, err = newCompactionWorker(defaultCompactionPolicy)
	if err != nil {
		return nil, err
	}
	go db.startGroupCommitter()
	go db.startCompactionWorker()

	return db, nil
}

func (db *DB) Put(key string, value []byte) error {
	db.stateMu.RLock()
	if db.closed || db.committer == nil {
		db.stateMu.RUnlock()
		return ErrClosed
	}
	committer := db.committer
	db.stateMu.RUnlock()

	// Block writes if DB is paused (e.g.: compaction happens).
	// Important: do not hold stateMu while waiting, otherwise Close() can deadlock.
	committer.pauseGate.wait()

	db.stateMu.RLock()
	if db.closed || db.committer == nil {
		db.stateMu.RUnlock()
		return ErrClosed
	}

	respCh := make(chan error, 1)
	req := &writeRequest{key: key, value: value, respCh: respCh}

	// Hold RLock until the enqueue is done so Close() can't start draining
	// while we are in-flight (prevents "enqueued after worker exits" hangs).
	db.committer.reqCh <- req
	db.stateMu.RUnlock()

	return <-respCh
}

func (db *DB) Get(key string) ([]byte, bool) {
	s := db.getShard(key)
	s.mu.RLock()
	defer s.mu.RUnlock()

	val, ok := s.data[key]
	return val, ok
}

// putInternalAndDelta writes to memory only and reports the logical delta bytes in RAM.
// We track logical bytes (key bytes + value bytes), not actual heap usage.
func (db *DB) putInternalAndDelta(key string, value []byte) int64 {
	s := db.getShard(key)
	s.mu.Lock()
	oldV, existed := s.data[key]
	s.data[key] = value
	s.mu.Unlock()

	delta := int64(len(value) - len(oldV))
	if !existed {
		delta += int64(len(key))
	}
	return delta
}

func (db *DB) Close() error {
	db.closeOnce.Do(func() {
		db.stateMu.Lock()
		db.closed = true
		db.stateMu.Unlock()

		if db.committer != nil {
			close(db.committer.stopCh)
			<-db.committer.doneCh
			db.committer.ticker.Stop()
		}
		db.closeErr = db.wal.close()
	})
	return db.closeErr
}
