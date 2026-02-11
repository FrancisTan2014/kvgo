package server

import (
	"kvgo/protocol"
	"kvgo/utils"
	"net"
	"time"
)

// ---------------------------------------------------------------------------
// GET Request Handler
// ---------------------------------------------------------------------------

func (s *Server) handleGet(ctx *RequestContext) error {
	req := &ctx.Request

	if s.isStaleness() {
		return s.responseStatusWithPrimaryAddress(ctx, protocol.StatusReplicaTooStale)
	}

	if req.WaitForSeq > 0 {
		return s.doStrongGet(ctx, req)
	}

	return s.doGet(ctx, req)
}

func (s *Server) isStaleness() bool {
	if !s.isReplica {
		return false
	}

	seqLag := s.primarySeq - s.lastSeq.Load()
	heartbeatAge := time.Since(s.lastHeartbeat)
	return seqLag > uint64(s.opts.ReplicaStaleLag) || heartbeatAge > s.opts.ReplicaStaleHeartbeat
}

func (s *Server) doGet(ctx *RequestContext, req *protocol.Request) error {
	key := string(req.Key)
	val, ok := s.db.Get(key)

	var resp protocol.Response
	if !ok {
		resp = protocol.Response{Status: protocol.StatusNotFound, Seq: s.lastSeq.Load()}
	} else {
		// Avoid aliasing engine memory.
		copyVal := append([]byte(nil), val...)
		resp = protocol.Response{Status: protocol.StatusOK, Value: copyVal, Seq: s.lastSeq.Load()}
	}

	return s.writeResponse(ctx.Framer, resp)
}

func (s *Server) doStrongGet(ctx *RequestContext, req *protocol.Request) error {
	maxDuration := 10 * time.Millisecond
	currentDuration := time.Millisecond

	startedAt := time.Now()
	for time.Since(startedAt) < s.opts.StrongReadTimeout {

		if s.lastSeq.Load() >= req.WaitForSeq {
			return s.doGet(ctx, req)
		}

		time.Sleep(currentDuration)
		currentDuration = min(currentDuration*2, maxDuration)
	}

	// Timeout: replica didn't catch up within StrongReadTimeout window
	s.log().Warn("strong read timeout: replica lagging, redirecting to primary",
		"requested_seq", req.WaitForSeq,
		"current_seq", s.lastSeq.Load(),
		"elapsed", time.Since(startedAt),
		"timeout", s.opts.StrongReadTimeout)

	return s.responseStatusWithPrimaryAddress(ctx, protocol.StatusReadOnly)
}

// ---------------------------------------------------------------------------
// PUT Request Handler
// ---------------------------------------------------------------------------

func (s *Server) handlePut(ctx *RequestContext) error {
	req := &ctx.Request
	key := string(req.Key)

	if s.isReplica {
		// Check if this PUT is from our primary (replication stream)
		s.mu.Lock()
		isPrimaryConn := (s.primary != nil && ctx.Conn == s.primary)
		s.mu.Unlock()

		if isPrimaryConn {
			// Replicated write from primary - apply locally
			return s.applyReplicatedPut(ctx)
		}

		// Direct client write to replica - reject with redirect
		s.log().Warn("PUT rejected on replica", "key", key)
		return s.responseStatusWithPrimaryAddress(ctx, protocol.StatusReadOnly)
	}

	// Primary processing
	return s.processPrimaryPut(ctx)
}

// applyReplicatedPut applies a write received from primary via replication.
// Sends ACK for quorum writes, NACK on failure.
func (s *Server) applyReplicatedPut(ctx *RequestContext) error {
	req := &ctx.Request
	key := string(req.Key)

	// Apply write locally
	if err := s.db.Put(key, req.Value); err != nil {
		s.log().Error("replication PUT failed", "key", key, "seq", req.Seq, "error", err)

		// Send NACK for quorum writes that failed
		if req.RequestId != "" {
			nackReq := protocol.Request{
				Cmd:       protocol.CmdNack,
				RequestId: req.RequestId,
			}
			nackPayload, _ := protocol.EncodeRequest(nackReq)
			if err := ctx.Framer.Write(nackPayload); err != nil {
				s.log().Error("failed to NACK quorum write",
					"request_id", req.RequestId,
					"error", err)
			}
		}
		return err
	}

	// Send ACK only for quorum writes (those with RequestId)
	if req.RequestId != "" {
		ackReq := protocol.Request{
			Cmd:       protocol.CmdAck,
			RequestId: req.RequestId,
		}
		ackPayload, _ := protocol.EncodeRequest(ackReq)
		if err := ctx.Framer.Write(ackPayload); err != nil {
			s.log().Error("failed to ACK quorum write",
				"request_id", req.RequestId,
				"error", err)
		}
	}

	// Update replica state
	s.lastSeq.Store(req.Seq)
	if err := s.storeState(); err != nil {
		s.log().Error("failed to store replica state", "error", err)
	}

	return nil
}

// processPrimaryPut handles a write on the primary node.
// Applies locally, forwards to replicas, optionally waits for quorum ACKs.
func (s *Server) processPrimaryPut(ctx *RequestContext) error {
	req := &ctx.Request
	key := string(req.Key)

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
			needed:        s.computeReplicaAcksNeeded(),
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

func (s *Server) computeReplicaAcksNeeded() int {
	s.mu.Lock()
	totalNodes := len(s.replicas) + 1 // +1 for primary itself
	s.mu.Unlock()
	quorum := totalNodes/2 + 1
	return quorum - 1 // Primary doesn't ACK itself, only count replica ACKs
}

// ---------------------------------------------------------------------------
// Helper Functions
// ---------------------------------------------------------------------------

func (s *Server) responseStatusWithPrimaryAddress(ctx *RequestContext, status protocol.Status) error {
	return s.writeResponse(ctx.Framer, protocol.Response{
		Status: status,
		Value:  []byte(s.opts.ReplicaOf), // Primary address for client redirect
	})
}

func (s *Server) responseStatusError(ctx *RequestContext) error {
	return s.writeResponse(ctx.Framer, protocol.Response{Status: protocol.StatusError})
}
