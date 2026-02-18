// Package transport provides protocol-agnostic abstractions for network communication.
//
// This layer decouples the server's business logic from protocol details (TCP, HTTP/2, QUIC),
// enabling clean architecture, thread safety, and future protocol support.
package transport

import "context"

// StreamTransport abstracts bidirectional byte streaming.
//
// Use cases:
//   - Replication (replica→primary stream)
//   - Client connections (request-response over persistent connection)
//
// Thread safety: Implementations must be safe for concurrent Send and Receive.
//
// Context usage: Pass context.Background() for no timeout/cancellation.
// Use context.WithTimeout or context.WithDeadline for deadline-bounded operations.
type StreamTransport interface {
	// Send transmits a message. Respects context deadline if set.
	Send(ctx context.Context, payload []byte) error

	// Receive reads the next message. Respects context deadline if set.
	Receive(ctx context.Context) ([]byte, error)

	// Close terminates the transport. Subsequent operations return errors.
	Close() error

	// RemoteAddr returns the remote endpoint address (for logging/debugging).
	RemoteAddr() string
}

// RequestTransport abstracts request-response communication.
//
// Use cases:
//   - Quorum reads (query peer replicas)
//   - Service discovery (health checks, peer queries)
//
// Thread safety: Implementations must support concurrent requests.
//
// Context usage: The context deadline covers the entire request-response cycle.
type RequestTransport interface {
	// Request sends a message and waits for a response.
	// Returns error if context is cancelled/expired or connection fails.
	Request(ctx context.Context, payload []byte) ([]byte, error)

	// Close terminates the transport. Pending requests return errors.
	Close() error

	// RemoteAddr returns the remote endpoint address.
	RemoteAddr() string
}
