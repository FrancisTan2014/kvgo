package transport

import (
	"context"
	"sync"
)

// streamAsRequest wraps a StreamTransport to implement RequestTransport.
// Used when you need request-response semantics on an existing stream connection.
//
// Thread safety: Serializes requests (no multiplexing). For concurrent requests,
// use separate connections or a proper multiplexing transport.
type streamAsRequest struct {
	stream StreamTransport
	mu     sync.Mutex // Serialize request-response cycles
}

// WrapStreamAsRequest converts a StreamTransport to RequestTransport.
// Useful for quorum reads on primary connection (which is a replication stream).
func WrapStreamAsRequest(stream StreamTransport) RequestTransport {
	return &streamAsRequest{stream: stream}
}

// AsRequestTransport returns the RequestTransport interface for a StreamTransport.
// If the transport implements both (like MultiplexedTransport), returns it directly.
// Otherwise, wraps it with an adapter. This hides implementation details from callers.
func AsRequestTransport(stream StreamTransport) RequestTransport {
	if rt, ok := stream.(RequestTransport); ok {
		return rt
	}
	return WrapStreamAsRequest(stream)
}

func (s *streamAsRequest) Request(ctx context.Context, payload []byte) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.stream.Send(ctx, payload); err != nil {
		return nil, err
	}
	return s.stream.Receive(ctx)
}

func (s *streamAsRequest) Close() error {
	return s.stream.Close()
}

func (s *streamAsRequest) RemoteAddr() string {
	return s.stream.RemoteAddr()
}
