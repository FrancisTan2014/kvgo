package main

import (
	"fmt"
	"kvgo/core"
	"math/rand"
	"testing"
)

// The Logic: This runs the "Sabotage" scenario on ANY implementation
func runConcurrentBenchmark(b *testing.B, db KVStore) {
	b.ResetTimer()

	// Simulate high concurrency
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			key := fmt.Sprintf("key-%d", i)
			// Small value to focus on Lock Contention, not memcpy
			db.Put(key, []byte("x"))
			i++
		}
	})
}

// Mixed read/write benchmark with random access pattern
func runMixedWorkload(b *testing.B, db KVStore, readPct int) {
	// Pre-populate with 10K keys
	const numKeys = 10000
	keys := make([]string, numKeys)
	for i := 0; i < numKeys; i++ {
		keys[i] = fmt.Sprintf("key-%d", i)
		db.Put(keys[i], []byte("initial-value"))
	}

	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		// Each goroutine gets its own RNG to avoid contention on rand
		rng := rand.New(rand.NewSource(rand.Int63()))

		for pb.Next() {
			key := keys[rng.Intn(numKeys)]

			if rng.Intn(100) < readPct {
				// Read
				db.Get(key)
			} else {
				// Write
				db.Put(key, []byte("updated-value"))
			}
		}
	})
}

// Sub-benchmarks for side-by-side comparison
func BenchmarkDB(b *testing.B) {
	implementations := map[string]func() KVStore{
		"Simple":  func() KVStore { return core.NewSimpleDB() },
		"Sharded": func() KVStore { return core.NewShardedDB() },
	}

	for name, newDB := range implementations {
		b.Run(name, func(b *testing.B) {
			db := newDB()
			runConcurrentBenchmark(b, db)
		})
	}
}

// Extreme scenario: mixed reads/writes with random keys
func BenchmarkMixed(b *testing.B) {
	implementations := map[string]func() KVStore{
		"Simple":  func() KVStore { return core.NewSimpleDB() },
		"Sharded": func() KVStore { return core.NewShardedDB() },
	}

	// Test different read/write ratios
	ratios := []struct {
		name    string
		readPct int
	}{
		{"Write100", 0}, // 100% writes
		{"Read50", 50},  // 50% reads, 50% writes
		{"Read90", 90},  // 90% reads, 10% writes (typical cache)
		{"Read99", 99},  // 99% reads (heavy read)
	}

	for _, ratio := range ratios {
		b.Run(ratio.name, func(b *testing.B) {
			for name, newDB := range implementations {
				b.Run(name, func(b *testing.B) {
					db := newDB()
					runMixedWorkload(b, db, ratio.readPct)
				})
			}
		})
	}
}
