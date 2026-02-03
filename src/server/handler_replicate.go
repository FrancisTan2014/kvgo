package server

import "kvgo/protocol"

func (s *Server) handleReplicate(ctx *RequestContext) error {
	if s.isReplica {
		s.log().Warn("REPLICATE rejected: node is replica")
		return s.writeResponse(ctx.Framer, protocol.Response{Status: protocol.StatusError})
	}

	req, err := protocol.DecodeRequest(ctx.Payload)
	if err != nil {
		s.log().Error("failed to decode replicate request", "replica", ctx.Conn.RemoteAddr())
		return s.writeResponse(ctx.Framer, protocol.Response{Status: protocol.StatusError})
	}

	replid := string(req.Value)
	rc := newReplicaConn(ctx.Conn, req.Seq, replid)
	s.mu.Lock()
	s.replicas[ctx.Conn] = rc
	s.mu.Unlock()
	// serveReplica takes over this connection - call directly, don't return
	s.serveReplica(rc)
	// serveReplica returns when replica disconnects - connection will be closed by caller
	return s.writeResponse(ctx.Framer, protocol.Response{Status: protocol.StatusNoReply})
}
