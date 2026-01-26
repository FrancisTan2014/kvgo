package engine

import (
	"fmt"
	"time"
)

const syncBatchSize = 100                   // group commit: amortize fsync across many writes
const syncDuration = 100 * time.Millisecond // latency bound: force a flush at least this often

type groupCommitter struct {
	ticker    *time.Ticker       // periodic flush trigger
	reqCh     chan *writeRequest // incoming Put() requests
	buf       []*writeRequest    // current batch (reused to reduce allocations/GC pressure)
	walBuf    []byte             // reusable WAL encode buffer (reduces large short-lived allocs -> less GC)
	stopCh    chan struct{}      // shutdown signal
	doneCh    chan struct{}      // closed when committer exits
	compactCh chan *compactRequest
}

type compactRequest struct {
	doCompact func() error
	err       chan error
}

type writeRequest struct {
	key    string
	value  []byte
	respCh chan error
}

func newGroupCommitter() *groupCommitter {
	return &groupCommitter{
		ticker:    time.NewTicker(syncDuration), // pacing for periodic flushes
		reqCh:     make(chan *writeRequest, syncBatchSize*2),
		buf:       make([]*writeRequest, 0, syncBatchSize),
		walBuf:    nil,
		stopCh:    make(chan struct{}),
		doneCh:    make(chan struct{}),
		compactCh: make(chan *compactRequest),
	}
}

func (db *DB) startGroupCommitter() {
	defer close(db.committer.doneCh)

	appendToBatch := func(r *writeRequest) {
		db.committer.buf = append(db.committer.buf, r)
		if len(db.committer.buf) >= cap(db.committer.buf) {
			// Flush on size to cap memory and maximize fsync amortization.
			db.flush(db.committer.buf)
			db.committer.buf = db.committer.buf[:0]
		}
	}

	flushBuffered := func() {
		if len(db.committer.buf) > 0 {
			// Flush on timer/shutdown to bound write latency and ensure acks.
			db.flush(db.committer.buf)
			db.committer.buf = db.committer.buf[:0]
		}
	}

	drainReqCh := func() {
		// Drain reqCh so enqueued Put() callers always get an ack.
		for {
			select {
			case r := <-db.committer.reqCh:
				appendToBatch(r)
			default:
				flushBuffered()
				return
			}
		}
	}

	drainAllForShutdown := func() {
		// Drain writes and also unblock any in-flight compaction requests.
		for {
			select {
			case r := <-db.committer.reqCh:
				appendToBatch(r)
			case cr := <-db.committer.compactCh:
				// On shutdown, don't start expensive compaction work.
				cr.err <- ErrClosed
			default:
				flushBuffered()
				return
			}
		}
	}

	for {
		select {
		case r := <-db.committer.reqCh:
			appendToBatch(r)
		case <-db.committer.ticker.C:
			flushBuffered()
		case cr := <-db.committer.compactCh:
			// Barrier: drain everything already enqueued before compaction proceeds.
			drainReqCh()
			err := cr.doCompact()
			cr.err <- err
		case <-db.committer.stopCh:
			drainAllForShutdown()
			return
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
	buf := db.committer.walBuf
	if cap(buf) < totalBytes {
		buf = make([]byte, totalBytes)
		db.committer.walBuf = buf
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

	db.bytesOnDisk.Add(int64(len(buf)))

	var ramDelta int64
	for _, r := range batch {
		// Apply to memory only after WAL fsync succeeds ("strict" durability for acks).
		ramDelta += db.putInternalAndDelta(r.key, r.value)
		r.respCh <- nil
	}
	if ramDelta != 0 {
		db.bytesInRAM.Add(ramDelta)
	}
}
