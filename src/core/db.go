package core

import (
	"errors"
	"io"
	"sync"
	"time"
)

const numShards = 256      // lock striping: reduce contention vs 1 global mutex
const syncBatchSize = 100  // group commit: amortize fsync across many writes
const syncPeriodInMs = 100 // latency bound: force a flush at least this often
const walRecordHeaderBytes = 8

var ErrClosed = errors.New("db is closed")

type DB struct {
	shards    []*shard
	wal       *wal
	committer *groupCommitter
	stateMu   sync.RWMutex // shutdown gate: prevents Put/Close race (no enqueue after drain)
	closed    bool         // protected by stateMu

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
		db.putInternal(string(key), value)
		return nil
	})
	if err != nil && err != io.EOF {
		return nil, err
	}

	db.committer = &groupCommitter{
		ticker: time.NewTicker(syncPeriodInMs * time.Millisecond), // pacing for periodic flushes
		reqCh:  make(chan *writeRequest, syncBatchSize*2),
		buf:    make([]*writeRequest, 0, syncBatchSize),
		walBuf: nil,
		stopCh: make(chan struct{}),
		doneCh: make(chan struct{}),
	}

	go db.startGroupCommitter()

	return db, nil
}

func (db *DB) Put(key string, value []byte) error {
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

// putInternal writes to memory only (used by replay and Put)
func (db *DB) putInternal(key string, value []byte) {
	s := db.getShard(key)
	s.mu.Lock()
	s.data[key] = value
	s.mu.Unlock()
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
