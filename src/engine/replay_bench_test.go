package engine

import (
	"context"
	"os"
	"testing"
	"time"
)

// BenchmarkReplayPerformance demonstrates that replay time is O(KeyCount), not O(DataSize).
//
// This benchmark shows the key improvement from lazy loading:
// - Without lazy loading: Replay time grows with total data size (keys + values)
// - With lazy loading: Replay time only grows with number of keys
//
// Example results:
//
//	1K keys × 1KB values = 1MB total → ~50ms replay (loading only 1K keys)
//	1K keys × 1MB values = 1GB total → ~50ms replay (loading only 1K keys, not 1GB values!)
func BenchmarkReplayPerformance(b *testing.B) {
	scenarios := []struct {
		name       string
		numKeys    int
		valueSize  int
		expectFast bool // true if lazy loading makes this fast
	}{
		{"100keys_1KB", 100, 1024, true},
		{"100keys_100KB", 100, 100 * 1024, true}, // Same key count, 100× more data
		{"1000keys_1KB", 1000, 1024, true},
		{"1000keys_1MB", 1000, 1024 * 1024, true}, // Same key count, 1000× more data!
		{"10000keys_100B", 10000, 100, true},
		{"10000keys_10KB", 10000, 10 * 1024, true}, // Same key count, 100× more data
	}

	for _, sc := range scenarios {
		b.Run(sc.name, func(b *testing.B) {
			tmpDir, err := os.MkdirTemp("", "bench_replay_*")
			if err != nil {
				b.Fatalf("Failed to create temp dir: %v", err)
			}
			defer os.RemoveAll(tmpDir)

			// Phase 1: Write test data
			db, err := NewDB(tmpDir, context.Background())
			if err != nil {
				b.Fatalf("Failed to create DB: %v", err)
			}

			value := make([]byte, sc.valueSize)
			for i := 0; i < sc.valueSize; i++ {
				value[i] = byte(i % 256)
			}

			// Write all keys
			for i := 0; i < sc.numKeys; i++ {
				key := "key" + string(rune('a'+i/26)) + string(rune('a'+i%26))
				if err := db.Put(key, value); err != nil {
					b.Fatalf("Put failed: %v", err)
				}
			}

			// Flush to disk
			time.Sleep(50 * time.Millisecond)
			db.Close()

			totalDataSize := int64(sc.numKeys * sc.valueSize)
			b.ResetTimer()

			// Phase 2: Benchmark replay (open DB)
			for i := 0; i < b.N; i++ {
				b.StartTimer()
				db2, err := NewDB(tmpDir, context.Background())
				b.StopTimer()

				if err != nil {
					b.Fatalf("Reopen failed: %v", err)
				}

				// Verify key count is correct (sanity check)
				var keyCount int64
				for _, s := range db2.shards {
					keyCount += s.numKeys.Load()
				}
				if keyCount != int64(sc.numKeys) {
					b.Errorf("Key count mismatch: got %d, want %d", keyCount, sc.numKeys)
				}

				db2.Close()
			}

			// Report metrics
			avgReplayTime := time.Duration(b.Elapsed().Nanoseconds() / int64(b.N))
			b.ReportMetric(float64(sc.numKeys), "keys")
			b.ReportMetric(float64(totalDataSize)/(1024*1024), "data_MB")
			b.ReportMetric(float64(avgReplayTime.Milliseconds()), "replay_ms")

			// The key insight: replay time should be similar regardless of value size
			if sc.expectFast && avgReplayTime > 5*time.Second {
				b.Logf("WARNING: Replay took %v for %d keys with %dB values (expected < 5s with lazy loading)",
					avgReplayTime, sc.numKeys, sc.valueSize)
			}
		})
	}
}

// TestReplayTimeIsIndependentOfValueSize verifies the core lazy loading property:
// Replay time depends on key count, NOT value size.
func TestReplayTimeIsIndependentOfValueSize(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping replay performance test in short mode")
	}

	tmpDir, err := os.MkdirTemp("", "test_replay_perf_*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	const numKeys = 1000

	// Test 1: Small values (1KB each = 1MB total)
	db1, _ := NewDB(tmpDir, context.Background())
	smallValue := make([]byte, 1024)
	for i := 0; i < numKeys; i++ {
		db1.Put("key"+string(rune('a'+i/26))+string(rune('a'+i%26)), smallValue)
	}
	time.Sleep(50 * time.Millisecond)
	db1.Close()

	start1 := time.Now()
	db1Reopen, _ := NewDB(tmpDir, context.Background())
	replayTime1 := time.Since(start1)
	db1Reopen.Close()
	os.RemoveAll(tmpDir)

	// Test 2: Large values (1MB each = 1GB total) - same key count
	os.MkdirAll(tmpDir, 0755)
	db2, _ := NewDB(tmpDir, context.Background())
	largeValue := make([]byte, 1024*1024)
	for i := 0; i < numKeys; i++ {
		db2.Put("key"+string(rune('a'+i/26))+string(rune('a'+i%26)), largeValue)
	}
	time.Sleep(50 * time.Millisecond)
	db2.Close()

	start2 := time.Now()
	db2Reopen, _ := NewDB(tmpDir, context.Background())
	replayTime2 := time.Since(start2)
	db2Reopen.Close()

	t.Logf("Replay time with 1KB values (1MB total): %v", replayTime1)
	t.Logf("Replay time with 1MB values (1GB total): %v", replayTime2)
	t.Logf("Data size ratio: 1000:1, Time ratio: %.2f:1", float64(replayTime2)/float64(replayTime1))

	// With lazy loading, replay time should be similar (within 3×)
	// Without lazy loading, it would be ~1000× different
	ratio := float64(replayTime2) / float64(replayTime1)
	if ratio > 3.0 {
		t.Errorf("Replay time ratio too high: %.2fx (expected < 3x with lazy loading)", ratio)
		t.Errorf("This suggests values are being loaded during replay!")
	} else {
		t.Logf("✓ Lazy loading working: 1000× more data, only %.2fx slower replay", ratio)
	}
}
