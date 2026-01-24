package core

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"os"
	"sync"
	"time"
)

const numShards = 256      // lock striping: reduce contention vs 1 global mutex
const syncBatchSize = 100  // group commit: amortize fsync across many writes
const syncPeriodInMs = 100 // latency bound: force a flush at least this often
const walRecordHeaderBytes = 8

var ErrClosed = errors.New("db is closed")

type DB struct {
	shards  []*shard
	wal     *wal
	worker  *worker
	stateMu sync.RWMutex // shutdown gate: prevents Put/Close race (no enqueue after drain)
	closed  bool         // protected by stateMu

	closeOnce sync.Once
	closeErr  error
}

type shard struct {
	data map[string][]byte
	mu   sync.RWMutex
}

type worker struct {
	ticker *time.Ticker       // periodic flush trigger
	reqCh  chan *writeRequest // incoming Put() requests
	buf    []*writeRequest    // current batch (reused to reduce allocations/GC pressure)
	walBuf []byte             // reusable WAL encode buffer (reduces large short-lived allocs -> less GC)
	stopCh chan struct{}      // shutdown signal
	doneCh chan struct{}      // closed when worker exits
}

type writeRequest struct {
	key    string
	value  []byte
	respCh chan error
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

	db.worker = &worker{
		ticker: time.NewTicker(syncPeriodInMs * time.Millisecond), // pacing for periodic flushes
		reqCh:  make(chan *writeRequest, syncBatchSize*2),
		buf:    make([]*writeRequest, 0, syncBatchSize),
		walBuf: nil,
		stopCh: make(chan struct{}),
		doneCh: make(chan struct{}),
	}

	go db.startWorker()

	return db, nil
}

func (db *DB) Put(key string, value []byte) error {
	db.stateMu.RLock()
	if db.closed || db.worker == nil {
		db.stateMu.RUnlock()
		return ErrClosed
	}

	respCh := make(chan error, 1)
	req := &writeRequest{key: key, value: value, respCh: respCh}

	// Hold RLock until the enqueue is done so Close() can't start draining
	// while we are in-flight (prevents "enqueued after worker exits" hangs).
	db.worker.reqCh <- req
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

func (db *DB) startWorker() {
	defer close(db.worker.doneCh)

	for {
		select {
		case r := <-db.worker.reqCh:
			db.worker.buf = append(db.worker.buf, r)
			if len(db.worker.buf) >= cap(db.worker.buf) {
				// Flush on size to cap memory and maximize fsync amortization.
				db.flush(db.worker.buf)
				db.worker.buf = db.worker.buf[:0]
			}
		case <-db.worker.ticker.C:
			if len(db.worker.buf) > 0 {
				// Flush on timer to bound write latency.
				db.flush(db.worker.buf)
				db.worker.buf = db.worker.buf[:0]
			}
		case <-db.worker.stopCh:
			// Drain reqCh before exit so enqueued Put() callers always get an ack.
			for {
				select {
				case r := <-db.worker.reqCh:
					db.worker.buf = append(db.worker.buf, r)
					if len(db.worker.buf) >= cap(db.worker.buf) {
						db.flush(db.worker.buf)
						db.worker.buf = db.worker.buf[:0]
					}
				default:
					if len(db.worker.buf) > 0 {
						db.flush(db.worker.buf)
						db.worker.buf = db.worker.buf[:0]
					}
					return
				}
			}
		}
	}
}

func (db *DB) flush(batch []*writeRequest) {
	// Compute encoded size first so we can write a single contiguous WAL record buffer.
	totalBytes := 0
	for _, r := range batch {
		totalBytes += walRecordLen(len(r.key), len(r.value))
	}

	// Reuse a single encode buffer to avoid large short-lived allocations (GC pressure).
	buf := db.worker.walBuf
	if cap(buf) < totalBytes {
		buf = make([]byte, totalBytes)
		db.worker.walBuf = buf
	} else {
		buf = buf[:totalBytes]
	}

	offset := 0
	for _, r := range batch {
		offset += encodeInto(buf[offset:], r.key, r.value)
	}

	err := db.wal.writeAndSync(buf)
	if err != nil {
		// In a critical system, failing to fsync means we cannot meet durability guarantees.
		// Prefer crash-fast (restart + alert) over continuing in an unknown state.
		for _, r := range batch {
			r.respCh <- err
		}
		panic(fmt.Sprintf("wal write+sync failed: %v", err))
	}
	for _, r := range batch {
		// Apply to memory only after WAL fsync succeeds ("strict" durability for acks).
		db.putInternal(r.key, r.value)
		r.respCh <- nil
	}
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

		if db.worker != nil {
			close(db.worker.stopCh)
			<-db.worker.doneCh
			db.worker.ticker.Stop()
		}
		db.closeErr = db.wal.close()
	})
	return db.closeErr
}

func encodeInto(dst []byte, key string, value []byte) int {
	klen := len(key)
	vlen := len(value)
	binary.LittleEndian.PutUint32(dst[0:4], uint32(klen))
	binary.LittleEndian.PutUint32(dst[4:8], uint32(vlen))
	copy(dst[walRecordHeaderBytes:walRecordHeaderBytes+klen], key)
	copy(dst[walRecordHeaderBytes+klen:walRecordHeaderBytes+klen+vlen], value)
	return walRecordLen(klen, vlen)
}

func walRecordLen(keyLen int, valueLen int) int {
	return walRecordHeaderBytes + keyLen + valueLen
}

func newShard() *shard {
	return &shard{
		data: make(map[string][]byte),
	}
}

func (db *DB) getShard(key string) *shard {
	return db.shards[hash(key)%numShards]
}

func hash(s string) uint32 {
	h := fnv.New32a()
	h.Write([]byte(s))
	return h.Sum32()
}

// wal is a private write-ahead log implementation.
// It is intentionally unexported so external packages can't rely on it.
type wal struct {
	file *os.File
}

func newWAL(path string) (*wal, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_RDWR, 0644)
	if file == nil {
		return nil, err
	}

	return &wal{file: file}, nil
}

func (w *wal) writeAndSync(buf []byte) error {
	_, err := w.file.Write(buf)
	if err != nil {
		return err
	}

	return w.file.Sync()
}

func (w *wal) close() error {
	return w.file.Close()
}

func (w *wal) read(handler func([]byte, []byte) error) error {
	for {
		var header [walRecordHeaderBytes]byte
		_, err := io.ReadFull(w.file, header[:])
		if err == io.ErrUnexpectedEOF {
			fmt.Printf("Warning: WAL corrupted at end, truncating.\n")
			return io.EOF
		}
		if err != nil {
			return err
		}

		klen := binary.LittleEndian.Uint32(header[0:4])
		vlen := binary.LittleEndian.Uint32(header[4:8])

		buf := make([]byte, int(klen+vlen))
		_, err = io.ReadFull(w.file, buf)
		if err == io.ErrUnexpectedEOF {
			// Ignore trailing partial record instead of failing startup.
			fmt.Printf("Warning: WAL corrupted at end, truncating.\n")
			return io.EOF
		}
		if err != nil {
			return err
		}

		err = handler(buf[:klen], buf[klen:])
		if err != nil {
			return err
		}
	}
}
