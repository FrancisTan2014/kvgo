package server

import (
	"kvgo/protocol"
	"net"
	"time"
)

func (s *Server) handleRequest(conn net.Conn) {
	f := protocol.NewConnFramer(conn)
	f.SetMaxPayload(s.opts.MaxFrameSize)

	for {
		var payload []byte
		var err error
		if s.opts.ReadTimeout > 0 {
			payload, err = f.ReadWithTimeout(s.opts.ReadTimeout)
		} else {
			payload, err = f.Read()
		}
		if err != nil {
			return
		}

		req, err := protocol.DecodeRequest(payload)
		if err != nil {
			// Protocol error: reply with StatusError and close.
			_ = s.writeResponse(f, protocol.Response{Status: protocol.StatusError})
			return
		}

		resp := s.handle(req, conn)
		if err := s.writeResponse(f, resp); err != nil {
			return
		}
	}
}

func (s *Server) receiveFromPrimary(f *protocol.Framer) {
	for {
		payload, err := f.Read()
		if err != nil {
			s.logf("replication stream closed: %v", err)
			return
		}

		req, err := protocol.DecodeRequest(payload)
		if err != nil {
			s.logf("replication decode error: %v", err)
			return
		}

		// Apply write locally (fire-and-forget from primary's perspective).
		if req.Op == protocol.OpPut {
			key := string(req.Key)
			if err := s.db.Put(key, req.Value); err != nil {
				s.logf("replication PUT %q failed: %v (seq=%d)", key, err, req.Seq)
			} else {
				s.lastSeq.Store(req.Seq)
				s.logf("replication PUT %q <- %d bytes (seq=%d)", key, len(req.Value), req.Seq)
			}
		}
	}
}

func (s *Server) handle(req protocol.Request, conn net.Conn) protocol.Response {
	key := string(req.Key)
	switch req.Op {
	case protocol.OpGet:
		val, ok := s.db.Get(key)
		if !ok {
			s.logf("GET %q -> not found", key)
			return protocol.Response{Status: protocol.StatusNotFound}
		}
		s.logf("GET %q -> %d bytes", key, len(val))
		// Avoid aliasing engine memory.
		copyVal := append([]byte(nil), val...)
		return protocol.Response{Status: protocol.StatusOK, Value: copyVal}

	case protocol.OpPut:
		if s.isReplica {
			// Replicas reject direct writes from clients.
			s.logf("PUT %q rejected: replica is read-only", key)
			return protocol.Response{Status: protocol.StatusError}
		}
		if err := s.db.Put(key, req.Value); err != nil {
			s.logf("PUT %q -> error: %v", key, err)
			return protocol.Response{Status: protocol.StatusError}
		}
		seq := s.seq.Add(1)
		s.logf("PUT %q <- %d bytes (seq=%d)", key, len(req.Value), seq)
		s.forwardToReplicas(req, seq)
		return protocol.Response{Status: protocol.StatusOK}

	case protocol.OpReplicate:
		if s.isReplica {
			s.logf("REPLICATE rejected: this node is a replica")
			return protocol.Response{Status: protocol.StatusError}
		}
		replicaLastSeq := req.Seq
		currentSeq := s.seq.Load()
		gap := currentSeq - replicaLastSeq
		rc := &replicaConn{
			conn:    conn,
			framer:  protocol.NewConnFramer(conn),
			sendCh:  make(chan []byte, replicaSendBuffer),
			lastSeq: replicaLastSeq,
		}
		s.mu.Lock()
		s.replicas[conn] = rc
		s.mu.Unlock()
		s.wg.Go(func() { s.replicaWriter(rc) })
		if gap > 0 {
			s.logf("REPLICATE registered replica %s (last_seq=%d, current=%d, gap=%d writes)",
				conn.RemoteAddr(), replicaLastSeq, currentSeq, gap)
		} else {
			s.logf("REPLICATE registered replica %s (seq=%d, no gap)", conn.RemoteAddr(), currentSeq)
		}
		return protocol.Response{Status: protocol.StatusOK}

	default:
		s.logf("unknown op %d", req.Op)
		return protocol.Response{Status: protocol.StatusError}
	}
}

func (s *Server) writeResponse(f *protocol.Framer, resp protocol.Response) error {
	payload, err := protocol.EncodeResponse(resp)
	if err != nil {
		return err
	}
	if s.opts.WriteTimeout > 0 {
		return f.WriteWithTimeout(payload, s.opts.WriteTimeout)
	}
	return f.Write(payload)
}

// forwardToReplicas sends a write to all connected replicas (non-blocking).
func (s *Server) forwardToReplicas(req protocol.Request, seq uint64) {
	req.Seq = seq
	payload, err := protocol.EncodeRequest(req)
	if err != nil {
		s.logf("failed to encode replica request: %v", err)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	for _, rc := range s.replicas {
		select {
		case rc.sendCh <- payload:
			// queued
		default:
			// channel full, replica is slow — drop the write
			s.logf("replica %s send buffer full, dropping write (seq=%d)", rc.conn.RemoteAddr(), seq)
		}
	}
}

// replicaWriter is a dedicated goroutine that drains sendCh and writes to the replica.
func (s *Server) replicaWriter(rc *replicaConn) {
	for payload := range rc.sendCh {
		if err := rc.conn.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
			s.logf("replica %s set deadline failed: %v", rc.conn.RemoteAddr(), err)
			break
		}
		if err := rc.framer.Write(payload); err != nil {
			s.logf("replica %s write failed: %v", rc.conn.RemoteAddr(), err)
			break
		}
	}

	// Clean up: remove from replicas map and close connection.
	s.mu.Lock()
	delete(s.replicas, rc.conn)
	s.mu.Unlock()
	_ = rc.conn.Close()
	s.logf("replica %s disconnected", rc.conn.RemoteAddr())
}
