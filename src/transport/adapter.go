package transport

import (
	"sync"
	"time"
)

// streamAsRequest wraps a StreamTransport to implement RequestTransport.
// Used when you need request-response semantics on an existing stream connection.
//
// Thread safety: Serializes requests (no multiplexing). For concurrent requests,
// use separate connections or a proper multiplexing transport.
type streamAsRequest struct {
	stream  StreamTransport
	timeout time.Duration
	mu      sync.Mutex // Serialize request-response cycles
}

// WrapStreamAsRequest converts a StreamTransport to RequestTransport.
// Useful for quorum reads on primary connection (which is a replication stream).
func WrapStreamAsRequest(stream StreamTransport, defaultTimeout time.Duration) RequestTransport {
	return &streamAsRequest{
		stream:  stream,
		timeout: defaultTimeout,
	}
}

// AsRequestTransport returns the RequestTransport interface for a StreamTransport.
// If the transport implements both (like MultiplexedTransport), returns it directly.
// Otherwise, wraps it with an adapter. This hides implementation details from callers.
func AsRequestTransport(stream StreamTransport, defaultTimeout time.Duration) RequestTransport {
	if rt, ok := stream.(RequestTransport); ok {
		return rt
	}
	return WrapStreamAsRequest(stream, defaultTimeout)
}

func (s *streamAsRequest) Request(payload []byte, timeout time.Duration) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Use provided timeout or fallback to default
	if timeout == 0 {
		timeout = s.timeout
	}

	// Assuming the stream has timeout methods (TcpStreamTransport does)
	// Type assert to get access to timeout methods
	type timeoutStream interface {
		SendWithTimeout([]byte, time.Duration) error
		ReceiveWithTimeout(time.Duration) ([]byte, error)
	}

	if ts, ok := s.stream.(timeoutStream); ok {
		if err := ts.SendWithTimeout(payload, timeout); err != nil {
			return nil, err
		}
		return ts.ReceiveWithTimeout(timeout)
	}

	// Fallback: use basic methods (no timeout control)
	if err := s.stream.Send(payload); err != nil {
		return nil, err
	}
	return s.stream.Receive()
}

func (s *streamAsRequest) Close() error {
	return s.stream.Close()
}

func (s *streamAsRequest) RemoteAddr() string {
	return s.stream.RemoteAddr()
}
