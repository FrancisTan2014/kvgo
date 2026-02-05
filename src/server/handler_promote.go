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

	// Generate new replid — this node is starting a new timeline
	s.replid = generateReplID()
	s.seq.Store(0)

	if s.replCancel != nil {
		s.replCancel() // signal loop to exit
	}
	if s.primary != nil {
		_ = s.primary.Close()
	}

	// Start backlog trimmer for new primary role
	s.startBacklogTrimmer()

	if err := s.storeState(); err != nil {
		s.log().Error("failed to store new replid after promotion", "error", err)
	}

	return nil
}
