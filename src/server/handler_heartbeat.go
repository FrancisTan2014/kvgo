package server

import (
	"kvgo/protocol"
	"time"
)

// ---------------------------------------------------------------------------
// PING/PONG Handlers (Heartbeat)
// ---------------------------------------------------------------------------

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
		return ctx.StreamTransport.Send(pongPayload)
	}

	// Primary shouldn't receive PING (replicas send PONG)
	return nil
}

// handlePong processes PONG responses from replicas (heartbeat acknowledgment).
//
// Replica → Primary: Sends PONG in response to PING
func (s *Server) handlePong(ctx *RequestContext) error {
	// Simply acknowledge the heartbeat - connection is alive
	// No response needed (PONG is the response)
	return nil
}
