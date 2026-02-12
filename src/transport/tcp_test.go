package transport

import (
	"bytes"
	"net"
	"testing"
	"time"
)

// setupTCPPair creates a real TCP connection pair for testing
func setupTCPPair(t *testing.T) (*TcpStreamTransport, *TcpStreamTransport, func()) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen failed: %v", err)
	}

	addr := listener.Addr().String()

	// Accept in background
	serverConnCh := make(chan net.Conn, 1)
	go func() {
		conn, _ := listener.Accept()
		serverConnCh <- conn
	}()

	// Dial
	client, err := net.Dial("tcp", addr)
	if err != nil {
		listener.Close()
		t.Fatalf("Dial failed: %v", err)
	}

	server := <-serverConnCh

	cleanup := func() {
		listener.Close()
		server.Close()
		client.Close()
	}

	return NewTcpStream(server), NewTcpStream(client), cleanup
}

func TestTcpStreamTransport_BasicSendReceive(t *testing.T) {
	serverTransport, clientTransport, cleanup := setupTCPPair(t)
	defer cleanup()

	// Send from client to server
	msg := []byte("hello from client")
	if err := clientTransport.Send(msg); err != nil {
		t.Fatalf("Send failed: %v", err)
	}

	// Receive on server
	received, err := serverTransport.Receive()
	if err != nil {
		t.Fatalf("Receive failed: %v", err)
	}

	if !bytes.Equal(received, msg) {
		t.Errorf("got %q, want %q", received, msg)
	}
}

func TestTcpStreamTransport_Bidirectional(t *testing.T) {
	serverTransport, clientTransport, cleanup := setupTCPPair(t)
	defer cleanup()

	// Client → Server
	msg1 := []byte("ping")
	if err := clientTransport.Send(msg1); err != nil {
		t.Fatalf("Client send failed: %v", err)
	}

	received1, err := serverTransport.Receive()
	if err != nil {
		t.Fatalf("Server receive failed: %v", err)
	}
	if !bytes.Equal(received1, msg1) {
		t.Errorf("Server got %q, want %q", received1, msg1)
	}

	// Server → Client
	msg2 := []byte("pong")
	if err := serverTransport.Send(msg2); err != nil {
		t.Fatalf("Server send failed: %v", err)
	}

	received2, err := clientTransport.Receive()
	if err != nil {
		t.Fatalf("Client receive failed: %v", err)
	}
	if !bytes.Equal(received2, msg2) {
		t.Errorf("Client got %q, want %q", received2, msg2)
	}
}

func TestTcpStreamTransport_Close(t *testing.T) {
	_, clientTransport, cleanup := setupTCPPair(t)
	defer cleanup()

	// Close transport
	if err := clientTransport.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	// Operations after close should fail
	if err := clientTransport.Send([]byte("test")); err == nil {
		t.Error("Send after close should fail")
	}

	if _, err := clientTransport.Receive(); err == nil {
		t.Error("Receive after close should fail")
	}

	// Double close should be safe
	if err := clientTransport.Close(); err != nil {
		t.Errorf("Second close failed: %v", err)
	}
}

func TestTcpRequestTransport_BasicRequest(t *testing.T) {
	// Setup listener
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen failed: %v", err)
	}
	defer listener.Close()

	addr := listener.Addr().String()

	// Server echo goroutine
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		transport := NewTcpStream(conn)
		msg, _ := transport.Receive()
		_ = transport.Send(msg)
	}()

	// Client request
	clientTransport, err := DialTcpRequest("tcp", addr, 1*time.Second)
	if err != nil {
		t.Fatalf("Dial failed: %v", err)
	}
	defer clientTransport.Close()

	req := []byte("echo test")
	resp, err := clientTransport.Request(req, 1*time.Second)
	if err != nil {
		t.Fatalf("Request failed: %v", err)
	}

	if !bytes.Equal(resp, req) {
		t.Errorf("got %q, want %q", resp, req)
	}
}

func TestDialTcpStream(t *testing.T) {
	// Start simple echo server
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen failed: %v", err)
	}
	defer listener.Close()

	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		transport := NewTcpStream(conn)
		msg, _ := transport.Receive()
		_ = transport.Send(msg)
	}()

	// Dial and test
	addr := listener.Addr().String()
	transport, err := DialTcpStream("tcp", addr, 1*time.Second)
	if err != nil {
		t.Fatalf("DialTcpStream failed: %v", err)
	}
	defer transport.Close()

	msg := []byte("dial test")
	if err := transport.Send(msg); err != nil {
		t.Fatalf("Send failed: %v", err)
	}

	resp, err := transport.Receive()
	if err != nil {
		t.Fatalf("Receive failed: %v", err)
	}

	if !bytes.Equal(resp, msg) {
		t.Errorf("got %q, want %q", resp, msg)
	}
}
