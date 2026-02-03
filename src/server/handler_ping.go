package server

import "kvgo/protocol"

func (s *Server) handlePing(ctx *RequestContext) error {
	return s.writeResponse(ctx.Framer, protocol.Response{Status: protocol.StatusPong})
}
