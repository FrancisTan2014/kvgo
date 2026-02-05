package server

import (
	"context"
	"testing"
	"time"
)

func TestBacklogTrimmer_TrimsWhenOverThreshold(t *testing.T) {
	dir := t.TempDir()

	// Use limits above MinBacklogSize to avoid default override
	const (
		backlogLimit = 2 * 1024 * 1024 // 2MB (above 1MB min)
		trimDuration = 10 * time.Millisecond
		entrySize    = 100 * 1024 // 100KB per entry
	)

	s, err := NewServer(Options{
		Port:                0,
		DataDir:             dir,
		BacklogSizeLimit:    backlogLimit,
		BacklogTrimDuration: trimDuration,
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Shutdown(context.Background())

	// Append entries until we exceed 2x the limit (trim threshold)
	payload := make([]byte, entrySize)
	targetSize := int64(TrimRatioThreshold*backlogLimit) + int64(entrySize)

	seq := uint64(1)
	for s.backlogSize.Load() < targetSize {
		s.appendBacklog(backlogEntry{
			size:    entrySize,
			seq:     seq,
			payload: payload,
		})
		seq++
	}

	beforeTrim := s.backlogSize.Load()
	if beforeTrim < targetSize {
		t.Fatalf("expected backlogSize >= %d, got %d", targetSize, beforeTrim)
	}
	t.Logf("before trim: size=%d, entries=%d", beforeTrim, s.backlog.Len())

	// Wait for trimmer to run
	time.Sleep(trimDuration * 3)

	afterTrim := s.backlogSize.Load()
	t.Logf("after trim: size=%d, entries=%d", afterTrim, s.backlog.Len())

	// Verify size is trimmed down to <= limit
	if afterTrim > int64(backlogLimit) {
		t.Errorf("expected backlogSize <= %d after trim, got %d", backlogLimit, afterTrim)
	}

	// Verify we actually removed entries
	if afterTrim >= beforeTrim {
		t.Errorf("trimmer did not reduce size: before=%d, after=%d", beforeTrim, afterTrim)
	}
}

func TestBacklogTrimmer_DoesNotTrimBelowThreshold(t *testing.T) {
	dir := t.TempDir()

	const (
		backlogLimit = 2 * 1024 * 1024 // 2MB (above 1MB min)
		trimDuration = 10 * time.Millisecond
		entrySize    = 100 * 1024 // 100KB per entry
	)

	s, err := NewServer(Options{
		Port:                0,
		DataDir:             dir,
		BacklogSizeLimit:    backlogLimit,
		BacklogTrimDuration: trimDuration,
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Shutdown(context.Background())

	// Append entries but stay below threshold (< 2x limit)
	payload := make([]byte, entrySize)
	targetSize := int64(backlogLimit) // 1x limit, below 2x threshold

	seq := uint64(1)
	for s.backlogSize.Load() < targetSize {
		s.appendBacklog(backlogEntry{
			size:    entrySize,
			seq:     seq,
			payload: payload,
		})
		seq++
	}

	beforeWait := s.backlogSize.Load()
	entriesBefore := s.backlog.Len()
	t.Logf("before wait: size=%d, entries=%d", beforeWait, entriesBefore)

	// Wait for trimmer cycles
	time.Sleep(trimDuration * 3)

	afterWait := s.backlogSize.Load()
	entriesAfter := s.backlog.Len()
	t.Logf("after wait: size=%d, entries=%d", afterWait, entriesAfter)

	// Verify nothing was trimmed
	if entriesAfter != entriesBefore {
		t.Errorf("trimmer should not run below threshold: entries before=%d, after=%d", entriesBefore, entriesAfter)
	}
}

func TestBacklog_ExistsInBacklog(t *testing.T) {
	dir := t.TempDir()

	s, err := NewServer(Options{Port: 0, DataDir: dir})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Shutdown(context.Background())

	// Empty backlog
	if s.existsInBacklog(1) {
		t.Error("existsInBacklog should return false for empty backlog")
	}

	// Add entries seq 5, 6, 7
	for seq := uint64(5); seq <= 7; seq++ {
		s.appendBacklog(backlogEntry{size: 10, seq: seq, payload: []byte("test")})
	}

	tests := []struct {
		seq    uint64
		exists bool
	}{
		{4, false}, // before range
		{5, true},  // first entry
		{6, true},  // middle
		{7, true},  // last entry
		{8, false}, // after range
	}

	for _, tt := range tests {
		got := s.existsInBacklog(tt.seq)
		if got != tt.exists {
			t.Errorf("existsInBacklog(%d) = %v, want %v", tt.seq, got, tt.exists)
		}
	}
}

func TestBacklog_ForwardBacklog(t *testing.T) {
	dir := t.TempDir()

	s, err := NewServer(Options{Port: 0, DataDir: dir})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Shutdown(context.Background())

	// Add entries seq 10, 11, 12, 13, 14
	for seq := uint64(10); seq <= 14; seq++ {
		s.appendBacklog(backlogEntry{size: 10, seq: seq, payload: []byte{byte(seq)}})
	}

	// Forward from seq 12 onwards
	var forwarded []uint64
	err = s.forwardBacklog(12, func(e backlogEntry) error {
		forwarded = append(forwarded, e.seq)
		return nil
	})
	if err != nil {
		t.Fatalf("forwardBacklog: %v", err)
	}

	expected := []uint64{12, 13, 14}
	if len(forwarded) != len(expected) {
		t.Fatalf("forwarded %d entries, want %d", len(forwarded), len(expected))
	}
	for i, seq := range expected {
		if forwarded[i] != seq {
			t.Errorf("forwarded[%d] = %d, want %d", i, forwarded[i], seq)
		}
	}
}

func TestBacklog_ForwardBacklog_TrimmedSeq(t *testing.T) {
	dir := t.TempDir()

	s, err := NewServer(Options{Port: 0, DataDir: dir})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Shutdown(context.Background())

	// Add entries seq 10, 11, 12
	for seq := uint64(10); seq <= 12; seq++ {
		s.appendBacklog(backlogEntry{size: 10, seq: seq, payload: []byte{byte(seq)}})
	}

	// Request seq 5, which was "trimmed" - should start from 10
	var forwarded []uint64
	err = s.forwardBacklog(5, func(e backlogEntry) error {
		forwarded = append(forwarded, e.seq)
		return nil
	})
	if err != nil {
		t.Fatalf("forwardBacklog: %v", err)
	}

	expected := []uint64{10, 11, 12}
	if len(forwarded) != len(expected) {
		t.Fatalf("forwarded %d entries, want %d", len(forwarded), len(expected))
	}
	for i, seq := range expected {
		if forwarded[i] != seq {
			t.Errorf("forwarded[%d] = %d, want %d", i, forwarded[i], seq)
		}
	}
}
