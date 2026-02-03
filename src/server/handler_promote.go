package server

import "kvgo/protocol"

func (s *Server) handlePromote(ctx *RequestContext) error {
	if !s.isReplica {
		s.log().Warn("PROMOTE rejected: already primary")
		return s.writeResponse(ctx.Framer, protocol.Response{Status: protocol.StatusError})
	} else {
		if err := s.promote(); err != nil {
			s.log().Error("PROMOTE failed", "error", err)
			return s.writeResponse(ctx.Framer, protocol.Response{Status: protocol.StatusError})
		} else {
			s.log().Info("promoted to primary")
			return s.writeResponse(ctx.Framer, protocol.Response{Status: protocol.StatusOK})
		}
	}
}

func (s *Server) promote() error {
	s.isReplica = false
	if s.replCancel != nil {
		s.replCancel() // signal loop to exit
	}
	if s.primary != nil {
		return s.primary.Close()
	}
	return nil
}
