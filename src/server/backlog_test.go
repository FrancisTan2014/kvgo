package server

import (
	"container/list"
	"sync/atomic"
	"testing"
)

// TestExistsInBacklog tests the backlog membership check logic
func TestExistsInBacklog(t *testing.T) {
	tests := []struct {
		name       string
		entries    []uint64 // sequence numbers to add
		checkSeq   uint64
		wantExists bool
	}{
		{
			name:       "empty backlog",
			entries:    []uint64{},
			checkSeq:   1,
			wantExists: false,
		},
		{
			name:       "seq before range",
			entries:    []uint64{5, 6, 7},
			checkSeq:   4,
			wantExists: false,
		},
		{
			name:       "seq at start",
			entries:    []uint64{5, 6, 7},
			checkSeq:   5,
			wantExists: true,
		},
		{
			name:       "seq in middle",
			entries:    []uint64{5, 6, 7},
			checkSeq:   6,
			wantExists: true,
		},
		{
			name:       "seq at end",
			entries:    []uint64{5, 6, 7},
			checkSeq:   7,
			wantExists: true,
		},
		{
			name:       "seq after range",
			entries:    []uint64{5, 6, 7},
			checkSeq:   8,
			wantExists: false,
		},
		{
			name:       "single entry match",
			entries:    []uint64{10},
			checkSeq:   10,
			wantExists: true,
		},
		{
			name:       "large gap",
			entries:    []uint64{100, 101, 102},
			checkSeq:   1,
			wantExists: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &Server{}
			for _, seq := range tt.entries {
				s.backlog.PushBack(backlogEntry{seq: seq, size: 10, payload: []byte("test")})
			}

			got := s.existsInBacklog(tt.checkSeq)
			if got != tt.wantExists {
				t.Errorf("existsInBacklog(%d) = %v, want %v", tt.checkSeq, got, tt.wantExists)
			}
		})
	}
}

// TestForwardBacklog tests forwarding entries from a specific sequence
func TestForwardBacklog(t *testing.T) {
	tests := []struct {
		name        string
		entries     []uint64 // sequence numbers to add
		startSeq    uint64   // where to start forwarding
		wantForward []uint64 // expected forwarded sequences
	}{
		{
			name:        "empty backlog",
			entries:     []uint64{},
			startSeq:    1,
			wantForward: []uint64{},
		},
		{
			name:        "forward from start",
			entries:     []uint64{10, 11, 12, 13},
			startSeq:    10,
			wantForward: []uint64{10, 11, 12, 13},
		},
		{
			name:        "forward from middle",
			entries:     []uint64{10, 11,12, 13, 14},
			startSeq:    12,
			wantForward: []uint64{12, 13, 14},
		},
		{
			name:        "forward from end",
			entries:     []uint64{10, 11, 12},
			startSeq:    12,
			wantForward: []uint64{12},
		},
		{
			name:        "seq before range (starts from beginning)",
			entries:     []uint64{10, 11, 12},
			startSeq:    5,
			wantForward: []uint64{10, 11, 12},
		},
		{
			name:        "seq after range (nothing forwarded)",
			entries:     []uint64{10, 11, 12},
			startSeq:    15,
			wantForward: []uint64{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &Server{}
			for _, seq := range tt.entries {
				s.backlog.PushBack(backlogEntry{seq: seq, size: 10, payload: []byte{byte(seq)}})
			}

			var forwarded []uint64
			err := s.forwardBacklog(tt.startSeq, func(e backlogEntry) error {
				forwarded = append(forwarded, e.seq)
				return nil
			})

			if err != nil {
				t.Fatalf("forwardBacklog: %v", err)
			}

			if len(forwarded) != len(tt.wantForward) {
				t.Errorf("forwarded %d entries, want %d", len(forwarded), len(tt.wantForward))
				return
			}

			for i, want := range tt.wantForward {
				if forwarded[i] != want {
					t.Errorf("forwarded[%d] = %d, want %d", i, forwarded[i], want)
				}
			}
		})
	}
}

// TestAppendBacklog tests the backlog append logic
func TestAppendBacklog(t *testing.T) {
	s := &Server{
		backlog: list.List{},
	}
	s.backlogSize = atomic.Int64{}

	// Append first entry
	s.appendBacklog(backlogEntry{seq: 1, size: 100, payload: []byte("a")})
	if s.backlog.Len() != 1 {
		t.Errorf("backlog length = %d, want 1", s.backlog.Len())
	}
	if s.backlogSize.Load() != 100 {
		t.Errorf("backlog size = %d, want 100", s.backlogSize.Load())
	}

	// Append second entry
	s.appendBacklog(backlogEntry{seq: 2, size: 200, payload: []byte("b")})
	if s.backlog.Len() != 2 {
		t.Errorf("backlog length = %d, want 2", s.backlog.Len())
	}
	if s.backlogSize.Load() != 300 {
		t.Errorf("backlog size = %d, want 300", s.backlogSize.Load())
	}

	// Verify order (FIFO)
	front := s.backlog.Front().Value.(backlogEntry)
	if front.seq != 1 {
		t.Errorf("front seq = %d, want 1", front.seq)
	}

	back := s.backlog.Back().Value.(backlogEntry)
	if back.seq != 2 {
		t.Errorf("back seq = %d, want 2", back.seq)
	}
}
