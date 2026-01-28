package server

import (
	"kvgo/protocol"
	"net"
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
		rc := newReplicaConn(conn, replicaLastSeq)
		s.mu.Lock()
		s.replicas[conn] = rc
		s.mu.Unlock()
		s.wg.Go(func() { s.serveReplica(rc) })
		if gap > 0 {
			s.logf("REPLICATE registered replica %s (last_seq=%d, current=%d, gap=%d writes)",
				conn.RemoteAddr(), replicaLastSeq, currentSeq, gap)
		} else {
			s.logf("REPLICATE registered replica %s (seq=%d, no gap)", conn.RemoteAddr(), currentSeq)
		}
		return protocol.Response{Status: protocol.StatusOK}

	case protocol.OpPing:
		return protocol.Response{Status: protocol.StatusPong}

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
