package server

import (
	"kvgo/protocol"
	"kvgo/utils"
	"net"
	"sync"
	"sync/atomic"
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

	var resp protocol.Response
	if !ok {
		resp = protocol.Response{Status: protocol.StatusNotFound, Seq: s.lastSeq.Load()}
	} else {
		seq := s.seq.Load()
		if s.isReplica {
			seq = s.lastSeq.Load()
		}

		// Avoid aliasing engine memory.
		copyVal := append([]byte(nil), val...)

		resp = protocol.Response{Status: protocol.StatusOK, Value: copyVal, Seq: seq}
	}

	return s.writeResponse(ctx.Framer, resp)
}

func (s *Server) doQuorumGet(ctx *RequestContext) error {
	req := ctx.Request

	key := string(req.Key)
	localVal, localOk := s.db.Get(key)
	localSeq := s.seq.Load()
	if s.isReplica {
		localSeq = s.lastSeq.Load()
	}

	var wg sync.WaitGroup
	var ackedCnt atomic.Int32
	var maxSeq atomic.Uint64
	var mu sync.Mutex
	var val []byte

	maxSeq.Store(localSeq)
	if localOk {
		val = append([]byte(nil), localVal...)
		ackedCnt.Store(1) // local counts as first ACK
	}

	req.RequireQuorum = false
	payload, err := protocol.EncodeRequest(req)
	if err != nil {
		s.log().Error("quorum read: failed to encode request", "error", err)
		return s.writeResponse(ctx.Framer, protocol.Response{Status: protocol.StatusError})
	}

	// Get snapshot of reachable nodes for safe iteration
	nodes := s.getReachableNodesSnapshot()
	n := len(nodes)
	quorum := utils.ComputeQuorum(n + 1) // +1 for local node

	for c, f := range nodes {
		wg.Go(func() {
			value, seq, ok := s.quorumReadFromReplica(f, payload, c.RemoteAddr())
			if !ok {
				return
			}

			ackedCnt.Add(1)

			// Atomically update both seq and val together
			for {
				currentMax := maxSeq.Load()
				if seq <= currentMax {
					break // This response is older
				}
				if maxSeq.CompareAndSwap(currentMax, seq) {
					mu.Lock()
					val = value
					mu.Unlock()
					break
				}
				// CAS failed, retry
			}
		})
	}

	wg.Wait()

	var resp protocol.Response
	if ackedCnt.Load() >= int32(quorum) {
		if len(val) == 0 && !localOk {
			resp = protocol.Response{Status: protocol.StatusNotFound, Seq: maxSeq.Load()}
		} else {
			resp = protocol.Response{Status: protocol.StatusOK, Value: val, Seq: maxSeq.Load()}
		}
	} else {
		s.log().Error("quorum read failed: insufficient responses",
			"responses", ackedCnt.Load(),
			"quorum_needed", quorum,
			"total_nodes", n+1)
		resp = protocol.Response{Status: protocol.StatusQuorumFailed}
	}

	return s.writeResponse(ctx.Framer, resp)
}

func (s *Server) quorumReadFromReplica(f *protocol.Framer, payload []byte, remoteAddr net.Addr) (value []byte, seq uint64, ok bool) {
	deadline := time.Now().Add(s.opts.QuorumReadTimeout)
	if err := f.WriteWithDeadline(payload, deadline); err != nil {
		s.log().Warn("quorum read: failed to send GET to replica", "remote_addr", remoteAddr, "error", err)
		return nil, 0, false
	}

	resp, err := f.ReadWithDeadline(deadline)
	if err != nil {
		s.log().Warn("quorum read: failed to get response", "remote_addr", remoteAddr, "error", err)
		return nil, 0, false
	}

	r, err := protocol.DecodeResponse(resp)
	if err != nil {
		s.log().Warn("quorum read: decode error", "remote_addr", remoteAddr, "error", err)
		return nil, 0, false
	}

	if r.Status != protocol.StatusOK && r.Status != protocol.StatusNotFound {
		s.log().Warn("quorum read: non-OK response from replica",
			"remote_addr", remoteAddr,
			"status", r.Status)
		return nil, 0, false
	}

	return r.Value, r.Seq, true
}

// ---------------------------------------------------------------------------
// PUT Request Handler
// ---------------------------------------------------------------------------

func (s *Server) handlePut(ctx *RequestContext) error {
	req := &ctx.Request
	key := string(req.Key)

	s.log().Debug("handlePut called", "key", key, "is_replica", s.isReplica, "from_conn", ctx.Conn.RemoteAddr())

	if s.isReplica {
		// Check if this PUT is from our primary (replication stream)
		s.mu.Lock()
		isPrimaryConn := (s.primary != nil && ctx.Conn == s.primary)
		primaryAddr := ""
		if s.primary != nil {
			primaryAddr = s.primary.RemoteAddr().String()
		}
		s.mu.Unlock()

		s.log().Debug("replica PUT check", "is_primary_conn", isPrimaryConn, "primary_addr", primaryAddr, "from_addr", ctx.Conn.RemoteAddr())

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
	// Also update primarySeq to track the primary's latest seq (helps reduce staleness window)
	if req.Seq > s.primarySeq {
		s.primarySeq = req.Seq
	}
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
	var state *quorumWriteState
	if req.RequireQuorum {
		req.RequestId = utils.GenerateUniqueID() // Only generate ID for quorum writes
		state = &quorumWriteState{
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
	quorum := utils.ComputeQuorum(totalNodes)
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
