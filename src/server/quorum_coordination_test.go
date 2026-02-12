package server

import (
	"context"
	"errors"
	"kvgo/engine"
	"kvgo/protocol"
	"kvgo/transport"
	"testing"
	"time"
)

// TestQuorumWriteCoordination tests the full quorum write flow with mock transports
func TestQuorumWriteCoordination(t *testing.T) {
	t.Run("majority ACKs → success", func(t *testing.T) {
		dir := t.TempDir()
		db, _ := engine.NewDB(dir, context.Background())
		defer db.Close()

		s := &Server{
			db:           db,
			isReplica:    false,
			replicas:     make(map[transport.StreamTransport]*replicaConn),
			quorumWrites: make(map[string]*quorumWriteState),
			opts: Options{
				QuorumWriteTimeout: 200 * time.Millisecond,
			},
		}
		s.seq.Store(100)

		// Create 3 mock replicas (need 2 ACKs for quorum in 4-node cluster)
		replica1 := &mockStreamTransport{shouldACK: true}
		replica2 := &mockStreamTransport{shouldACK: true}
		replica3 := &mockStreamTransport{shouldACK: false, delay: 500 * time.Millisecond} // slow, won't ACK in time

		s.replicas[replica1] = &replicaConn{transport: replica1}
		s.replicas[replica2] = &replicaConn{transport: replica2}
		s.replicas[replica3] = &replicaConn{transport: replica3}

		// Quorum write
		req := protocol.Request{
			Cmd:           protocol.CmdPut,
			Key:           []byte("key1"),
			Value:         []byte("value1"),
			RequireQuorum: true,
		}

		ctx := &RequestContext{
			Transport: &mockStreamTransport{},
			Request:   req,
		}

		err := s.handlePut(ctx)
		if err != nil {
			t.Fatalf("handlePut: %v", err)
		}

		// Verify data written
		val, ok := db.Get("key1")
		if !ok || string(val) != "value1" {
			t.Errorf("PUT failed: got %q, want %q", val, "value1")
		}

		// Verify seq incremented
		if s.seq.Load() != 101 {
			t.Errorf("seq = %d, want 101", s.seq.Load())
		}
	})

	t.Run("NACK from replica → failure", func(t *testing.T) {
		dir := t.TempDir()
		db, _ := engine.NewDB(dir, context.Background())
		defer db.Close()

		s := &Server{
			db:           db,
			isReplica:    false,
			replicas:     make(map[transport.StreamTransport]*replicaConn),
			quorumWrites: make(map[string]*quorumWriteState),
			opts: Options{
				QuorumWriteTimeout: 200 * time.Millisecond,
			},
		}
		s.seq.Store(100)

		// One replica sends NACK
		replica1 := &mockStreamTransport{shouldACK: false, sendNACK: true}
		replica2 := &mockStreamTransport{shouldACK: true}

		s.replicas[replica1] = &replicaConn{transport: replica1}
		s.replicas[replica2] = &replicaConn{transport: replica2}

		req := protocol.Request{
			Cmd:           protocol.CmdPut,
			Key:           []byte("key1"),
			Value:         []byte("value1"),
			RequireQuorum: true,
		}

		ctx := &RequestContext{
			Transport: &mockStreamTransport{},
			Request:   req,
		}

		// Start NACK sender
		go func() {
			time.Sleep(10 * time.Millisecond)
			nackReq := protocol.Request{
				Cmd:       protocol.CmdNack,
				RequestId: replica1.lastRequestId,
			}
			s.handleNack(&RequestContext{
				Transport: replica1,
				Request:   nackReq,
			})
		}()

		err := s.handlePut(ctx)
		// Should timeout or get NACK
		if err == nil {
			t.Error("expected error for NACK, got nil")
		}
	})

	t.Run("timeout → failure", func(t *testing.T) {
		dir := t.TempDir()
		db, _ := engine.NewDB(dir, context.Background())
		defer db.Close()

		s := &Server{
			db:           db,
			isReplica:    false,
			replicas:     make(map[transport.StreamTransport]*replicaConn),
			quorumWrites: make(map[string]*quorumWriteState),
			opts: Options{
				QuorumWriteTimeout: 50 * time.Millisecond, // short timeout
			},
		}
		s.seq.Store(100)

		// All replicas slow (won't ACK in time)
		replica1 := &mockStreamTransport{shouldACK: true, delay: 200 * time.Millisecond}
		replica2 := &mockStreamTransport{shouldACK: true, delay: 200 * time.Millisecond}

		s.replicas[replica1] = &replicaConn{transport: replica1}
		s.replicas[replica2] = &replicaConn{transport: replica2}

		req := protocol.Request{
			Cmd:           protocol.CmdPut,
			Key:           []byte("key1"),
			Value:         []byte("value1"),
			RequireQuorum: true,
		}

		ctx := &RequestContext{
			Transport: &mockStreamTransport{},
			Request:   req,
		}

		err := s.handlePut(ctx)
		if err == nil {
			t.Error("expected timeout error, got nil")
		}
	})
}

// TestQuorumReadCoordination tests the full quorum read flow with mock transports
func TestQuorumReadCoordination(t *testing.T) {
	t.Run("majority responses → returns highest seq", func(t *testing.T) {
		dir := t.TempDir()
		db, _ := engine.NewDB(dir, context.Background())
		defer db.Close()
		db.Put("key1", []byte("value1"))

		s := &Server{
			db:             db,
			isReplica:      true,
			reachableNodes: make(map[string]transport.RequestTransport),
			opts: Options{
				QuorumReadTimeout: 200 * time.Millisecond,
			},
		}
		s.lastSeq.Store(100)

		// Mock 2 replicas with different seqs
		r1Response, _ := protocol.EncodeResponse(protocol.Response{
			Status: protocol.StatusOK,
			Value:  []byte("value1"),
			Seq:    105, // highest
		})
		r2Response, _ := protocol.EncodeResponse(protocol.Response{
			Status: protocol.StatusOK,
			Value:  []byte("value1"),
			Seq:    103,
		})

		s.addReachableNode("r1", &mockRequestTransport{response: r1Response})
		s.addReachableNode("r2", &mockRequestTransport{response: r2Response})

		req := protocol.Request{
			Cmd:           protocol.CmdGet,
			Key:           []byte("key1"),
			RequireQuorum: true,
		}

		ctx := &RequestContext{
			Transport: &mockStreamTransport{},
			Request:   req,
		}

		err := s.handleGet(ctx)
		if err != nil {
			t.Fatalf("handleGet: %v", err)
		}

		// Verify response sent (mock transport captured it)
		mockTrans := ctx.Transport.(*mockStreamTransport)
		if mockTrans.written == nil {
			t.Fatal("no response written")
		}

		resp, _ := protocol.DecodeResponse(mockTrans.written)
		if resp.Status != protocol.StatusOK {
			t.Errorf("status = %v, want StatusOK", resp.Status)
		}
		if resp.Seq != 105 {
			t.Errorf("seq = %d, want 105 (highest)", resp.Seq)
		}
	})

	t.Run("partial failures → still reaches quorum", func(t *testing.T) {
		dir := t.TempDir()
		db, _ := engine.NewDB(dir, context.Background())
		defer db.Close()
		db.Put("key1", []byte("value1"))

		s := &Server{
			db:             db,
			isReplica:      true,
			reachableNodes: make(map[string]transport.RequestTransport),
			opts: Options{
				QuorumReadTimeout: 200 * time.Millisecond,
			},
		}
		s.lastSeq.Store(100)

		// 3 replicas: 2 succeed, 1 fails
		r1Response, _ := protocol.EncodeResponse(protocol.Response{
			Status: protocol.StatusOK,
			Value:  []byte("value1"),
			Seq:    102,
		})
		r2Response, _ := protocol.EncodeResponse(protocol.Response{
			Status: protocol.StatusOK,
			Value:  []byte("value1"),
			Seq:    104,
		})

		s.addReachableNode("r1", &mockRequestTransport{response: r1Response})
		s.addReachableNode("r2", &mockRequestTransport{response: r2Response})
		s.addReachableNode("r3", &mockRequestTransport{err: errors.New("network error")})

		req := protocol.Request{
			Cmd:           protocol.CmdGet,
			Key:           []byte("key1"),
			RequireQuorum: true,
		}

		ctx := &RequestContext{
			Transport: &mockStreamTransport{},
			Request:   req,
		}

		err := s.handleGet(ctx)
		if err != nil {
			t.Fatalf("handleGet: %v", err)
		}

		mockTrans := ctx.Transport.(*mockStreamTransport)
		resp, _ := protocol.DecodeResponse(mockTrans.written)
		if resp.Status != protocol.StatusOK {
			t.Errorf("status = %v, want StatusOK (quorum reached despite 1 failure)", resp.Status)
		}
	})

	t.Run("timeout → quorum failed", func(t *testing.T) {
		dir := t.TempDir()
		db, _ := engine.NewDB(dir, context.Background())
		defer db.Close()

		s := &Server{
			db:             db,
			isReplica:      true,
			reachableNodes: make(map[string]transport.RequestTransport),
			opts: Options{
				QuorumReadTimeout: 50 * time.Millisecond, // short
			},
		}
		s.lastSeq.Store(100)

		// All replicas timeout
		s.addReachableNode("r1", &mockRequestTransport{delay: 200 * time.Millisecond})
		s.addReachableNode("r2", &mockRequestTransport{delay: 200 * time.Millisecond})

		req := protocol.Request{
			Cmd:           protocol.CmdGet,
			Key:           []byte("key1"),
			RequireQuorum: true,
		}

		ctx := &RequestContext{
			Transport: &mockStreamTransport{},
			Request:   req,
		}

		err := s.handleGet(ctx)
		if err != nil {
			t.Fatalf("handleGet: %v", err)
		}

		mockTrans := ctx.Transport.(*mockStreamTransport)
		resp, _ := protocol.DecodeResponse(mockTrans.written)
		if resp.Status != protocol.StatusQuorumFailed {
			t.Errorf("status = %v, want StatusQuorumFailed", resp.Status)
		}
	})
}
