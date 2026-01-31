package engine

import (
	"bytes"
	"os"
	"strings"
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
	if err := db.Put("user:1", []byte("Alice")); err != nil {
		t.Fatalf("Put user:1 failed: %v", err)
	}
	if err := db.Put("user:2", []byte("Bob")); err != nil {
		t.Fatalf("Put user:2 failed: %v", err)
	}

	// CRITICAL: Close before reopening (Windows won't delete open files)
	if err := db.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	// --- PHASE 2: Restart ---
	t.Log("Restarting DB...")
	db2, err := NewDB(tmpFile) // Open the SAME file
	if err != nil {
		t.Fatalf("Failed to re-open DB: %v", err)
	}
	defer db2.Close()

	// --- PHASE 3: Verify ---
	val, ok := db2.Get("user:1")
	if !ok {
		t.Fatal("Key 'user:1' not found after restart!")
	}
	if string(val) != "Alice" {
		t.Fatalf("Expected 'Alice', got '%s'", val)
	}
}

func TestPut_EdgeCases(t *testing.T) {
	tmpFile := "test_put_edges.db"
	os.Remove(tmpFile)
	defer os.Remove(tmpFile)

	db, err := NewDB(tmpFile)
	if err != nil {
		t.Fatalf("Failed to create DB: %v", err)
	}
	defer db.Close()

	tests := []struct {
		name    string
		key     string
		value   []byte
		wantErr bool
	}{
		// Basic cases
		{name: "simple key-value", key: "foo", value: []byte("bar"), wantErr: false},
		{name: "single char key", key: "x", value: []byte("y"), wantErr: false},

		// Empty/nil values
		{name: "empty key", key: "", value: []byte("value"), wantErr: false},
		{name: "empty value", key: "key", value: []byte{}, wantErr: false},
		{name: "nil value", key: "nilval", value: nil, wantErr: false},

		// Special characters
		{name: "key with spaces", key: "hello world", value: []byte("spaced"), wantErr: false},
		{name: "key with unicode", key: "用户:123", value: []byte("中文"), wantErr: false},
		{name: "key with emoji", key: "user:🔥", value: []byte("fire"), wantErr: false},
		{name: "key with newlines", key: "line1\nline2", value: []byte("multiline"), wantErr: false},
		{name: "key with null byte", key: "before\x00after", value: []byte("null"), wantErr: false},
		{name: "key with tabs", key: "col1\tcol2", value: []byte("tabbed"), wantErr: false},

		// Binary values
		{name: "binary value", key: "binary", value: []byte{0x00, 0xFF, 0x7F, 0x80}, wantErr: false},
		{name: "all zeros value", key: "zeros", value: make([]byte, 100), wantErr: false},

		// Size edge cases
		{name: "long key (1KB)", key: strings.Repeat("k", 1024), value: []byte("v"), wantErr: false},
		{name: "long value (1MB)", key: "bigval", value: make([]byte, 1024*1024), wantErr: false},

		// Redis-style patterns
		{name: "colon separator", key: "user:100:profile", value: []byte("data"), wantErr: false},
		{name: "dot separator", key: "config.db.host", value: []byte("localhost"), wantErr: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := db.Put(tt.key, tt.value)

			if tt.wantErr && err == nil {
				t.Error("expected error, got nil")
				return
			}
			if !tt.wantErr && err != nil {
				t.Errorf("unexpected error: %v", err)
				return
			}

			// Verify round-trip
			if !tt.wantErr {
				got, ok := db.Get(tt.key)
				if !ok {
					t.Errorf("key not found after Put")
					return
				}

				// Handle nil value case (stored as empty)
				expected := tt.value
				if expected == nil {
					expected = []byte{}
				}

				if !bytes.Equal(got, expected) {
					if len(got) > 50 || len(expected) > 50 {
						t.Errorf("value mismatch: got %d bytes, want %d bytes", len(got), len(expected))
					} else {
						t.Errorf("value mismatch: got %q, want %q", got, expected)
					}
				}
			}
		})
	}
}

func TestClear(t *testing.T) {
	tmpFile := "test_clear.db"
	os.Remove(tmpFile)
	defer os.Remove(tmpFile)

	db, err := NewDB(tmpFile)
	if err != nil {
		t.Fatalf("Failed to create DB: %v", err)
	}
	defer db.Close()

	// Write some data
	if err := db.Put("key1", []byte("value1")); err != nil {
		t.Fatalf("Put key1 failed: %v", err)
	}
	if err := db.Put("key2", []byte("value2")); err != nil {
		t.Fatalf("Put key2 failed: %v", err)
	}

	// Verify data exists
	if _, ok := db.Get("key1"); !ok {
		t.Fatal("key1 should exist before clear")
	}
	if _, ok := db.Get("key2"); !ok {
		t.Fatal("key2 should exist before clear")
	}

	// Clear the database
	if err := db.Clear(); err != nil {
		t.Fatalf("Clear failed: %v", err)
	}

	// Verify all data is gone
	if _, ok := db.Get("key1"); ok {
		t.Error("key1 should not exist after clear")
	}
	if _, ok := db.Get("key2"); ok {
		t.Error("key2 should not exist after clear")
	}

	// Verify we can write new data after clear
	if err := db.Put("key3", []byte("value3")); err != nil {
		t.Fatalf("Put after clear failed: %v", err)
	}
	if val, ok := db.Get("key3"); !ok || string(val) != "value3" {
		t.Error("key3 should exist after clear and new write")
	}
}
