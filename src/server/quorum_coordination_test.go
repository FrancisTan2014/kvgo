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
// waitForQuorumWrite polls until a quorum write state is registered, returning its request ID.
func waitForQuorumWrite(s *Server) string {
	for i := 0; i < 200; i++ {
		time.Sleep(1 * time.Millisecond)
		s.quorumMu.Lock()
		for id := range s.quorumWrites {
			s.quorumMu.Unlock()
			return id
		}
		s.quorumMu.Unlock()
	}
	return ""
}

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
				QuorumWriteTimeout: 500 * time.Millisecond,
			},
		}
		s.seq.Store(100)

		// 3 replicas → 4-node cluster → quorum=3 → need 2 replica ACKs
		replica1 := &mockStreamTransport{address: "r1:6379"}
		replica2 := &mockStreamTransport{address: "r2:6379"}
		replica3 := &mockStreamTransport{address: "r3:6379"}

		s.replicas[replica1] = &replicaConn{transport: replica1}
		s.replicas[replica2] = &replicaConn{transport: replica2}
		s.replicas[replica3] = &replicaConn{transport: replica3}

		// Send 2 ACKs once quorum write state is registered
		go func() {
			requestId := waitForQuorumWrite(s)
			if requestId == "" {
				return
			}
			ackReq := protocol.Request{Cmd: protocol.CmdAck, RequestId: requestId}
			s.handleAck(&RequestContext{StreamTransport: replica1, Request: ackReq})
			s.handleAck(&RequestContext{StreamTransport: replica2, Request: ackReq})
		}()

		req := protocol.Request{
			Cmd:           protocol.CmdPut,
			Key:           []byte("key1"),
			Value:         []byte("value1"),
			RequireQuorum: true,
		}

		ctx := &RequestContext{
			StreamTransport: &mockStreamTransport{},
			Request:         req,
		}

		err := s.handlePut(ctx)
		if err != nil {
			t.Fatalf("handlePut: %v", err)
		}

		mockTrans := ctx.StreamTransport.(*mockStreamTransport)
		resp, _ := protocol.DecodeResponse(mockTrans.written)
		if resp.Status != protocol.StatusOK {
			t.Errorf("status = %d, want StatusOK", resp.Status)
		}

		val, ok := db.Get("key1")
		if !ok || string(val) != "value1" {
			t.Errorf("db.Get = %q, want %q", val, "value1")
		}
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
				QuorumWriteTimeout: 500 * time.Millisecond,
			},
		}
		s.seq.Store(100)

		replica1 := &mockStreamTransport{address: "r1:6379"}
		replica2 := &mockStreamTransport{address: "r2:6379"}

		s.replicas[replica1] = &replicaConn{transport: replica1}
		s.replicas[replica2] = &replicaConn{transport: replica2}

		// Send NACK once quorum write state is registered
		go func() {
			requestId := waitForQuorumWrite(s)
			if requestId == "" {
				return
			}
			nackReq := protocol.Request{Cmd: protocol.CmdNack, RequestId: requestId}
			s.handleNack(&RequestContext{StreamTransport: replica1, Request: nackReq})
		}()

		req := protocol.Request{
			Cmd:           protocol.CmdPut,
			Key:           []byte("key1"),
			Value:         []byte("value1"),
			RequireQuorum: true,
		}

		ctx := &RequestContext{
			StreamTransport: &mockStreamTransport{},
			Request:         req,
		}

		err := s.handlePut(ctx)
		if err != nil {
			t.Fatalf("handlePut: %v", err)
		}

		mockTrans := ctx.StreamTransport.(*mockStreamTransport)
		resp, _ := protocol.DecodeResponse(mockTrans.written)
		if resp.Status != protocol.StatusError {
			t.Errorf("status = %d, want StatusError after NACK", resp.Status)
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
				QuorumWriteTimeout: 50 * time.Millisecond,
			},
		}
		s.seq.Store(100)

		// 2 replicas, neither ACKs → timeout
		replica1 := &mockStreamTransport{address: "r1:6379"}
		replica2 := &mockStreamTransport{address: "r2:6379"}

		s.replicas[replica1] = &replicaConn{transport: replica1}
		s.replicas[replica2] = &replicaConn{transport: replica2}

		req := protocol.Request{
			Cmd:           protocol.CmdPut,
			Key:           []byte("key1"),
			Value:         []byte("value1"),
			RequireQuorum: true,
		}

		ctx := &RequestContext{
			StreamTransport: &mockStreamTransport{},
			Request:         req,
		}

		err := s.handlePut(ctx)
		if err != nil {
			t.Fatalf("handlePut: %v", err)
		}

		mockTrans := ctx.StreamTransport.(*mockStreamTransport)
		resp, _ := protocol.DecodeResponse(mockTrans.written)
		if resp.Status != protocol.StatusError {
			t.Errorf("status = %d, want StatusError after timeout", resp.Status)
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

		mocks := map[string]transport.RequestTransport{
			"r1": &mockRequestTransport{response: r1Response},
			"r2": &mockRequestTransport{response: r2Response},
		}
		pm := NewPeerManager(func(addr string) (transport.RequestTransport, error) {
			return mocks[addr], nil
		}, noopLogger)
		pm.SavePeers([]string{"r1", "r2"})

		s := &Server{
			db:            db,
			isReplica:     true,
			peerManager:   pm,
			lastHeartbeat: time.Now(),
			primarySeq:    100,
			opts: Options{
				QuorumReadTimeout:     200 * time.Millisecond,
				ReplicaStaleHeartbeat: 5 * time.Second,
				ReplicaStaleLag:       1000,
			},
		}
		s.lastSeq.Store(100)

		req := protocol.Request{
			Cmd:           protocol.CmdGet,
			Key:           []byte("key1"),
			RequireQuorum: true,
		}

		ctx := &RequestContext{
			StreamTransport: &mockStreamTransport{},
			Request:         req,
		}

		err := s.handleGet(ctx)
		if err != nil {
			t.Fatalf("handleGet: %v", err)
		}

		// Verify response sent (mock transport captured it)
		mockTrans := ctx.StreamTransport.(*mockStreamTransport)
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

		mocks := map[string]transport.RequestTransport{
			"r1": &mockRequestTransport{response: r1Response},
			"r2": &mockRequestTransport{response: r2Response},
			"r3": &mockRequestTransport{err: errors.New("network error")},
		}
		pm := NewPeerManager(func(addr string) (transport.RequestTransport, error) {
			m := mocks[addr]
			if m.(*mockRequestTransport).err != nil {
				return nil, m.(*mockRequestTransport).err
			}
			return m, nil
		}, noopLogger)
		pm.SavePeers([]string{"r1", "r2", "r3"})

		s := &Server{
			db:            db,
			isReplica:     true,
			peerManager:   pm,
			lastHeartbeat: time.Now(),
			primarySeq:    100,
			opts: Options{
				QuorumReadTimeout:     200 * time.Millisecond,
				ReplicaStaleHeartbeat: 5 * time.Second,
				ReplicaStaleLag:       1000,
			},
		}
		s.lastSeq.Store(100)

		req := protocol.Request{
			Cmd:           protocol.CmdGet,
			Key:           []byte("key1"),
			RequireQuorum: true,
		}

		ctx := &RequestContext{
			StreamTransport: &mockStreamTransport{},
			Request:         req,
		}

		err := s.handleGet(ctx)
		if err != nil {
			t.Fatalf("handleGet: %v", err)
		}

		mockTrans := ctx.StreamTransport.(*mockStreamTransport)
		resp, _ := protocol.DecodeResponse(mockTrans.written)
		if resp.Status != protocol.StatusOK {
			t.Errorf("status = %v, want StatusOK (quorum reached despite 1 failure)", resp.Status)
		}
	})

	t.Run("timeout → quorum failed", func(t *testing.T) {
		dir := t.TempDir()
		db, _ := engine.NewDB(dir, context.Background())
		defer db.Close()

		mocks := map[string]transport.RequestTransport{
			"r1": &mockRequestTransport{delay: 200 * time.Millisecond},
			"r2": &mockRequestTransport{delay: 200 * time.Millisecond},
		}
		pm := NewPeerManager(func(addr string) (transport.RequestTransport, error) {
			return mocks[addr], nil
		}, noopLogger)
		pm.SavePeers([]string{"r1", "r2"})

		s := &Server{
			db:            db,
			isReplica:     true,
			peerManager:   pm,
			lastHeartbeat: time.Now(),
			primarySeq:    100,
			opts: Options{
				QuorumReadTimeout:     50 * time.Millisecond, // short
				ReplicaStaleHeartbeat: 5 * time.Second,
				ReplicaStaleLag:       1000,
			},
		}
		s.lastSeq.Store(100)

		req := protocol.Request{
			Cmd:           protocol.CmdGet,
			Key:           []byte("key1"),
			RequireQuorum: true,
		}

		ctx := &RequestContext{
			StreamTransport: &mockStreamTransport{},
			Request:         req,
		}

		err := s.handleGet(ctx)
		if err != nil {
			t.Fatalf("handleGet: %v", err)
		}

		mockTrans := ctx.StreamTransport.(*mockStreamTransport)
		resp, _ := protocol.DecodeResponse(mockTrans.written)
		if resp.Status != protocol.StatusQuorumFailed {
			t.Errorf("status = %v, want StatusQuorumFailed", resp.Status)
		}
	})
}
