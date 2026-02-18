package transport

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

const DefaultMaxInflight = 128

var ErrTransportClosed = errors.New("transport: closed")

type MultiplexedTransport struct {
	conn            net.Conn
	framer          *Framer
	pendingRequests map[uint32]chan response
	nextId          atomic.Uint32
	mu              sync.RWMutex // protects pendingRequests
	writeMu         sync.Mutex   // protects concurrent writes to framer
	inflightSem     chan struct{}
	closeCh         chan struct{}
	readLoopOnce    sync.Once // ensures readLoop starts only once
	closeOnce       sync.Once // ensures cleanup happens only once

	// lastRecvId holds the transport-level requestId from the last Receive() call.
	// Send() echoes it back (then resets to 0). This enables server-side response
	// routing: when a client sends via Request() with a non-zero requestId,
	// the server's Receive() captures it and the subsequent Send() echoes it
	// so the client's readLoop can route the response.
	lastRecvId atomic.Uint32
}

type response struct {
	payload []byte
	err     error
}

func NewMultiplexedTransport(conn net.Conn) *MultiplexedTransport {
	return NewMultiplexedTransportWithLimit(conn, DefaultMaxInflight)
}

func NewMultiplexedTransportWithLimit(conn net.Conn, maxInflight int) *MultiplexedTransport {
	if maxInflight <= 0 {
		maxInflight = DefaultMaxInflight
	}

	return &MultiplexedTransport{
		conn:            conn,
		framer:          NewConnFramer(conn),
		pendingRequests: map[uint32]chan response{},
		inflightSem:     make(chan struct{}, maxInflight),
		closeCh:         make(chan struct{}),
		// readLoop starts lazily on first Request() to avoid conflict with Receive()
	}
}

func (t *MultiplexedTransport) allocateID() uint32 {
	for {
		id := t.nextId.Add(1)

		// Check for collision (only possible after wraparound at 2^32)
		t.mu.RLock()
		_, exists := t.pendingRequests[id]
		t.mu.RUnlock()

		if !exists {
			return id
		}
		// Collision detected (extremely rare) - try next ID
	}
}

func (t *MultiplexedTransport) Send(ctx context.Context, payload []byte) error {
	if len(payload) == 0 {
		return fmt.Errorf("invalid payload")
	}

	// Echo the requestId from the last Receive() call, then reset.
	// Streaming messages (no prior Receive) use 0; request-response
	// messages echo the client's non-zero requestId for readLoop routing.
	id := t.lastRecvId.Swap(0)

	buf := make([]byte, 4+len(payload))
	binary.LittleEndian.PutUint32(buf, id)
	copy(buf[4:], payload)

	t.writeMu.Lock()
	defer t.writeMu.Unlock()

	if deadline, ok := ctx.Deadline(); ok {
		return t.framer.WriteWithDeadline(buf, deadline)
	}
	return t.framer.Write(buf)
}

func (t *MultiplexedTransport) Receive(ctx context.Context) ([]byte, error) {
	id, payload, err := t.receiveInternal(ctx)
	if err == nil {
		t.lastRecvId.Store(id)
	}
	return payload, err
}

func (t *MultiplexedTransport) receiveInternal(ctx context.Context) (uint32, []byte, error) {
	var payload []byte
	var err error

	if deadline, ok := ctx.Deadline(); ok {
		payload, err = t.framer.ReadWithDeadline(deadline)
	} else {
		payload, err = t.framer.Read()
	}

	if err != nil {
		return 0, nil, err
	}

	if len(payload) <= 4 {
		return 0, nil, fmt.Errorf("transport: invalid payload, len=%d", len(payload))
	}

	requestId := binary.LittleEndian.Uint32(payload[0:4])
	return requestId, payload[4:], nil
}

func (t *MultiplexedTransport) RemoteAddr() string {
	return t.conn.RemoteAddr().String()
}

func (t *MultiplexedTransport) Request(ctx context.Context, payload []byte) ([]byte, error) {
	if len(payload) == 0 {
		return nil, fmt.Errorf("invalid payload")
	}

	// Start readLoop on first Request() call (lazy initialization)
	t.readLoopOnce.Do(func() {
		go t.startReadLoop()
	})

	select {
	case t.inflightSem <- struct{}{}:
		defer func() { <-t.inflightSem }()
	case <-t.closeCh:
		return nil, ErrTransportClosed
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	requestId := t.allocateID()
	buf := make([]byte, 4+len(payload))
	binary.LittleEndian.PutUint32(buf, requestId)
	copy(buf[4:], payload)

	// Register response channel BEFORE sending
	respCh := make(chan response, 1)
	t.mu.Lock()
	t.pendingRequests[requestId] = respCh
	t.mu.Unlock()

	// Write with deadline if context has one
	if deadline, ok := ctx.Deadline(); ok {
		t.writeMu.Lock()
		err := t.framer.WriteWithDeadline(buf, deadline)
		t.writeMu.Unlock()
		if err != nil {
			t.mu.Lock()
			delete(t.pendingRequests, requestId)
			t.mu.Unlock()
			return nil, err
		}
	} else {
		t.writeMu.Lock()
		err := t.framer.Write(buf)
		t.writeMu.Unlock()
		if err != nil {
			t.mu.Lock()
			delete(t.pendingRequests, requestId)
			t.mu.Unlock()
			return nil, err
		}
	}

	// Wait for response
	select {
	case resp := <-respCh:
		return resp.payload, resp.err
	case <-ctx.Done():
		t.mu.Lock()
		delete(t.pendingRequests, requestId)
		t.mu.Unlock()
		return nil, ctx.Err()
	case <-t.closeCh:
		return nil, ErrTransportClosed
	}
}

func (t *MultiplexedTransport) startReadLoop() {
	defer t.cleanup()

	for {
		// Read next message
		payload, err := t.framer.Read()
		if err != nil {
			// Connection closed or error
			return
		}

		if len(payload) < 4 {
			// Invalid message, skip
			continue
		}

		// Extract RequestID
		requestID := binary.LittleEndian.Uint32(payload[0:4])
		payloadData := payload[4:]

		if requestID == 0 {
			// Streaming message (replication) - no response expected
			// These are handled by direct Receive() calls
			// Skip in readLoop (or queue if implementing full multiplexing)
			continue
		}

		// Route to pending request
		t.mu.RLock()
		ch, exists := t.pendingRequests[requestID]
		t.mu.RUnlock()

		if exists {
			t.mu.Lock()
			delete(t.pendingRequests, requestID)
			t.mu.Unlock()

			ch <- response{payload: payloadData, err: nil}
		}
		// If not exists, response for unknown/expired request - drop
	}
}

// cleanup closes closeCh and fails all pending requests. Called by readLoop defer or Close().
func (t *MultiplexedTransport) cleanup() {
	t.closeOnce.Do(func() {
		close(t.closeCh)
		// Fail all pending requests
		t.mu.Lock()
		for id, ch := range t.pendingRequests {
			ch <- response{err: ErrTransportClosed}
			delete(t.pendingRequests, id)
		}
		t.mu.Unlock()
	})
}

func (t *MultiplexedTransport) Close() error {
	// Close connection first
	err := t.conn.Close()

	// Trigger cleanup (safe to call multiple times)
	t.cleanup()

	return err
}

func DialMultiplexedTransport(network, addr string, timeout time.Duration) (StreamTransport, RequestTransport, error) {
	conn, err := net.DialTimeout(network, addr, timeout)
	if err != nil {
		return nil, nil, fmt.Errorf("dial %s: %w", addr, err)
	}

	t := NewMultiplexedTransport(conn)
	return t, t, nil
}
