package server

// handlePong processes PONG responses from replicas (heartbeat acknowledgment).
//
// Replica → Primary: Sends PONG in response to PING
func (s *Server) handlePong(ctx *RequestContext) error {
	// Simply acknowledge the heartbeat - connection is alive
	// No response needed (PONG is the response)
	return nil
}
