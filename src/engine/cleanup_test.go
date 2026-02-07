package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCleanup(t *testing.T) {
	t.Run("removes orphaned data", func(t *testing.T) {
		t.Parallel()
		dataDir := t.TempDir()
		db, err := NewDB(dataDir, context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()

		// Write initial data
		for i := 0; i < 100; i++ {
			key := "key" + string(rune(i))
			value := make([]byte, 1024) // 1KB values
			if err := db.Put(key, value); err != nil {
				t.Fatal(err)
			}
		}

		// Get initial value file sizes across all shards
		getTotalValueSize := func() int64 {
			var total int64
			for _, shard := range db.shards {
				valueFilePath := filepath.Join(shard.wal.path, shard.wal.valueFilename)
				stat, err := os.Stat(valueFilePath)
				if err != nil {
					continue
				}
				total += stat.Size()
			}
			return total
		}

		initialSize := getTotalValueSize()

		// Overwrite all keys (creates orphans)
		for i := 0; i < 100; i++ {
			key := "key" + string(rune(i))
			value := make([]byte, 512) // Smaller values
			if err := db.Put(key, value); err != nil {
				t.Fatal(err)
			}
		}

		// Value file should be larger due to orphaned data
		beforeCleanup := getTotalValueSize()
		if beforeCleanup <= initialSize {
			t.Fatalf("expected value file to grow with orphaned data: initial=%d, before_cleanup=%d",
				initialSize, beforeCleanup)
		}

		// Run cleanup
		if err := db.Clean(); err != nil {
			t.Fatalf("cleanup failed: %v", err)
		}

		// Value file should be smaller after cleanup
		afterCleanup := getTotalValueSize()
		if afterCleanup >= beforeCleanup {
			t.Fatalf("cleanup did not reduce file size: before=%d, after=%d",
				beforeCleanup, afterCleanup)
		}

		t.Logf("Value file size: initial=%d, before_cleanup=%d, after_cleanup=%d",
			initialSize, beforeCleanup, afterCleanup)
	})

	t.Run("preserves all live data", func(t *testing.T) {
		t.Parallel()
		dataDir := t.TempDir()
		db, err := NewDB(dataDir, context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()

		// Write test data
		testData := make(map[string][]byte)
		for i := 0; i < 50; i++ {
			key := "preserve-" + string(rune(i))
			value := []byte("value-" + string(rune(i)))
			testData[key] = value
			if err := db.Put(key, value); err != nil {
				t.Fatal(err)
			}
		}

		// Overwrite some keys to create orphans
		for i := 0; i < 20; i++ {
			key := "preserve-" + string(rune(i))
			value := []byte("updated-" + string(rune(i)))
			testData[key] = value
			if err := db.Put(key, value); err != nil {
				t.Fatal(err)
			}
		}

		// Run cleanup
		if err := db.Clean(); err != nil {
			t.Fatalf("cleanup failed: %v", err)
		}

		// Verify all data is still accessible with correct values
		for keyStr, expectedValue := range testData {
			actualValue, ok := db.Get(keyStr)
			if !ok {
				t.Fatalf("failed to get key %q after cleanup", keyStr)
			}
			if string(actualValue) != string(expectedValue) {
				t.Fatalf("key %q: expected %q, got %q", keyStr, expectedValue, actualValue)
			}
		}
	})

	t.Run("handles empty database", func(t *testing.T) {
		t.Parallel()
		dataDir := t.TempDir()
		db, err := NewDB(dataDir, context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()

		// Run cleanup on empty database
		if err := db.Clean(); err != nil {
			t.Fatalf("cleanup failed on empty db: %v", err)
		}
	})

	t.Run("maintains consistency after cleanup", func(t *testing.T) {
		t.Parallel()
		dataDir := t.TempDir()
		db, err := NewDB(dataDir, context.Background())
		if err != nil {
			t.Fatal(err)
		}

		// Write data
		for i := 0; i < 100; i++ {
			key := "consistency-" + string(rune(i))
			value := []byte("value-" + string(rune(i)))
			if err := db.Put(key, value); err != nil {
				t.Fatal(err)
			}
		}

		// Create orphans
		for i := 0; i < 50; i++ {
			key := "consistency-" + string(rune(i))
			value := []byte("updated-" + string(rune(i)))
			if err := db.Put(key, value); err != nil {
				t.Fatal(err)
			}
		}

		// Cleanup
		if err := db.Clean(); err != nil {
			t.Fatal(err)
		}

		// Close and reopen
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}

		db, err = NewDB(dataDir, context.Background())
		if err != nil {
			t.Fatalf("failed to reopen after cleanup: %v", err)
		}
		defer db.Close()

		// Verify all data is correct after reopen
		for i := 0; i < 50; i++ {
			key := "consistency-" + string(rune(i))
			expected := []byte("updated-" + string(rune(i)))
			actual, ok := db.Get(key)
			if !ok {
				t.Fatalf("failed to get key %q after reopen", key)
			}
			if string(actual) != string(expected) {
				t.Fatalf("key %q: expected %q, got %q", key, expected, actual)
			}
		}

		for i := 50; i < 100; i++ {
			key := "consistency-" + string(rune(i))
			expected := []byte("value-" + string(rune(i)))
			actual, ok := db.Get(key)
			if !ok {
				t.Fatalf("failed to get key %q after reopen", key)
			}
			if string(actual) != string(expected) {
				t.Fatalf("key %q: expected %q, got %q", key, expected, actual)
			}
		}
	})

	t.Run("works with large values", func(t *testing.T) {
		t.Parallel()
		dataDir := t.TempDir()
		db, err := NewDB(dataDir, context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()

		// Write large values
		largeValue := make([]byte, 1024*1024) // 1MB
		for i := 0; i < 10; i++ {
			key := "large-" + string(rune(i))
			if err := db.Put(key, largeValue); err != nil {
				t.Fatal(err)
			}
		}

		// Overwrite to create orphans
		smallValue := make([]byte, 1024) // 1KB
		for i := 0; i < 10; i++ {
			key := "large-" + string(rune(i))
			if err := db.Put(key, smallValue); err != nil {
				t.Fatal(err)
			}
		}

		// Cleanup should significantly reduce file size
		if err := db.Clean(); err != nil {
			t.Fatal(err)
		}

		// Verify data is still correct
		for i := 0; i < 10; i++ {
			key := "large-" + string(rune(i))
			value, ok := db.Get(key)
			if !ok {
				t.Fatal("key not found after cleanup")
			}
			if len(value) != 1024 {
				t.Fatalf("expected value size 1024, got %d", len(value))
			}
		}
	})

	t.Run("concurrent with writes", func(t *testing.T) {
		t.Parallel()
		dataDir := t.TempDir()
		db, err := NewDB(dataDir, context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()

		// Write initial data
		for i := 0; i < 100; i++ {
			key := "concurrent-" + string(rune(i))
			value := make([]byte, 512)
			if err := db.Put(key, value); err != nil {
				t.Fatal(err)
			}
		}

		// Create orphans
		for i := 0; i < 100; i++ {
			key := "concurrent-" + string(rune(i))
			value := make([]byte, 256)
			if err := db.Put(key, value); err != nil {
				t.Fatal(err)
			}
		}

		// Start cleanup in background
		cleanupDone := make(chan error)
		go func() {
			cleanupDone <- db.Clean()
		}()

		// Write more data while cleanup is running
		for i := 0; i < 20; i++ {
			key := "concurrent-new-" + string(rune(i))
			value := []byte("new-value")
			if err := db.Put(key, value); err != nil {
				t.Logf("write during cleanup: %v", err)
			}
			time.Sleep(time.Millisecond)
		}

		// Wait for cleanup to complete
		if err := <-cleanupDone; err != nil {
			t.Fatalf("cleanup failed: %v", err)
		}

		// Verify original data
		for i := 0; i < 100; i++ {
			key := "concurrent-" + string(rune(i))
			value, ok := db.Get(key)
			if !ok {
				t.Fatalf("key not found after concurrent cleanup")
			}
			if len(value) != 256 {
				t.Fatalf("expected value size 256, got %d", len(value))
			}
		}

		// Verify new data written during cleanup
		for i := 0; i < 20; i++ {
			key := "concurrent-new-" + string(rune(i))
			value, ok := db.Get(key)
			if !ok {
				continue // Write was blocked during cleanup
			}
			if string(value) != "new-value" {
				t.Fatalf("expected 'new-value', got %q", value)
			}
		}
	})

	t.Run("with clear operation", func(t *testing.T) {
		t.Parallel()
		dataDir := t.TempDir()
		db, err := NewDB(dataDir, context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()

		// Write data
		for i := 0; i < 50; i++ {
			key := "clear-test-" + string(rune(i))
			value := make([]byte, 2048)
			if err := db.Put(key, value); err != nil {
				t.Fatal(err)
			}
		}

		// Create orphans
		for i := 0; i < 25; i++ {
			key := "clear-test-" + string(rune(i))
			value := make([]byte, 1024)
			if err := db.Put(key, value); err != nil {
				t.Fatal(err)
			}
		}

		// Clear the database
		if err := db.Clear(); err != nil {
			t.Fatal(err)
		}

		// Write new data
		for i := 0; i < 30; i++ {
			key := "after-clear-" + string(rune(i))
			value := []byte("new-data")
			if err := db.Put(key, value); err != nil {
				t.Fatal(err)
			}
		}

		// Cleanup should work after clear
		if err := db.Clean(); err != nil {
			t.Fatal(err)
		}

		// Verify new data is intact
		for i := 0; i < 30; i++ {
			key := "after-clear-" + string(rune(i))
			value, ok := db.Get(key)
			if !ok {
				t.Fatal("key not found after cleanup")
			}
			if string(value) != "new-data" {
				t.Fatalf("expected 'new-data', got %q", value)
			}
		}

		// Verify old data is gone
		for i := 0; i < 50; i++ {
			key := "clear-test-" + string(rune(i))
			_, ok := db.Get(key)
			if ok {
				t.Fatal("old data should not exist after clear")
			}
		}
	})
}

func TestCleanupEdgeCases(t *testing.T) {
	t.Run("empty values", func(t *testing.T) {
		t.Parallel()
		dataDir := t.TempDir()
		db, err := NewDB(dataDir, context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()

		// Write keys with empty values
		for i := 0; i < 20; i++ {
			key := "empty-" + string(rune(i))
			if err := db.Put(key, []byte{}); err != nil {
				t.Fatal(err)
			}
		}

		if err := db.Clean(); err != nil {
			t.Fatal(err)
		}

		// Verify empty values are preserved
		for i := 0; i < 20; i++ {
			key := "empty-" + string(rune(i))
			value, ok := db.Get(key)
			if !ok {
				t.Fatal("key with empty value not found")
			}
			if len(value) != 0 {
				t.Fatalf("expected empty value, got %d bytes", len(value))
			}
		}
	})

	t.Run("closed database", func(t *testing.T) {
		t.Parallel()
		dataDir := t.TempDir()
		db, err := NewDB(dataDir, context.Background())
		if err != nil {
			t.Fatal(err)
		}

		if err := db.Close(); err != nil {
			t.Fatal(err)
		}

		err = db.Clean()
		if err != ErrClosed {
			t.Fatalf("expected ErrClosed, got %v", err)
		}
	})
}
