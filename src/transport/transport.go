// Package transport provides protocol-agnostic abstractions for network communication.
//
// This layer decouples the server's business logic from protocol details (TCP, HTTP/2, QUIC),
// enabling clean architecture, thread safety, and future protocol support.
package transport

import "time"

// StreamTransport abstracts bidirectional byte streaming.
//
// Use cases:
//   - Replication (replica→primary stream)
//   - Client connections (request-response over persistent connection)
//
// Thread safety: Implementations must be safe for concurrent Send and Receive.
type StreamTransport interface {
	// Send transmits a message. Blocks until written or error.
	Send(payload []byte) error

	// SendWithTimeout transmits a message with timeout.
	SendWithTimeout(payload []byte, timeout time.Duration) error

	// Receive reads the next message. Blocks until available or error.
	Receive() ([]byte, error)

	// ReceiveWithTimeout reads the next message with timeout.
	ReceiveWithTimeout(timeout time.Duration) ([]byte, error)

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
type RequestTransport interface {
	// Request sends a message and waits for a response.
	// Returns error if timeout expires or connection fails.
	Request(payload []byte, timeout time.Duration) ([]byte, error)

	// Close terminates the transport. Pending requests return errors.
	Close() error

	// RemoteAddr returns the remote endpoint address.
	RemoteAddr() string
}
