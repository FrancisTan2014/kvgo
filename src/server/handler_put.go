package server

import "kvgo/protocol"

func (s *Server) handlePut(ctx *RequestContext) error {
	req, err := protocol.DecodeRequest(ctx.Payload)
	if err != nil {
		return err
	}

	key := string(req.Key)
	if s.isReplica {
		// Replicas reject direct writes from clients.
		s.log().Warn("PUT rejected on replica", "key", key)
		return s.writeResponse(ctx.Framer, protocol.Response{Status: protocol.StatusError})
	}

	if err := s.db.Put(key, req.Value); err != nil {
		s.log().Error("PUT failed", "key", key, "error", err)
		return s.writeResponse(ctx.Framer, protocol.Response{Status: protocol.StatusError})
	}
	seq := s.seq.Add(1)

	req.Seq = seq
	payload, err := protocol.EncodeRequest(req)
	if err != nil {
		s.log().Error("failed to encode replica request", "error", err)
		return s.writeResponse(ctx.Framer, protocol.Response{Status: protocol.StatusError})
	}

	s.writeBacklog(payload)
	s.forwardToReplicas(payload, seq)
	return s.writeResponse(ctx.Framer, protocol.Response{Status: protocol.StatusOK})
}
