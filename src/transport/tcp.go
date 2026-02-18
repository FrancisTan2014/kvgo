package transport

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"
)

// TcpStreamTransport implements StreamTransport over TCP with length-prefixed framing.
//
// Thread safety: Uses separate mutexes for read and write operations,
// allowing concurrent Send and Receive from different goroutines.
type TcpStreamTransport struct {
	conn    net.Conn
	framer  *Framer
	readMu  sync.Mutex
	writeMu sync.Mutex
	closed  bool
	closeMu sync.Mutex
}

// NewTcpStream creates a TCP stream transport from an existing connection.
func NewTcpStream(conn net.Conn) *TcpStreamTransport {
	return &TcpStreamTransport{
		conn:   conn,
		framer: NewConnFramer(conn),
	}
}

// Send transmits a message. Thread-safe for concurrent goroutines.
func (t *TcpStreamTransport) Send(ctx context.Context, payload []byte) error {
	t.closeMu.Lock()
	if t.closed {
		t.closeMu.Unlock()
		return fmt.Errorf("transport closed")
	}
	t.closeMu.Unlock()

	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	if deadline, ok := ctx.Deadline(); ok {
		return t.framer.WriteWithDeadline(payload, deadline)
	}
	return t.framer.Write(payload)
}

// Receive reads the next message. Thread-safe for concurrent goroutines.
func (t *TcpStreamTransport) Receive(ctx context.Context) ([]byte, error) {
	t.closeMu.Lock()
	if t.closed {
		t.closeMu.Unlock()
		return nil, fmt.Errorf("transport closed")
	}
	t.closeMu.Unlock()

	t.readMu.Lock()
	defer t.readMu.Unlock()
	if deadline, ok := ctx.Deadline(); ok {
		return t.framer.ReadWithDeadline(deadline)
	}
	return t.framer.Read()
}

// Close terminates the transport.
func (t *TcpStreamTransport) Close() error {
	t.closeMu.Lock()
	defer t.closeMu.Unlock()

	if t.closed {
		return nil
	}
	t.closed = true
	return t.conn.Close()
}

// RemoteAddr returns the remote address.
func (t *TcpStreamTransport) RemoteAddr() string {
	return t.conn.RemoteAddr().String()
}

// TcpRequestTransport implements RequestTransport over TCP with length-prefixed framing.
//
// Thread safety: Uses mutex to serialize requests. Only one request at a time
// (no multiplexing). For concurrent requests, use multiple transports.
//
// Note: This is a simple implementation. For high-concurrency scenarios,
// consider implementing correlation IDs for request multiplexing.
type TcpRequestTransport struct {
	conn    net.Conn
	framer  *Framer
	mu      sync.Mutex // Serializes requests
	closed  bool
	closeMu sync.Mutex
}

// NewTcpRequest creates a TCP request transport from an existing connection.
func NewTcpRequest(conn net.Conn) *TcpRequestTransport {
	return &TcpRequestTransport{
		conn:   conn,
		framer: NewConnFramer(conn),
	}
}

// Request sends a message and waits for response.
// Thread-safe but serializes requests (no concurrent requests).
func (t *TcpRequestTransport) Request(ctx context.Context, payload []byte) ([]byte, error) {
	t.closeMu.Lock()
	if t.closed {
		t.closeMu.Unlock()
		return nil, fmt.Errorf("transport closed")
	}
	t.closeMu.Unlock()

	t.mu.Lock()
	defer t.mu.Unlock()

	// Extract deadline from context; if none, use zero (no deadline)
	deadline, _ := ctx.Deadline()

	// Send request
	if err := t.framer.WriteWithDeadline(payload, deadline); err != nil {
		return nil, fmt.Errorf("write request: %w", err)
	}

	// Receive response (shares same deadline)
	resp, err := t.framer.ReadWithDeadline(deadline)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	return resp, nil
}

// Close terminates the transport.
func (t *TcpRequestTransport) Close() error {
	t.closeMu.Lock()
	defer t.closeMu.Unlock()

	if t.closed {
		return nil
	}
	t.closed = true
	return t.conn.Close()
}

// RemoteAddr returns the remote address.
func (t *TcpRequestTransport) RemoteAddr() string {
	return t.conn.RemoteAddr().String()
}

// DialTcpStream establishes a TCP connection and returns a StreamTransport.
func DialTcpStream(network, addr string, timeout time.Duration) (StreamTransport, error) {
	conn, err := net.DialTimeout(network, addr, timeout)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", addr, err)
	}
	return NewTcpStream(conn), nil
}

// DialTcpRequest establishes a TCP connection and returns a RequestTransport.
func DialTcpRequest(network, addr string, timeout time.Duration) (RequestTransport, error) {
	conn, err := net.DialTimeout(network, addr, timeout)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", addr, err)
	}
	return NewTcpRequest(conn), nil
}
