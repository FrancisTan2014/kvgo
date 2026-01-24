package core

import (
	"fmt"
	"time"
)

type groupCommitter struct {
	ticker *time.Ticker       // periodic flush trigger
	reqCh  chan *writeRequest // incoming Put() requests
	buf    []*writeRequest    // current batch (reused to reduce allocations/GC pressure)
	walBuf []byte             // reusable WAL encode buffer (reduces large short-lived allocs -> less GC)
	stopCh chan struct{}      // shutdown signal
	doneCh chan struct{}      // closed when committer exits
}

type writeRequest struct {
	key    string
	value  []byte
	respCh chan error
}

func (db *DB) startGroupCommitter() {
	defer close(db.committer.doneCh)

	for {
		select {
		case r := <-db.committer.reqCh:
			db.committer.buf = append(db.committer.buf, r)
			if len(db.committer.buf) >= cap(db.committer.buf) {
				// Flush on size to cap memory and maximize fsync amortization.
				db.flush(db.committer.buf)
				db.committer.buf = db.committer.buf[:0]
			}
		case <-db.committer.ticker.C:
			if len(db.committer.buf) > 0 {
				// Flush on timer to bound write latency.
				db.flush(db.committer.buf)
				db.committer.buf = db.committer.buf[:0]
			}
		case <-db.committer.stopCh:
			// Drain reqCh before exit so enqueued Put() callers always get an ack.
			for {
				select {
				case r := <-db.committer.reqCh:
					db.committer.buf = append(db.committer.buf, r)
					if len(db.committer.buf) >= cap(db.committer.buf) {
						db.flush(db.committer.buf)
						db.committer.buf = db.committer.buf[:0]
					}
				default:
					if len(db.committer.buf) > 0 {
						db.flush(db.committer.buf)
						db.committer.buf = db.committer.buf[:0]
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
	for _, r := range batch {
		// Apply to memory only after WAL fsync succeeds ("strict" durability for acks).
		db.putInternal(r.key, r.value)
		r.respCh <- nil
	}
}
