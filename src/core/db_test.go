package core

import (
	"os"
	"testing"
)

// The "Systems" Test
func TestDurability(t *testing.T) {
	// 1. Setup: Use a temp file so we don't mess up your real DB
	tmpFile := "test_wal.db"
	os.Remove(tmpFile)       // Clean up previous runs
	defer os.Remove(tmpFile) // Clean up after we are done

	// --- PHASE 1: Write ---
	db, err := NewDB(tmpFile)
	if err != nil {
		t.Fatalf("Failed to open DB: %v", err)
	}

	t.Log("Writing data...")
	db.Put("user:1", []byte("Alice"))
	db.Put("user:2", []byte("Bob"))

	// CRITICAL: Close the DB to flush buffers and release file handles
	db.Close()

	// --- PHASE 2: Restart ---
	t.Log("Restarting DB...")
	db2, err := NewDB(tmpFile) // Open the SAME file
	if err != nil {
		t.Fatalf("Failed to re-open DB: %v", err)
	}

	// --- PHASE 3: Verify ---
	val, ok := db2.Get("user:1")
	if !ok {
		t.Fatal("Key 'user:1' not found after restart!")
	}
	if string(val) != "Alice" {
		t.Fatalf("Expected 'Alice', got '%s'", val)
	}
}
