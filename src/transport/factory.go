package transport

import (
	"fmt"
	"net"
	"time"
)

// Supported protocols
const (
	ProtocolTCP = "tcp" // Length-prefixed framing over TCP
)

// NewStreamTransport creates a StreamTransport from an existing connection.
// Currently only supports TCP protocol. Will panic for unsupported protocols.
func NewStreamTransport(protocol string, conn net.Conn) StreamTransport {
	if protocol == "" {
		protocol = ProtocolTCP
	}

	switch protocol {
	case ProtocolTCP:
		return NewTcpStream(conn)
	default:
		panic(fmt.Sprintf("transport: unsupported protocol %q (only %q is implemented)", protocol, ProtocolTCP))
	}
}

// DialStreamTransport establishes a connection and returns a StreamTransport.
// Currently only supports TCP protocol. Will panic for unsupported protocols.
func DialStreamTransport(protocol, network, addr string, timeout time.Duration) (StreamTransport, error) {
	if protocol == "" {
		protocol = ProtocolTCP
	}

	switch protocol {
	case ProtocolTCP:
		return DialTcpStream(network, addr, timeout)
	default:
		panic(fmt.Sprintf("transport: unsupported protocol %q (only %q is implemented)", protocol, ProtocolTCP))
	}
}

// NewRequestTransport creates a RequestTransport from an existing connection.
// Currently only supports TCP protocol. Will panic for unsupported protocols.
func NewRequestTransport(protocol string, conn net.Conn) RequestTransport {
	if protocol == "" {
		protocol = ProtocolTCP
	}

	switch protocol {
	case ProtocolTCP:
		return NewTcpRequest(conn)
	default:
		panic(fmt.Sprintf("transport: unsupported protocol %q (only %q is implemented)", protocol, ProtocolTCP))
	}
}

// DialRequestTransport establishes a connection and returns a RequestTransport.
// Currently only supports TCP protocol. Will panic for unsupported protocols.
func DialRequestTransport(protocol, network, addr string, timeout time.Duration) (RequestTransport, error) {
	if protocol == "" {
		protocol = ProtocolTCP
	}

	switch protocol {
	case ProtocolTCP:
		return DialTcpRequest(network, addr, timeout)
	default:
		panic(fmt.Sprintf("transport: unsupported protocol %q (only %q is implemented)", protocol, ProtocolTCP))
	}
}
