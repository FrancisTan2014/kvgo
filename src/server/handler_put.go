package server

import (
	"kvgo/protocol"
	"kvgo/utils"
	"net"
	"time"
)

func (s *Server) handlePut(ctx *RequestContext) error {
	req := &ctx.Request

	key := string(req.Key)
	if s.isReplica {
		// Replicas reject direct writes from clients.
		s.log().Warn("PUT rejected on replica", "key", key)
		return s.responseStatusWithPrimaryAddress(ctx, protocol.StatusReadOnly)
	}

	if err := s.db.Put(key, req.Value); err != nil {
		s.log().Error("PUT failed", "key", key, "error", err)
		return s.responseStatusError(ctx)
	}
	seq := s.seq.Add(1)
	req.Seq = seq

	// Quorum write setup
	var state *quorumState
	if req.RequireQuorum {
		req.RequestId = utils.GenerateUniqueID() // Only generate ID for quorum writes
		state = &quorumState{
			needed:        s.computeQuorum() - 1, // -1 because primary already has write
			ackCh:         make(chan struct{}),
			ackedReplicas: make(map[net.Conn]struct{}),
		}
		s.quorumMu.Lock()
		s.quorumWrites[req.RequestId] = state
		s.quorumMu.Unlock()
	}

	payload, err := protocol.EncodeRequest(*req)
	if err != nil {
		s.log().Error("failed to encode replica request", "error", err)
		return s.responseStatusError(ctx)
	}

	s.forwardToReplicas(payload, req.Seq)
	s.appendBacklog(backlogEntry{size: len(payload), payload: payload, seq: seq})

	if req.RequireQuorum {
		defer func() {
			s.quorumMu.Lock()
			delete(s.quorumWrites, req.RequestId)
			s.quorumMu.Unlock()
		}()

		select {
		case <-state.ackCh:
			if state.failed.Load() {
				// NACK received - fail fast
				s.log().Error("quorum write failed due to NACK",
					"key", key,
					"request_id", req.RequestId,
					"seq", seq)
				return s.responseStatusError(ctx)
			}
			state.mu.Lock()
			acksReceived := state.ackCount
			state.mu.Unlock()
			s.log().Info("quorum write succeeded",
				"key", key,
				"request_id", req.RequestId,
				"seq", seq,
				"acks_received", acksReceived,
				"quorum_needed", state.needed)
			return s.writeResponse(ctx.Framer, protocol.Response{Status: protocol.StatusOK, Seq: seq})
		case <-time.After(s.opts.QuorumWriteTimeout):
			state.mu.Lock()
			acksReceived := state.ackCount
			state.mu.Unlock()
			s.log().Error("quorum write timeout",
				"key", key,
				"request_id", req.RequestId,
				"seq", seq,
				"acks_received", acksReceived,
				"quorum_needed", state.needed,
				"timeout", s.opts.QuorumWriteTimeout,
				"action", "write_rejected")
			return s.responseStatusError(ctx)
		}
	} else {
		return s.writeResponse(ctx.Framer, protocol.Response{Status: protocol.StatusOK, Seq: seq})
	}
}

func (s *Server) computeQuorum() int {
	s.mu.Lock()
	totalNodes := len(s.replicas) + 1 // +1 for primary itself
	s.mu.Unlock()
	return totalNodes/2 + 1
}

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
