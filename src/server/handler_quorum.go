package server

// ---------------------------------------------------------------------------
// Quorum Write Response Handlers (ACK/NACK from replicas)
// ---------------------------------------------------------------------------

// handleAck processes ACK messages from replicas for quorum writes.
// Uses per-request locking to avoid global serialization across concurrent writes.
func (s *Server) handleAck(ctx *RequestContext) error {
	// Fast path: lookup state with read lock
	s.quorumMu.RLock()
	state, ok := s.quorumWrites[ctx.Request.RequestId]
	s.quorumMu.RUnlock()

	if !ok {
		s.log().Warn("ACK arrived after quorum timeout or for unknown request",
			"request_id", ctx.Request.RequestId)
		return nil
	}

	// Per-request lock - different writes don't block each other
	state.mu.Lock()
	defer state.mu.Unlock()

	// Check if this specific replica already ACK'd (deduplication)
	if _, alreadyAcked := state.ackedReplicas[ctx.Conn]; alreadyAcked {
		s.log().Debug("duplicate ACK from same replica ignored",
			"request_id", ctx.Request.RequestId,
			"replica", ctx.Conn.RemoteAddr())
		return nil
	}

	// Mark this replica as having ACK'd
	state.ackedReplicas[ctx.Conn] = struct{}{}
	state.ackCount++

	if state.ackCount >= int32(state.needed) {
		state.closeOnce.Do(func() {
			close(state.ackCh)
		})
	}

	return nil
}

// handleNack processes NACK messages from replicas for failed quorum writes.
// NACK triggers immediate failure without waiting for timeout.
func (s *Server) handleNack(ctx *RequestContext) error {
	// Fast path: lookup state with read lock
	s.quorumMu.RLock()
	state, ok := s.quorumWrites[ctx.Request.RequestId]
	s.quorumMu.RUnlock()

	if !ok {
		s.log().Warn("NACK arrived after quorum timeout or for unknown request",
			"request_id", ctx.Request.RequestId)
		return nil
	}

	// Per-request lock - different writes don't block each other
	state.mu.Lock()
	defer state.mu.Unlock()

	// Check if this specific replica already responded (deduplication)
	if _, alreadyProcessed := state.ackedReplicas[ctx.Conn]; alreadyProcessed {
		s.log().Debug("duplicate NACK from same replica ignored",
			"request_id", ctx.Request.RequestId,
			"replica", ctx.Conn.RemoteAddr())
		return nil
	}

	// Mark this replica as having responded
	state.ackedReplicas[ctx.Conn] = struct{}{}

	s.log().Warn("replica NACK'd write",
		"request_id", ctx.Request.RequestId,
		"replica", ctx.Conn.RemoteAddr())

	// NACK means this replica failed - can't reach quorum
	// Close channel to fail fast instead of waiting for timeout
	state.closeOnce.Do(func() {
		state.failed.Store(true)
		close(state.ackCh)
	})

	return nil
}
