package server

import (
	"kvgo/protocol"
	"time"
)

// handlePing processes PING requests, which serve as heartbeat messages in replication.
//
// Primary → Replica: Sends PING with current seq periodically
func (s *Server) handlePing(ctx *RequestContext) error {
	req, err := protocol.DecodeRequest(ctx.Payload)
	if err != nil {
		s.log().Error("failed to decode PING request", "error", err)
		return s.writeResponse(ctx.Framer, protocol.Response{Status: protocol.StatusError})
	}

	if s.isReplica {
		s.lastHeartbeat = time.Now()
		s.primarySeq = req.Seq
	}
	return s.writeResponse(ctx.Framer, protocol.Response{Status: protocol.StatusPong})
}
