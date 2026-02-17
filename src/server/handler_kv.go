package server

import (
	"kvgo/protocol"
	"time"
)

// ---------------------------------------------------------------------------
// GET Request Handler
// ---------------------------------------------------------------------------

func (s *Server) handleGet(ctx *RequestContext) error {
	// Replicas check staleness before serving ANY reads (eventual or quorum)
	if s.isStaleness() {
		return s.responseStatusWithPrimaryAddress(ctx, protocol.StatusReplicaTooStale)
	}

	// Quorum reads only make sense for replicas in single-primary systems.
	// Primary is the authoritative source, always reads locally.
	if ctx.Request.RequireQuorum && s.isReplica {
		return s.doQuorumGet(ctx)
	}

	return s.doGet(ctx)
}

func (s *Server) isStaleness() bool {
	if !s.isReplica {
		return false
	}

	seqLag := s.primarySeq - s.lastSeq.Load()
	heartbeatAge := time.Since(s.lastHeartbeat)
	return seqLag > uint64(s.opts.ReplicaStaleLag) || heartbeatAge > s.opts.ReplicaStaleHeartbeat
}

func (s *Server) doGet(ctx *RequestContext) error {
	req := ctx.Request

	key := string(req.Key)
	val, ok := s.db.Get(key)

	// Determine which sequence number to report
	seq := s.seq.Load()
	if s.isReplica {
		seq = s.lastSeq.Load()
	}

	var resp protocol.Response
	if !ok {
		resp = protocol.Response{Status: protocol.StatusNotFound, Seq: seq}
	} else {
		// Avoid aliasing engine memory.
		copyVal := append([]byte(nil), val...)
		resp = protocol.Response{Status: protocol.StatusOK, Value: copyVal, Seq: seq}
	}

	return s.writeResponse(ctx.StreamTransport, resp)
}

// ---------------------------------------------------------------------------
// PUT Request Handler
// ---------------------------------------------------------------------------

func (s *Server) handlePut(ctx *RequestContext) error {
	req := &ctx.Request
	key := string(req.Key)

	s.log().Debug("handlePut called", "key", key, "is_replica", s.isReplica, "from_conn", ctx.StreamTransport.RemoteAddr())

	if s.isReplica {
		// Check if this PUT is from our primary (replication stream)
		s.connectionMu.Lock()
		isPrimaryConn := (s.primary != nil && ctx.StreamTransport == s.primary)
		primaryAddr := ""
		if s.primary != nil {
			primaryAddr = s.primary.RemoteAddr()
		}
		s.connectionMu.Unlock()

		s.log().Debug("replica PUT check", "is_primary_conn", isPrimaryConn, "primary_addr", primaryAddr, "from_addr", ctx.StreamTransport.RemoteAddr())

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
			if err := ctx.StreamTransport.Send(nackPayload); err != nil {
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
		if err := ctx.StreamTransport.Send(ackPayload); err != nil {
			s.log().Error("failed to ACK quorum write",
				"request_id", req.RequestId,
				"error", err)
		}
	}

	// Update replica state
	s.lastSeq.Store(req.Seq)
	// Also update primarySeq to track the primary's latest seq (helps reduce staleness window)
	if req.Seq > s.primarySeq {
		s.primarySeq = req.Seq
	}
	if err := s.storeState(); err != nil {
		s.log().Error("failed to store replica state", "error", err)
	}

	return nil
}

// ---------------------------------------------------------------------------
// Helper Functions
// ---------------------------------------------------------------------------

func (s *Server) responseStatusWithPrimaryAddress(ctx *RequestContext, status protocol.Status) error {
	return s.writeResponse(ctx.StreamTransport, protocol.Response{
		Status: status,
		Value:  []byte(s.opts.ReplicaOf), // Primary address for client redirect
	})
}

func (s *Server) responseStatusError(ctx *RequestContext) error {
	return s.writeResponse(ctx.StreamTransport, protocol.Response{Status: protocol.StatusError})
}

func (s *Server) responseStatusOk(ctx *RequestContext) error {
	return s.writeResponse(ctx.StreamTransport, protocol.Response{Status: protocol.StatusOK})
}
