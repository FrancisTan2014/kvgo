package transport

import (
	"net"
	"testing"
	"time"
)

func TestNewStreamTransport_TCP(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()

	st := NewStreamTransport(ProtocolTCP, c1)
	if st == nil {
		t.Fatal("expected non-nil StreamTransport")
	}
}

func TestNewStreamTransport_DefaultsToTCP(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()

	st := NewStreamTransport("", c1) // Empty string should default to TCP
	if st == nil {
		t.Fatal("expected non-nil StreamTransport")
	}
}

func TestNewStreamTransport_UnsupportedProtocol(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for unsupported protocol")
		}
	}()

	NewStreamTransport("quic", c1) // Should panic
}

func TestDialStreamTransport_TCP(t *testing.T) {
	// Start listener
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	addr := ln.Addr().String()

	// Accept in background
	done := make(chan struct{})
	go func() {
		conn, err := ln.Accept()
		if err == nil {
			conn.Close()
		}
		close(done)
	}()

	// Dial
	st, err := DialStreamTransport(ProtocolTCP, "tcp", addr, time.Second)
	if err != nil {
		t.Fatalf("DialStreamTransport: %v", err)
	}
	st.Close()

	<-done
}

func TestDialStreamTransport_UnsupportedProtocol(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for unsupported protocol")
		}
	}()

	_, _ = DialStreamTransport("grpc", "tcp", "127.0.0.1:9999", time.Second)
}

func TestNewRequestTransport_TCP(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()

	rt := NewRequestTransport(ProtocolTCP, c1)
	if rt == nil {
		t.Fatal("expected non-nil RequestTransport")
	}
}

func TestNewRequestTransport_DefaultsToTCP(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()

	rt := NewRequestTransport("", c1) // Empty string should default to TCP
	if rt == nil {
		t.Fatal("expected non-nil RequestTransport")
	}
}

func TestNewRequestTransport_UnsupportedProtocol(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for unsupported protocol")
		}
	}()

	NewRequestTransport("websocket", c1) // Should panic
}

func TestDialRequestTransport_TCP(t *testing.T) {
	// Start listener
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	addr := ln.Addr().String()

	// Accept in background
	done := make(chan struct{})
	go func() {
		conn, err := ln.Accept()
		if err == nil {
			conn.Close()
		}
		close(done)
	}()

	// Dial
	rt, err := DialRequestTransport(ProtocolTCP, "tcp", addr, time.Second)
	if err != nil {
		t.Fatalf("DialRequestTransport: %v", err)
	}
	rt.Close()

	<-done
}

func TestDialRequestTransport_UnsupportedProtocol(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for unsupported protocol")
		}
	}()

	_, _ = DialRequestTransport("http2", "tcp", "127.0.0.1:9999", time.Second)
}
