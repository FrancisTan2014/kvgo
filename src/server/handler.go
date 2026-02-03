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
		if resp.Status == protocol.StatusNoReply {
			// Handler took over connection (e.g., replication)
			return
		}
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
			return protocol.Response{Status: protocol.StatusNotFound}
		}
		// Avoid aliasing engine memory.
		copyVal := append([]byte(nil), val...)
		return protocol.Response{Status: protocol.StatusOK, Value: copyVal}

	case protocol.OpPut:
		if s.isReplica {
			// Replicas reject direct writes from clients.
			s.log().Warn("PUT rejected on replica", "key", key)
			return protocol.Response{Status: protocol.StatusError}
		}
		if err := s.db.Put(key, req.Value); err != nil {
			s.log().Error("PUT failed", "key", key, "error", err)
			return protocol.Response{Status: protocol.StatusError}
		}
		seq := s.seq.Add(1)

		req.Seq = seq
		payload, err := protocol.EncodeRequest(req)
		if err != nil {
			s.log().Error("failed to encode replica request", "error", err)
			return protocol.Response{Status: protocol.StatusError}
		}

		s.writeBacklog(payload)
		s.forwardToReplicas(payload, seq)
		return protocol.Response{Status: protocol.StatusOK}

	case protocol.OpReplicate:
		if s.isReplica {
			s.log().Warn("REPLICATE rejected: node is replica")
			return protocol.Response{Status: protocol.StatusError}
		}
		replid := string(req.Value)
		rc := newReplicaConn(conn, req.Seq, replid)
		s.mu.Lock()
		s.replicas[conn] = rc
		s.mu.Unlock()
		// serveReplica takes over this connection - call directly, don't return
		s.serveReplica(rc)
		// serveReplica returns when replica disconnects - connection will be closed by caller
		return protocol.Response{Status: protocol.StatusNoReply}

	case protocol.OpPing:
		return protocol.Response{Status: protocol.StatusPong}

	case protocol.OpPromote:
		if !s.isReplica {
			s.log().Warn("PROMOTE rejected: already primary")
			return protocol.Response{Status: protocol.StatusError}
		} else {
			if err := s.promote(); err != nil {
				s.log().Error("PROMOTE failed", "error", err)
				return protocol.Response{Status: protocol.StatusError}
			} else {
				s.log().Info("promoted to primary")
				return protocol.Response{Status: protocol.StatusOK}
			}
		}

	case protocol.OpReplicaOf:
		if err := s.relocate(string(req.Value)); err != nil {
			s.log().Error("REPLICAOF failed", "error", err)
			return protocol.Response{Status: protocol.StatusError}
		} else {
			return protocol.Response{Status: protocol.StatusOK}
		}

	default:
		s.log().Error("unknown operation", "op", req.Op)
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
