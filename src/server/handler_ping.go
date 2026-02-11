package server

import (
	"kvgo/protocol"
	"time"
)

// handlePing processes PING requests, which serve as heartbeat messages in replication.
//
// Primary → Replica: Sends PING with current seq periodically
func (s *Server) handlePing(ctx *RequestContext) error {
	req := &ctx.Request

	if s.isReplica {
		s.lastHeartbeat = time.Now()
		s.primarySeq = req.Seq

		// Send PONG as a Request (not Response) so primary's handleRequest can decode it
		pongReq := protocol.Request{Cmd: protocol.CmdPong}
		pongPayload, _ := protocol.EncodeRequest(pongReq)
		return ctx.Framer.Write(pongPayload)
	}

	// Primary shouldn't receive PING (replicas send PONG)
	return nil
}
