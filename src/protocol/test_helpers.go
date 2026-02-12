package protocol

import (
	"bytes"
	"kvgo/transport"
	"sync"
)

// TestFramer is a Framer implementation for testing that captures written data.
type TestFramer struct {
	buf bytes.Buffer
	mu  sync.Mutex
}

// NewTestFramer creates a new TestFramer for testing.
func NewTestFramer() *TestFramer {
	return &TestFramer{}
}

// Write writes a frame to the internal buffer.
func (f *TestFramer) Write(payload []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return transport.WriteFrameForTest(&f.buf, payload)
}

// Read reads a frame from the internal buffer.
func (f *TestFramer) Read() ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return transport.ReadFrameMaxForTest(&f.buf, transport.DefaultMaxFrameSize)
}

// ReadWithTimeout reads a frame with a timeout (not implemented for testing).
func (f *TestFramer) ReadWithTimeout(timeout int) ([]byte, error) {
	return f.Read()
}

// Written returns all data written to this framer.
func (f *TestFramer) Written() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.buf.Bytes()
}

// Reset clears the internal buffer.
func (f *TestFramer) Reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.buf.Reset()
}
