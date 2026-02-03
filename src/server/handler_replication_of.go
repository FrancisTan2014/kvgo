package server

import "kvgo/protocol"

func (s *Server) handleReplicaOf(ctx *RequestContext) error {
	req, err := protocol.DecodeRequest(ctx.Payload)
	if err != nil {
		return s.writeResponse(ctx.Framer, protocol.Response{Status: protocol.StatusError})
	}

	if err := s.relocate(string(req.Value)); err != nil {
		s.log().Error("REPLICAOF failed", "error", err)
		return s.writeResponse(ctx.Framer, protocol.Response{Status: protocol.StatusError})
	} else {
		return s.writeResponse(ctx.Framer, protocol.Response{Status: protocol.StatusOK})
	}
}
