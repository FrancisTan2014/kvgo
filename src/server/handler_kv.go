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
	if ctx.Request.RequireQuorum && !s.isLeader() {
		return s.doQuorumGet(ctx)
	}

	return s.doGet(ctx)
}

func (s *Server) isStaleness() bool {
	if s.isLeader() {
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
	if !s.isLeader() {
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

	if !s.isLeader() {
		// Check if this PUT is from our primary (replication stream)
		s.connectionMu.Lock()
		isPrimaryConn := (s.primary != nil && ctx.StreamTransport == s.primary)
		s.connectionMu.Unlock()

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
// Sends ACK/NACK through the peer channel (separate connection to primary).
func (s *Server) applyReplicatedPut(ctx *RequestContext) error {
	req := &ctx.Request
	key := string(req.Key)

	// Apply write locally
	if err := s.db.Put(key, req.Value); err != nil {
		s.log().Error("replication PUT failed", "key", key, "seq", req.Seq, "error", err)

		// Send NACK for quorum writes that failed
		if req.RequestId != "" {
			s.sendAckViaPeer(protocol.CmdNack, req.RequestId)
		}
		return err
	}

	// Send ACK only for quorum writes (those with RequestId)
	if req.RequestId != "" {
		s.sendAckViaPeer(protocol.CmdAck, req.RequestId)
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

// sendAckViaPeer sends an ACK or NACK to the primary through the peer channel.
// Fire-and-forget semantics: if the peer channel is unavailable the quorum
// write simply times out on the primary (correct, not catastrophic).
func (s *Server) sendAckViaPeer(cmd protocol.Cmd, requestId string) {
	nodeID := s.primaryNodeID
	if nodeID == "" {
		s.log().Warn("cannot send ACK/NACK: primary nodeID unknown (TOPOLOGY not received yet)")
		return
	}

	t, err := s.peerManager.Get(nodeID)
	if err != nil {
		s.log().Warn("cannot send ACK/NACK: peer transport unavailable",
			"primary", nodeID, "error", err)
		return
	}

	req := protocol.Request{Cmd: cmd, RequestId: requestId}
	payload, _ := protocol.EncodeRequest(req)

	if _, err := t.Request(payload, 2*time.Second); err != nil {
		s.log().Warn("ACK/NACK send failed",
			"cmd", cmd, "request_id", requestId, "error", err)
	}
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
