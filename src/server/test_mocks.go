package server

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"

	"kvgo/protocol"
)

// mockStreamTransport for testing - captures writes and provides configurable behavior
type mockStreamTransport struct {
	mu            sync.Mutex
	written       []byte
	shouldACK     bool
	sendNACK      bool
	delay         time.Duration
	lastRequestId string
	receiveData   []byte
	receiveErr    error
	address       string
}

func (m *mockStreamTransport) Send(ctx context.Context, payload []byte) error {
	if m.delay > 0 {
		time.Sleep(m.delay)
	}

	m.mu.Lock()
	m.written = payload

	// Capture RequestID for ACK/NACK
	if req, err := protocol.DecodeRequest(payload); err == nil && req.RequestId != "" {
		m.lastRequestId = req.RequestId
	}
	m.mu.Unlock()

	return nil
}

func (m *mockStreamTransport) Receive(ctx context.Context) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.receiveErr != nil {
		return nil, m.receiveErr
	}
	if m.receiveData != nil {
		return m.receiveData, nil
	}
	return nil, io.EOF
}

func (m *mockStreamTransport) Close() error {
	return nil
}

func (m *mockStreamTransport) RemoteAddr() string {
	if m.address != "" {
		return m.address
	}
	return "mock:6379"
}

// mockRequestTransport for testing request-response patterns
type mockRequestTransport struct {
	mu       sync.Mutex
	response []byte
	err      error
	delay    time.Duration
	address  string
}

func (m *mockRequestTransport) Request(ctx context.Context, payload []byte) ([]byte, error) {
	m.mu.Lock()
	delay := m.delay
	m.mu.Unlock()

	if delay > 0 {
		deadline, hasDeadline := ctx.Deadline()
		if hasDeadline && time.Until(deadline) < delay {
			return nil, errors.New("timeout")
		}
		time.Sleep(delay)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.err != nil {
		return nil, m.err
	}
	return m.response, nil
}

func (m *mockRequestTransport) Close() error {
	return nil
}

func (m *mockRequestTransport) RemoteAddr() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.address != "" {
		return m.address
	}
	return "mock:1234"
}

// fakeStreamTransport - minimal implementation for pointer identity tests
type fakeStreamTransport struct {
	id int // Make non-zero-sized so each instance gets unique address
}

func (f *fakeStreamTransport) Send(ctx context.Context, payload []byte) error { return nil }
func (f *fakeStreamTransport) Receive(ctx context.Context) ([]byte, error)    { return nil, nil }
func (f *fakeStreamTransport) Close() error                                   { return nil }
func (f *fakeStreamTransport) RemoteAddr() string                             { return "fake:1234" }
