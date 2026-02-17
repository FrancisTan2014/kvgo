package server

import (
	"kvgo/protocol"
	"kvgo/transport"
	"kvgo/utils"
	"sync"
	"sync/atomic"
	"time"
)

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
	remoteAddr := ctx.StreamTransport.RemoteAddr()
	if _, alreadyAcked := state.ackedReplicas[remoteAddr]; alreadyAcked {
		s.log().Debug("duplicate ACK from same replica ignored",
			"request_id", ctx.Request.RequestId,
			"replica", remoteAddr)
		return nil
	}

	// Mark this replica as having ACK'd
	state.ackedReplicas[remoteAddr] = struct{}{}
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
	remoteAddr := ctx.StreamTransport.RemoteAddr()
	if _, alreadyProcessed := state.ackedReplicas[remoteAddr]; alreadyProcessed {
		s.log().Debug("duplicate NACK from same replica ignored",
			"request_id", ctx.Request.RequestId,
			"replica", remoteAddr)
		return nil
	}

	// Mark this replica as having responded
	state.ackedReplicas[remoteAddr] = struct{}{}

	s.log().Warn("replica NACK'd write",
		"request_id", ctx.Request.RequestId,
		"replica", remoteAddr)

	// NACK means this replica failed - can't reach quorum
	// Close channel to fail fast instead of waiting for timeout
	state.closeOnce.Do(func() {
		state.failed.Store(true)
		close(state.ackCh)
	})

	return nil
}

// ---------------------------------------------------------------------------
// Quorum Read/Write Coordination
// ---------------------------------------------------------------------------

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
			ackedReplicas: make(map[string]struct{}),
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
			return s.writeResponse(ctx.StreamTransport, protocol.Response{Status: protocol.StatusOK, Seq: seq})
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
		return s.writeResponse(ctx.StreamTransport, protocol.Response{Status: protocol.StatusOK, Seq: seq})
	}
}

func (s *Server) computeReplicaAcksNeeded() int {
	s.connectionMu.Lock()
	totalNodes := len(s.replicas) + 1 // +1 for primary itself
	s.connectionMu.Unlock()
	quorum := utils.ComputeQuorum(totalNodes)
	return quorum - 1 // Primary doesn't ACK itself, only count replica ACKs
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
		return s.writeResponse(ctx.StreamTransport, protocol.Response{Status: protocol.StatusError})
	}

	// Get snapshot of reachable nodes for safe iteration
	nodes := s.peerManager.Addrs()
	n := len(nodes)
	quorum := utils.ComputeQuorum(n + 1) // +1 for local node

	for _, addr := range nodes {
		wg.Go(func() {
			t, err := s.peerManager.Get(addr)
			if err != nil {
				s.log().Warn("quorum read: peer unavailable", "addr", addr, "error", err)
				return
			}

			value, seq, ok := s.quorumReadFromReplica(t, payload, addr)
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

	return s.writeResponse(ctx.StreamTransport, resp)
}

func (s *Server) quorumReadFromReplica(t transport.RequestTransport, payload []byte, remoteAddr string) (value []byte, seq uint64, ok bool) {
	resp, err := t.Request(payload, s.opts.QuorumReadTimeout)
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
