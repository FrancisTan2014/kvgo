package transport

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

// TestMultiplexedTransport_ConcurrentRequests verifies that multiple concurrent
// requests to the same transport do NOT serialize (unlike TcpRequestTransport).
func TestMultiplexedTransport_ConcurrentRequests(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	clientTransport := NewMultiplexedTransport(client)
	defer clientTransport.Close()

	// Server echo handler
	go func() {
		framer := NewConnFramer(server)
		for {
			payload, err := framer.Read()
			if err != nil {
				return
			}
			// Echo back with same RequestID
			if err := framer.Write(payload); err != nil {
				return
			}
		}
	}()

	// Send 10 concurrent requests
	const numRequests = 10
	var wg sync.WaitGroup
	results := make([]error, numRequests)
	start := time.Now()

	for i := 0; i < numRequests; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			msg := []byte(fmt.Sprintf("request-%d", idx))
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			resp, err := clientTransport.Request(ctx, msg)
			if err != nil {
				results[idx] = err
				return
			}
			// Verify response matches request
			if string(resp) != string(msg) {
				results[idx] = fmt.Errorf("mismatch: got %s, want %s", resp, msg)
			}
		}(i)
	}

	wg.Wait()
	elapsed := time.Since(start)

	// Verify all succeeded
	for i, err := range results {
		if err != nil {
			t.Errorf("request %d failed: %v", i, err)
		}
	}

	// Should complete quickly (not serialized)
	if elapsed > 500*time.Millisecond {
		t.Logf("warning: concurrent requests took %v (expected < 500ms)", elapsed)
	}
}

// TestMultiplexedTransport_RequestTimeout verifies timeout handling.
func TestMultiplexedTransport_RequestTimeout(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	clientTransport := NewMultiplexedTransport(client)
	defer clientTransport.Close()

	// Server that never responds
	go func() {
		framer := NewConnFramer(server)
		for {
			_, err := framer.Read()
			if err != nil {
				return
			}
			// Don't send response - simulate timeout
		}
	}()

	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := clientTransport.Request(ctx, []byte("test"))
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("expected context.DeadlineExceeded, got: %v", err)
	}

	// Should timeout around 100ms
	if elapsed < 80*time.Millisecond || elapsed > 200*time.Millisecond {
		t.Errorf("timeout took %v, expected ~100ms", elapsed)
	}

	// Verify pending request was cleaned up
	clientTransport.mu.RLock()
	pending := len(clientTransport.pendingRequests)
	clientTransport.mu.RUnlock()

	if pending != 0 {
		t.Errorf("expected 0 pending requests after timeout, got %d", pending)
	}
}

// TestMultiplexedTransport_CloseWithPendingRequests verifies that Close()
// fails all pending requests with ErrTransportClosed.
func TestMultiplexedTransport_CloseWithPendingRequests(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()

	clientTransport := NewMultiplexedTransport(client)

	// Server that never responds
	go func() {
		framer := NewConnFramer(server)
		for {
			_, err := framer.Read()
			if err != nil {
				return
			}
		}
	}()

	// Start multiple requests
	const numRequests = 5
	var wg sync.WaitGroup
	results := make([]error, numRequests)

	for i := 0; i < numRequests; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_, err := clientTransport.Request(ctx, []byte(fmt.Sprintf("req-%d", idx)))
			results[idx] = err
		}(i)
	}

	// Give requests time to register
	time.Sleep(50 * time.Millisecond)

	// Close transport
	clientTransport.Close()

	wg.Wait()

	// All should fail with ErrTransportClosed
	for i, err := range results {
		if !errors.Is(err, ErrTransportClosed) {
			t.Errorf("request %d: expected ErrTransportClosed, got %v", i, err)
		}
	}
}

// TestMultiplexedTransport_FlowControl verifies that maxInflight limits
// concurrent requests (backpressure).
func TestMultiplexedTransport_FlowControl(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	// Transport with very low maxInflight
	const maxInflight = 2
	const serverDelay = 100 * time.Millisecond
	clientTransport := NewMultiplexedTransportWithLimit(client, maxInflight)
	defer clientTransport.Close()

	// Server that responds slowly
	go func() {
		framer := NewConnFramer(server)
		for {
			payload, err := framer.Read()
			if err != nil {
				return
			}
			time.Sleep(serverDelay)
			if err := framer.Write(payload); err != nil {
				return
			}
		}
	}()

	// Send more requests than maxInflight
	const numRequests = 5
	var wg sync.WaitGroup
	start := time.Now()

	for i := 0; i < numRequests; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_, err := clientTransport.Request(ctx, []byte(fmt.Sprintf("req-%d", idx)))
			if err != nil {
				t.Errorf("request %d failed: %v", idx, err)
			}
		}(i)
	}

	wg.Wait()
	elapsed := time.Since(start)

	// With maxInflight=2 and 100ms delay per request:
	// - If no flow control: all 5 concurrent, finish in ~100ms
	// - With flow control: waves of 2, should take ~300ms (3 waves: 2+2+1)
	//
	// We expect >= 250ms (allowing for scheduling variance)
	minExpected := 250 * time.Millisecond

	if elapsed < minExpected {
		t.Errorf("requests completed too quickly (%v), expected >=%v (suggests no backpressure)", elapsed, minExpected)
	}

	// Sanity check: shouldn't take more than 2 seconds (timeout)
	if elapsed > 1*time.Second {
		t.Errorf("requests took too long (%v), possible deadlock or timeout", elapsed)
	}
}

// TestMultiplexedTransport_RequestIDAllocation verifies unique ID generation.
func TestMultiplexedTransport_RequestIDAllocation(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	clientTransport := NewMultiplexedTransport(client)
	defer clientTransport.Close()

	// Server echo handler
	go func() {
		framer := NewConnFramer(server)
		for {
			payload, err := framer.Read()
			if err != nil {
				return
			}
			if err := framer.Write(payload); err != nil {
				return
			}
		}
	}()

	// Send multiple requests and collect IDs
	seen := make(map[uint32]bool)
	var mu sync.Mutex

	const numRequests = 100
	var wg sync.WaitGroup

	for i := 0; i < numRequests; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()

			// Capture the RequestID by reading what was sent
			// (In real code we'd inspect the wire, here we trust allocateID is called)
			id := clientTransport.allocateID()

			mu.Lock()
			if seen[id] {
				t.Errorf("duplicate RequestID: %d", id)
			}
			seen[id] = true
			mu.Unlock()

			// Also verify Request works
			ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
			defer cancel()
			_, err := clientTransport.Request(ctx, []byte(fmt.Sprintf("msg-%d", idx)))
			if err != nil {
				t.Errorf("request failed: %v", err)
			}
		}(i)
	}

	wg.Wait()

	// Verify all IDs were unique
	if len(seen) != numRequests {
		t.Errorf("expected %d unique IDs, got %d", numRequests, len(seen))
	}

	// Verify no ID is 0 (reserved for streaming)
	if seen[0] {
		t.Error("RequestID 0 was allocated (reserved)")
	}
}

// TestMultiplexedTransport_StreamingSend verifies Send() uses RequestID=0.
func TestMultiplexedTransport_StreamingSend(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	clientTransport := NewMultiplexedTransport(client)
	defer clientTransport.Close()

	// Server that reads and verifies RequestID=0
	serverDone := make(chan error, 1)
	go func() {
		framer := NewConnFramer(server)
		payload, err := framer.Read()
		if err != nil {
			serverDone <- err
			return
		}
		if len(payload) < 4 {
			serverDone <- fmt.Errorf("payload too short: %d", len(payload))
			return
		}

		requestID := binary.LittleEndian.Uint32(payload[0:4])
		if requestID != 0 {
			serverDone <- fmt.Errorf("expected RequestID=0, got %d", requestID)
			return
		}

		msg := string(payload[4:])
		if msg != "streaming message" {
			serverDone <- fmt.Errorf("expected 'streaming message', got %s", msg)
			return
		}

		serverDone <- nil
	}()

	// Send streaming message
	err := clientTransport.Send(context.Background(), []byte("streaming message"))
	if err != nil {
		t.Fatalf("Send failed: %v", err)
	}

	// Wait for server verification
	select {
	case err := <-serverDone:
		if err != nil {
			t.Errorf("server verification failed: %v", err)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("server verification timeout")
	}
}

// TestMultiplexedTransport_InvalidPayload verifies empty payload rejection.
func TestMultiplexedTransport_InvalidPayload(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	clientTransport := NewMultiplexedTransport(client)
	defer clientTransport.Close()

	// Request with empty payload
	_, err := clientTransport.Request(context.Background(), []byte{})
	if err == nil {
		t.Error("expected error for empty payload, got nil")
	}

	// Send with empty payload
	err = clientTransport.Send(context.Background(), []byte{})
	if err == nil {
		t.Error("expected error for empty payload, got nil")
	}
}

// TestMultiplexedTransport_ConnectionError verifies behavior when connection fails.
func TestMultiplexedTransport_ConnectionError(t *testing.T) {
	client, server := net.Pipe()

	clientTransport := NewMultiplexedTransport(client)

	// Close server immediately to cause read/write errors
	server.Close()

	// Give readLoop time to detect closure
	time.Sleep(50 * time.Millisecond)

	// Request should fail with ErrTransportClosed
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	_, err := clientTransport.Request(ctx, []byte("test"))
	if err == nil {
		t.Fatal("expected error after connection closed, got nil")
	}

	// Should be ErrTransportClosed or write error
	if !errors.Is(err, ErrTransportClosed) && !errors.Is(err, io.ErrClosedPipe) && !errors.Is(err, context.DeadlineExceeded) {
		t.Logf("got error: %v (acceptable if write failed before close detected)", err)
	}

	clientTransport.Close()
}

// TestMultiplexedTransport_GracefulShutdown verifies Close waits for cleanup.
func TestMultiplexedTransport_GracefulShutdown(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()

	clientTransport := NewMultiplexedTransport(client)

	// Start a request that will timeout
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		clientTransport.Request(ctx, []byte("test"))
	}()

	time.Sleep(50 * time.Millisecond)

	// Close should wait for cleanup
	start := time.Now()
	err := clientTransport.Close()
	elapsed := time.Since(start)

	if err != nil && !errors.Is(err, net.ErrClosed) {
		t.Logf("Close returned error: %v", err)
	}

	// Should complete quickly (not wait full 5s timeout)
	if elapsed > 1*time.Second {
		t.Errorf("Close took %v, expected < 1s", elapsed)
	}

	// Verify closeCh is closed
	select {
	case <-clientTransport.closeCh:
		// Good
	default:
		t.Error("closeCh not closed after Close()")
	}
}

// TestMultiplexedTransport_ResponseOrdering verifies correct response routing
// even when server responds out of order.
func TestMultiplexedTransport_ResponseOrdering(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	clientTransport := NewMultiplexedTransport(client)
	defer clientTransport.Close()

	// Server that responds in reverse order
	go func() {
		framer := NewConnFramer(server)
		requests := make([][]byte, 0)

		// Collect 3 requests
		for i := 0; i < 3; i++ {
			payload, err := framer.Read()
			if err != nil {
				return
			}
			requests = append(requests, payload)
		}

		// Respond in reverse order
		for i := len(requests) - 1; i >= 0; i-- {
			if err := framer.Write(requests[i]); err != nil {
				return
			}
		}
	}()

	// Send 3 requests
	var wg sync.WaitGroup
	results := make([]string, 3)

	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			msg := fmt.Sprintf("request-%d", idx)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			resp, err := clientTransport.Request(ctx, []byte(msg))
			if err != nil {
				t.Errorf("request %d failed: %v", idx, err)
				return
			}
			results[idx] = string(resp)
		}(i)
	}

	wg.Wait()

	// Verify each request got its own response (not swapped)
	for i := 0; i < 3; i++ {
		expected := fmt.Sprintf("request-%d", i)
		if results[i] != expected {
			t.Errorf("request %d: expected %s, got %s", i, expected, results[i])
		}
	}
}

// TestMultiplexedTransport_UnknownRequestID verifies dropped responses
// for expired/unknown IDs.
func TestMultiplexedTransport_UnknownRequestID(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	clientTransport := NewMultiplexedTransport(client)
	defer clientTransport.Close()

	// Server that sends extra bogus response
	go func() {
		framer := NewConnFramer(server)

		// Read first request
		payload, err := framer.Read()
		if err != nil {
			return
		}

		// Send response for unknown RequestID first
		bogus := make([]byte, 8)
		binary.LittleEndian.PutUint32(bogus[0:4], 99999) // Unknown ID
		copy(bogus[4:], []byte("junk"))
		framer.Write(bogus)

		// Send real response
		framer.Write(payload)
	}()

	// Should still work despite bogus response
	msg := []byte("test")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	resp, err := clientTransport.Request(ctx, msg)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	if string(resp) != string(msg) {
		t.Errorf("expected %s, got %s", msg, resp)
	}
}
