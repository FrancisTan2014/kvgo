package engine

import (
	"sync/atomic"
	"time"
)

// Stats is a lightweight snapshot of DB behavior.
//
// It is intended for observability (benchmarks, debugging) rather than strict
// accounting. Values are monotonically increasing counters or best-effort maxes.
type Stats struct {
	BytesInRAM  int64
	BytesOnDisk int64

	Flushes       uint64
	FlushOps      uint64
	FlushBytes    uint64
	FlushMaxOps   uint64
	FlushTotalDur time.Duration
	FlushMaxDur   time.Duration
}

type dbMetrics struct {
	flushes       atomic.Uint64
	flushOps      atomic.Uint64
	flushBytes    atomic.Uint64
	flushTotalDur atomic.Uint64 // nanos
	flushMaxOps   atomic.Uint64
	flushMaxDur   atomic.Uint64 // nanos
}

func (m *dbMetrics) observeFlush(ops int, bytes int, dur time.Duration) {
	m.flushes.Add(1)
	m.flushOps.Add(uint64(ops))
	m.flushBytes.Add(uint64(bytes))
	m.flushTotalDur.Add(uint64(dur.Nanoseconds()))
	updateMaxU64(&m.flushMaxOps, uint64(ops))
	updateMaxU64(&m.flushMaxDur, uint64(dur.Nanoseconds()))
}

func updateMaxU64(dst *atomic.Uint64, v uint64) {
	for {
		cur := dst.Load()
		if v <= cur {
			return
		}
		if dst.CompareAndSwap(cur, v) {
			return
		}
	}
}
