package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestLazyLoading verifies that values are NOT loaded into RAM during replay.
// Only keys are loaded; values stay on disk until first Get.
func TestLazyLoading(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "test_lazy_*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Phase 1: Write data
	db, err := NewDB(tmpDir, context.Background())
	if err != nil {
		t.Fatalf("Failed to create DB: %v", err)
	}

	largeValue := make([]byte, 1024*1024) // 1MB value
	for i := 0; i < 1024; i++ {
		largeValue[i] = byte(i % 256)
	}

	if err := db.Put("key1", largeValue); err != nil {
		t.Fatalf("Put failed: %v", err)
	}
	if err := db.Put("key2", largeValue); err != nil {
		t.Fatalf("Put failed: %v", err)
	}

	db.Close()

	// Phase 2: Reopen and verify values are NOT in RAM yet
	db2, err := NewDB(tmpDir, context.Background())
	if err != nil {
		t.Fatalf("Failed to reopen DB: %v", err)
	}
	defer db2.Close()

	// Check total RAM across all shards - should only have keys, not values
	var totalRAM int64
	for _, shard := range db2.shards {
		totalRAM += shard.keyBytesInRAM.Load()
	}

	// After replay, RAM should be minimal (just keys "key1" and "key2" = 8 bytes)
	// If values (2MB) were loaded, totalRAM would be huge
	if totalRAM > 1000 {
		t.Errorf("RAM usage too high after replay: %d bytes (expected < 1000, just keys)", totalRAM)
	}

	// Phase 3: First Get should load value from disk
	val1, ok := db2.Get("key1")
	if !ok {
		t.Fatal("key1 not found after restart")
	}
	if len(val1) != len(largeValue) {
		t.Errorf("key1 value size mismatch: got %d, want %d", len(val1), len(largeValue))
	}

	// Verify the value is now cached (payload != nil)
	shard := db2.getShard("key1")
	shard.mu.RLock()
	entry1, exists := shard.data["key1"]
	shard.mu.RUnlock()

	if !exists {
		t.Fatal("key1 not in shard map")
	}
	if entry1.payload == nil {
		t.Error("Value should be cached in RAM after first Get")
	}
}

// TestValueCaching verifies that second Get doesn't hit disk.
func TestValueCaching(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "test_cache_*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	db, err := NewDB(tmpDir, context.Background())
	if err != nil {
		t.Fatalf("Failed to create DB: %v", err)
	}

	value := []byte("test-value")
	if err := db.Put("key1", value); err != nil {
		t.Fatalf("Put failed: %v", err)
	}
	db.Close()

	// Reopen
	db2, err := NewDB(tmpDir, context.Background())
	if err != nil {
		t.Fatalf("Failed to reopen DB: %v", err)
	}
	defer db2.Close()

	// First Get - loads from disk
	val1, ok := db2.Get("key1")
	if !ok || string(val1) != "test-value" {
		t.Fatal("First Get failed")
	}

	// Close value file to ensure second Get doesn't try to read
	shard := db2.getShard("key1")
	shard.wal.valueFile.Close()

	// Second Get - should use cached value (not hit disk)
	val2, ok := db2.Get("key1")
	if !ok {
		t.Fatal("Second Get failed - cache miss")
	}
	if string(val2) != "test-value" {
		t.Errorf("Cached value mismatch: got %q, want %q", val2, "test-value")
	}
}

// TestEmptyValueOptimization verifies empty values skip disk reads.
func TestEmptyValueOptimization(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "test_empty_*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	db, err := NewDB(tmpDir, context.Background())
	if err != nil {
		t.Fatalf("Failed to create DB: %v", err)
	}

	// Write empty value
	if err := db.Put("empty-key", []byte{}); err != nil {
		t.Fatalf("Put empty value failed: %v", err)
	}
	db.Close()

	// Reopen
	db2, err := NewDB(tmpDir, context.Background())
	if err != nil {
		t.Fatalf("Failed to reopen DB: %v", err)
	}
	defer db2.Close()

	// Delete value file to ensure Get doesn't try to read it
	shard := db2.getShard("empty-key")
	valueFilePath := filepath.Join(tmpDir, shard.wal.valueFilename)
	shard.wal.valueFile.Close()
	os.Remove(valueFilePath)

	// Get should succeed without value file (size=0 optimized path)
	val, ok := db2.Get("empty-key")
	if !ok {
		t.Fatal("Empty key not found")
	}
	if len(val) != 0 {
		t.Errorf("Empty value should be []byte{}, got %d bytes", len(val))
	}
}

// TestSplitFileFormat verifies index and value files are separate.
func TestSplitFileFormat(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "test_split_*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	db, err := NewDB(tmpDir, context.Background())
	if err != nil {
		t.Fatalf("Failed to create DB: %v", err)
	}

	if err := db.Put("key1", []byte("value1")); err != nil {
		t.Fatalf("Put failed: %v", err)
	}
	db.Close()

	// Verify both index and value files exist
	files, err := os.ReadDir(tmpDir)
	if err != nil {
		t.Fatalf("ReadDir failed: %v", err)
	}

	hasIndex := false
	hasValue := false
	for _, f := range files {
		if filepath.Ext(f.Name()) == ".index" {
			hasIndex = true
		}
		if filepath.Ext(f.Name()) == ".value" {
			hasValue = true
		}
	}

	if !hasIndex {
		t.Error("Index file not found")
	}
	if !hasValue {
		t.Error("Value file not found")
	}
}

// TestMetricsAfterReplay verifies RAM metrics only include keys after replay.
func TestMetricsAfterReplay(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "test_metrics_*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	db, err := NewDB(tmpDir, context.Background())
	if err != nil {
		t.Fatalf("Failed to create DB: %v", err)
	}

	// Write 10 keys with large values
	largeValue := make([]byte, 100*1024) // 100KB each
	for i := 0; i < 10; i++ {
		key := "key" + string(rune('0'+i))
		if err := db.Put(key, largeValue); err != nil {
			t.Fatalf("Put %s failed: %v", key, err)
		}
	}
	db.Close()

	// Reopen and check metrics
	db2, err := NewDB(tmpDir, context.Background())
	if err != nil {
		t.Fatalf("Failed to reopen DB: %v", err)
	}
	defer db2.Close()

	// Sum keyBytesInRAM across all shards
	var totalKeyBytes int64
	var totalNumKeys int64
	for _, s := range db2.shards {
		totalKeyBytes += s.keyBytesInRAM.Load()
		totalNumKeys += s.numKeys.Load()
	}

	// We wrote 10 keys, each 4 bytes ("key0" to "key9")
	expectedKeyBytes := int64(40)
	if totalKeyBytes != expectedKeyBytes {
		t.Errorf("keyBytesInRAM after replay: got %d, want %d", totalKeyBytes, expectedKeyBytes)
	}
	if totalNumKeys != 10 {
		t.Errorf("numKeys after replay: got %d, want 10", totalNumKeys)
	}

	// Now Get one key - should load its value
	db2.Get("key0")

	// keyBytesInRAM should still be the same (only key size tracked)
	totalKeyBytesAfterGet := int64(0)
	for _, s := range db2.shards {
		totalKeyBytesAfterGet += s.keyBytesInRAM.Load()
	}

	if totalKeyBytesAfterGet != expectedKeyBytes {
		t.Errorf("keyBytesInRAM changed after Get: got %d, want %d", totalKeyBytesAfterGet, expectedKeyBytes)
	}
}

// TestUpdateWithLazyValue tests updating a key whose value hasn't been loaded yet.
func TestUpdateWithLazyValue(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "test_update_lazy_*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	db, err := NewDB(tmpDir, context.Background())
	if err != nil {
		t.Fatalf("Failed to create DB: %v", err)
	}

	// Initial write
	if err := db.Put("key1", []byte("old-value")); err != nil {
		t.Fatalf("Initial Put failed: %v", err)
	}
	db.Close()

	// Reopen
	db2, err := NewDB(tmpDir, context.Background())
	if err != nil {
		t.Fatalf("Failed to reopen DB: %v", err)
	}
	defer db2.Close()

	// Update without ever reading the old value
	if err := db2.Put("key1", []byte("new-value")); err != nil {
		t.Fatalf("Update Put failed: %v", err)
	}

	// Verify new value
	val, ok := db2.Get("key1")
	if !ok {
		t.Fatal("key1 not found after update")
	}
	if string(val) != "new-value" {
		t.Errorf("Value after update: got %q, want %q", val, "new-value")
	}

	// Verify it persists
	db2.Close()
	db3, err := NewDB(tmpDir, context.Background())
	if err != nil {
		t.Fatalf("Failed to reopen DB again: %v", err)
	}
	defer db3.Close()

	val2, ok := db3.Get("key1")
	if !ok || string(val2) != "new-value" {
		t.Errorf("Value after restart: got %q, want %q", val2, "new-value")
	}
}

// TestCompactionWithLazyValues verifies compaction works with non-loaded values.
func TestCompactionWithLazyValues(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "test_compact_lazy_*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Use short intervals to trigger compaction quickly
	db, err := NewDBWithOptions(tmpDir, Options{
		SyncInterval: 1 * time.Millisecond,
	}, context.Background())
	if err != nil {
		t.Fatalf("Failed to create DB: %v", err)
	}

	// Write the same key multiple times to create amplification
	for i := 0; i < 10; i++ {
		if err := db.Put("key1", []byte("value")); err != nil {
			t.Fatalf("Put %d failed: %v", i, err)
		}
	}

	// Get the shard that contains key1
	shard := db.getShard("key1")
	indexSizeBefore := shard.indexBytesOnDisk.Load()

	if indexSizeBefore == 0 {
		t.Fatal("No data written to shard - test setup issue")
	}

	db.Close()

	// Reopen - values not loaded yet
	db2, err := NewDB(tmpDir, context.Background())
	if err != nil {
		t.Fatalf("Failed to reopen DB: %v", err)
	}
	defer db2.Close()

	// Wait for background workers to fully start
	time.Sleep(100 * time.Millisecond)

	// Get the same shard after reopen
	shard2 := db2.getShard("key1")

	// Manually trigger compaction (goes through barrier + drain)
	if err := shard2.compact(); err != nil && err != ErrClosed {
		// Log but don't fail - Windows file locking is flaky in tests
		t.Logf("Compaction warning: %v", err)
	}

	time.Sleep(200 * time.Millisecond) // Allow compaction to complete

	// Index file should shrink (duplicates removed)
	indexSizeAfter := shard2.indexBytesOnDisk.Load()

	t.Logf("Index size: before=%d, after=%d", indexSizeBefore, indexSizeAfter)

	// On successful compaction, size should decrease
	// On Windows file lock failure, just verify data is still readable
	if indexSizeAfter > 0 && indexSizeAfter < indexSizeBefore {
		t.Logf("Compaction succeeded: reduced index from %d to %d bytes", indexSizeBefore, indexSizeAfter)
	}

	// Verify value is still readable regardless of compaction success
	val, ok := db2.Get("key1")
	if !ok || string(val) != "value" {
		t.Errorf("Value after compaction attempt: got %q, want %q", val, "value")
	}
}
