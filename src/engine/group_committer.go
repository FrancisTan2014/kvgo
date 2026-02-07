package engine

import (
	"fmt"
	"time"
)

const syncBatchSize = 100 // group commit: amortize fsync across many writes

// DefaultSyncInterval is the default latency bound for group commits.
// Lower values = lower latency but more fsyncs (slower throughput).
// Higher values = better throughput but higher tail latency.
const DefaultSyncInterval = 10 * time.Millisecond

type groupCommitter struct {
	ticker      *time.Ticker       // periodic flush trigger
	reqCh       chan *writeRequest // incoming Put() requests
	buf         []*writeRequest    // current batch (reused to reduce allocations/GC pressure)
	walIndexBuf []byte             // reusable WAL encode buffer (reduces large short-lived allocs -> less GC)
	walValueBuf []byte             // reusable WAL encode buffer (reduces large short-lived allocs -> less GC)
	stopCh      chan struct{}      // shutdown signal
	doneCh      chan struct{}      // closed when committer exits
	compactCh   chan *compactRequest
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

func newGroupCommitter(syncInterval time.Duration) *groupCommitter {
	if syncInterval <= 0 {
		syncInterval = DefaultSyncInterval
	}
	return &groupCommitter{
		ticker:    time.NewTicker(syncInterval), // pacing for periodic flushes
		reqCh:     make(chan *writeRequest, syncBatchSize*2),
		buf:       make([]*writeRequest, 0, syncBatchSize),
		stopCh:    make(chan struct{}),
		doneCh:    make(chan struct{}),
		compactCh: make(chan *compactRequest),
	}
}

func (s *shard) startGroupCommitter() {
	defer func() {
		if r := recover(); r != nil {
			// Critical failure (WAL write): log and signal Server for graceful shutdown
			if s.logger != nil {
				s.logger.Error("FATAL: WAL write failed",
					"panic", r,
					"index_file", s.wal.indexFilename,
					"value_file", s.wal.valueFilename,
					"action", "signaling_server_for_shutdown",
				)
			}
			// Signal fatal error to Server (close channel once)
			select {
			case <-s.fatalErrCh: // already closed
			default:
				close(s.fatalErrCh)
			}
		}
		close(s.committer.doneCh)
	}()

	appendToBatch := func(r *writeRequest) {
		s.committer.buf = append(s.committer.buf, r)
		if len(s.committer.buf) >= cap(s.committer.buf) {
			// Flush on size to cap memory and maximize fsync amortization.
			s.flush(s.committer.buf)
			s.committer.buf = s.committer.buf[:0]
		}
	}

	flushBuffered := func() {
		if len(s.committer.buf) > 0 {
			// Flush on timer/shutdown to bound write latency and ensure acks.
			s.flush(s.committer.buf)
			s.committer.buf = s.committer.buf[:0]
		}
	}

	drainReqCh := func() {
		// Drain reqCh so enqueued Put() callers always get an ack.
		for {
			select {
			case r := <-s.committer.reqCh:
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
			case r := <-s.committer.reqCh:
				appendToBatch(r)
			case cr := <-s.committer.compactCh:
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
		case r := <-s.committer.reqCh:
			appendToBatch(r)
		case <-s.committer.ticker.C:
			flushBuffered()
		case cr := <-s.committer.compactCh:
			// Barrier: drain everything already enqueued before compaction proceeds.
			drainReqCh()
			err := cr.doCompact()
			cr.err <- err
		case <-s.committer.stopCh:
			drainAllForShutdown()
			return
		}
	}
}

func (s *shard) flush(batch []*writeRequest) {
	// Compute encoded size first so we can write a single contiguous WAL record buffer.
	idxTotal := 0
	valTotal := 0
	for _, r := range batch {
		idxTotal += walIndexLen(len(r.key))
		valTotal += len(r.value)
	}

	// Reuse a single encode buffer to avoid large short-lived allocations (GC pressure).
	idxBuf := s.reuseIdxBuf(idxTotal)
	valBuf := s.reuseValBuf(valTotal)

	idxBufOff := 0
	valBufOff := 0
	valueFileOff := s.valueOffset.Load()
	for _, r := range batch {
		idxSize := encodeInto(idxBuf[idxBufOff:], r.key, len(r.value), valueFileOff)
		idxBufOff += idxSize

		copy(valBuf[valBufOff:], r.value)
		valBufOff += len(r.value)
		valueFileOff += int64(len(r.value))
	}

	if err := s.wal.writeValue(valBuf); err != nil {
		// Failing to fsync means we cannot guarantee durability.
		// Panic propagates to Server layer for logging and graceful shutdown.
		for _, r := range batch {
			r.respCh <- fmt.Errorf("WAL write value failed: %w", err)
		}
		panic(fmt.Sprintf("wal write value failed: %v filename=%s", err, s.wal.valueFilename))
	}

	if err := s.wal.writeIndex(idxBuf); err != nil {
		// Index write failed after value write succeeded.
		// Panic propagates to Server layer for logging and graceful shutdown.
		for _, r := range batch {
			r.respCh <- fmt.Errorf("WAL write index failed: %w", err)
		}
		panic(fmt.Sprintf("wal write index failed: %v filename=%s", err, s.wal.indexFilename))
	}

	s.indexBytesOnDisk.Add(int64(len(idxBuf)))
	s.valueOffset.Store(valueFileOff)

	for _, r := range batch {
		// Apply to memory only after WAL fsync succeeds ("strict" durability for acks).
		s.putInternal(r.key, r.value)
		r.respCh <- nil
	}
}

func (s *shard) reuseIdxBuf(size int) []byte {
	buf := s.committer.walIndexBuf
	if cap(buf) < size {
		buf = make([]byte, size)
		s.committer.walIndexBuf = buf
	} else {
		buf = buf[:size]
	}

	return buf
}

func (s *shard) reuseValBuf(size int) []byte {
	buf := s.committer.walValueBuf
	if cap(buf) < size {
		buf = make([]byte, size)
		s.committer.walValueBuf = buf
	} else {
		buf = buf[:size]
	}

	return buf
}
