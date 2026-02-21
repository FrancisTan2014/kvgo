package server

import (
	"context"
	"fmt"
	"kvgo/protocol"
	"kvgo/transport"
	"kvgo/utils"
	"time"
)

// ---------------------------------------------------------------------------
// REPLICATE Command Handler (Primary receives from replica)
// ---------------------------------------------------------------------------

// handleReplicate handles the REPLICATE command from a replica wanting to sync.
// After initial sync, connection returns to normal request loop for ACK/NACK/PONG.
func (s *Server) handleReplicate(ctx *RequestContext) error {
	if !s.isLeader() {
		s.log().Warn("REPLICATE rejected: node is not leader")
		return s.responseStatusWithPrimaryAddress(ctx, protocol.StatusReadOnly)
	}

	req := &ctx.Request

	rv, err := protocol.ParseReplicateValue(req.Value)
	if err != nil {
		s.log().Warn("REPLICATE rejected: invalid value format", "error", err)
		return s.responseStatusError(ctx)
	}

	s.connMu.RLock()
	existing, exists := s.replicas[rv.NodeID]
	s.connMu.RUnlock()

	var rc *replicaConn
	if exists {
		rc = existing
		s.updateReplicaConn(rc, ctx, &rv)
	} else {
		rc = newReplicaConn(ctx.StreamTransport, req.Seq, rv.Replid, rv.NodeID, rv.ListenAddr)
	}

	// Perform initial sync (blocks), then spawn writer goroutine.
	// Connection ownership transfers to serveReplica — caller must not close it.
	ctx.takenOver = s.serveReplica(rc)

	return nil
}

func (s *Server) updateReplicaConn(rc *replicaConn, ctx *RequestContext, rv *protocol.ReplicateValue) {
	rc.connected.Store(false) // gate off senders before closing old channel
	close(rc.sendCh)
	s.connMu.Lock()
	// Drain stale writes — fresh SYNC/PSYNC establishes the new baseline
	rc.sendCh = make(chan []byte, replicaSendBuffer)
	rc.transport = ctx.StreamTransport
	rc.listenAddr = rv.ListenAddr
	rc.lastSeq = ctx.Request.Seq
	rc.lastReplid = rv.Replid
	s.connMu.Unlock()
}

func (s *Server) buildReplicateRequest() protocol.Request {
	return protocol.NewReplicateRequest(s.replid, s.listenAddr(), s.nodeID, s.lastSeq.Load())
}

// ---------------------------------------------------------------------------
// REPLICAOF Command Handler (Client initiates replication change)
// ---------------------------------------------------------------------------

// handleReplicaOf handles the REPLICAOF command to dynamically change replication target.
func (s *Server) handleReplicaOf(ctx *RequestContext) error {
	req := &ctx.Request

	if err := s.relocate(string(req.Value)); err != nil {
		s.log().Error("REPLICAOF failed", "error", err)
		return s.responseStatusError(ctx)
	}
	return s.responseStatusOk(ctx)
}

func (s *Server) relocate(primaryAddr string) error {
	if primaryAddr == s.listenAddr() {
		return fmt.Errorf("relocate: refusing to replicate from self (%s)", primaryAddr)
	}
	s.log().Info("switching primary", "address", primaryAddr)

	if !s.isFollower() {
		if !s.becomeFollower() {
			return fmt.Errorf("relocate: invalid transition from %s to follower", s.currentRole())
		}
		// Initialize primarySeq to avoid false staleness after leader→follower transition.
		// Without this, primarySeq=0 minus lastSeq=N wraps to a huge uint64 and
		// isStaleness() rejects all reads until the first heartbeat arrives.
		s.primarySeq = s.lastSeq.Load()
	}

	// Update config immediately
	s.connMu.Lock()
	s.opts.ReplicaOf = primaryAddr
	s.connMu.Unlock()

	// Do cleanup async to avoid blocking client response
	go func() {
		s.teardownReplicationState()
		s.startReplicationLoop()
	}()

	return nil
}

// teardownReplicationState tears down all replication state: replica send channels,
// transports, replication loop, primary transport, and backlog trimmer.
// Used by both relocate (role change) and fenceLoop (quorum loss).
// Caller must NOT hold connMu.
func (s *Server) teardownReplicationState() {
	s.connMu.Lock()
	defer s.connMu.Unlock()

	if s.replCancel != nil {
		s.replCancel()
	}

	for id, rc := range s.replicas {
		rc.connected.Store(false) // gate off senders before closing channel
		close(rc.sendCh)
		_ = rc.transport.Close()
		delete(s.replicas, id)
	}

	if s.primary != nil {
		_ = s.primary.Close()
		s.primary = nil
	}

	if s.backlogCancel != nil {
		s.backlogCancel()
	}
}

// ---------------------------------------------------------------------------
// PROMOTE Command Handler (Replica promotes itself to primary)
// ---------------------------------------------------------------------------

func (s *Server) handlePromote(ctx *RequestContext) error {
	s.log().Warn("PROMOTE is deprecated; use election instead")
	return s.responseStatusError(ctx)
}

func (s *Server) promote() error {
	// Generate new replid — this node is starting a new timeline
	s.replid = utils.GenerateUniqueID()
	s.seq.Store(s.lastSeq.Load()) // continue from replica's position

	if s.replCancel != nil {
		s.replCancel() // signal loop to exit
	}
	if s.primary != nil {
		_ = s.primary.Close()
	}

	// Start backlog trimmer for new primary role
	s.startBacklogTrimmer()

	if err := s.storeState(); err != nil {
		s.log().Error("failed to store new replid after promotion", "error", err)
	}

	s.broadcastPromotion()

	return nil
}

// broadcastPromotion tells all peers to replicate from this node.
// Fire-and-forget: peers that miss this will self-heal via election timeout.
func (s *Server) broadcastPromotion() {
	req := protocol.Request{
		Cmd:   protocol.CmdReplicaOf,
		Value: []byte(s.listenAddr()),
	}
	payload, _ := protocol.EncodeRequest(req)

	peerIds := s.peerManager.NodeIDs()
	for _, pid := range peerIds {
		go func() {
			t, err := s.peerManager.Get(pid)
			if err != nil {
				s.log().Debug("broadcastPromotion: peer unreachable", "peer", pid, "error", err)
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if _, err := t.Request(ctx, payload); err != nil {
				s.log().Debug("broadcastPromotion: request failed", "peer", pid, "error", err)
			}
		}()
	}
}

// ---------------------------------------------------------------------------
// TOPOLOGY Command Handler (Primary broadcasts topology on replica joining/leaving)
// ---------------------------------------------------------------------------

func (s *Server) handleTopology(ctx *RequestContext) error {
	var peers []PeerInfo
	for _, entry := range protocol.ParseTopologyValue(ctx.Request.Value) {
		// Identify the primary by matching the listen address we replicate from.
		if entry.Addr == s.opts.ReplicaOf {
			s.primaryNodeID = entry.NodeID
		}
		if entry.NodeID != s.nodeID {
			peers = append(peers, PeerInfo{NodeID: entry.NodeID, Addr: entry.Addr})
		}
	}

	s.peerManager.MergePeers(peers)
	return nil // TOPOLOGY is a one-way notification; no response expected by primary's reader.
}

func (s *Server) buildTopologyRequest(replicas map[string]*replicaConn) protocol.Request {
	// Include the primary itself so replicas can reach it (e.g. for elections)
	entries := make([]protocol.TopologyEntry, 0, 1+len(replicas))
	entries = append(entries, protocol.TopologyEntry{NodeID: s.nodeID, Addr: s.listenAddr()})
	for _, rc := range replicas {
		entries = append(entries, protocol.TopologyEntry{NodeID: rc.nodeID, Addr: rc.listenAddr})
	}
	return protocol.NewTopologyRequest(entries)
}

func (s *Server) broadcastTopology() {
	if !s.isLeader() {
		return
	}

	snapshot := s.getReplicaSnapshot()
	req := s.buildTopologyRequest(snapshot)
	payload, _ := protocol.EncodeRequest(req)

	// Save peers on the primary itself so they survive a crash.
	// Replicas receive this via TOPOLOGY; the primary must self-save.
	peers := make([]PeerInfo, 0, len(snapshot))
	for _, rc := range snapshot {
		if rc.nodeID != "" && rc.listenAddr != "" {
			peers = append(peers, PeerInfo{NodeID: rc.nodeID, Addr: rc.listenAddr})
		}
	}
	s.peerManager.MergePeers(peers)

	for _, rc := range snapshot {
		if rc.connected.Load() {
			rc.sendCh <- payload
		}
	}
}

// ---------------------------------------------------------------------------
// PEER Command Handler (Peer establishes long-lived connection via handshake)
// ---------------------------------------------------------------------------

// handlePeerHandshake responds to a PEER handshake, then transfers
// connection ownership to servePeer. The caller's dispatch loop exits
// via takenOver, and the peer gets a long-lived, timeout-free channel.
func (s *Server) handlePeerHandshake(ctx *RequestContext) error {
	go s.servePeer(ctx.StreamTransport)
	ctx.takenOver = true
	return s.responseStatusOk(ctx)
}

// servePeer runs the request dispatch loop with no read timeout.
// It blocks until the peer disconnects or the transport is closed.
func (s *Server) servePeer(transport transport.StreamTransport) {
	s.handleRequest(transport, 0)
}

// DialPeer returns a DialFunc that connects to a peer and performs
// the PEER handshake. After the handshake, the remote side switches
// to a timeout-free dispatch loop, keeping the connection alive.
func DialPeer(proto, network string, timeout time.Duration) DialFunc {
	return func(addr string) (transport.RequestTransport, error) {
		t, err := transport.DialRequestTransport(proto, network, addr, timeout)
		if err != nil {
			return nil, err
		}

		req := protocol.Request{Cmd: protocol.CmdPeerHandshake}
		payload, _ := protocol.EncodeRequest(req)

		ctx := context.Background()
		if timeout > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, timeout)
			defer cancel()
		}
		pr, err := t.Request(ctx, payload)
		if err != nil {
			return nil, err
		}

		resp, err := protocol.DecodeResponse(pr)
		if err != nil {
			return nil, err
		}

		if resp.Status != protocol.StatusOK {
			return nil, fmt.Errorf("peer handshake: non-OK response, status=%d", resp.Status)
		}

		return t, nil
	}
}

// ---------------------------------------------------------------------------
// VOTE Command Handler (Candidate requests votes during leader election)
// ---------------------------------------------------------------------------

func (s *Server) handleVoteRequest(ctx *RequestContext) error {
	vr, err := protocol.ParseVoteRequestValue(ctx.Request.Value)
	if err != nil {
		s.log().Warn("VOTE rejected: malformed request", "error", err)
		return s.writeResponse(ctx.StreamTransport, s.buildVoteResponse(false))
	}

	wasLeader := s.isLeader()

	s.roleMu.Lock()
	resp, stepped := s.evaluateVoteLocked(vr)
	s.roleMu.Unlock()

	if stepped {
		s.notifyRoleChanged()

		// After stepping down from leader, reconnect to the cluster
		// via the candidate (or any known peer).
		if wasLeader {
			if addr, ok := s.peerManager.Addr(vr.NodeID); ok {
				go func() {
					if err := s.relocate(addr); err != nil {
						s.log().Warn("post-stepdown relocate failed", "addr", addr, "error", err)
					}
				}()
			}
		}
	}

	return s.writeResponse(ctx.StreamTransport, resp)
}

// evaluateVoteLocked decides whether to grant a vote.
// Caller must hold s.roleMu.
// Returns the response and whether a step-down occurred (caller must notify).
func (s *Server) evaluateVoteLocked(vr protocol.VoteRequestValue) (protocol.Response, bool) {
	myTerm := s.term.Load()
	stepped := false

	// Stale term — reject without updating anything.
	if vr.Term < myTerm {
		s.log().Debug("VOTE rejected: stale term", "candidate", vr.NodeID, "candidateTerm", vr.Term, "myTerm", myTerm)
		return s.buildVoteResponse(false), false
	}

	// Higher term — step down and update our term before evaluating the vote.
	if vr.Term > myTerm {
		s.term.Store(vr.Term)
		s.votedFor = ""
		cur := s.currentRole()
		if cur != RoleFollower {
			s.role.Store(uint32(RoleFollower))
			s.log().Info("became follower", "term", s.term.Load())
			stepped = true
		}
		if err := s.storeState(); err != nil {
			s.log().Error("failed to persist state on term bump", "error", err)
		}
	}

	// Already voted for someone else this term.
	if s.votedFor != "" && s.votedFor != vr.NodeID {
		s.log().Debug("VOTE rejected: already voted", "candidate", vr.NodeID, "votedFor", s.votedFor)
		return s.buildVoteResponse(false), stepped
	}

	// Log completeness check — candidate must be at least as up-to-date.
	if vr.LastSeq < s.lastSeq.Load() {
		s.log().Debug("VOTE rejected: candidate log behind", "candidate", vr.NodeID, "candidateLastSeq", vr.LastSeq, "myLastSeq", s.lastSeq.Load())
		return s.buildVoteResponse(false), stepped
	}

	// Grant the vote.
	s.votedFor = vr.NodeID
	s.lastHeartbeat = time.Now() // reset election timer so we don't immediately campaign
	if err := s.storeState(); err != nil {
		s.log().Error("failed to persist votedFor", "error", err)
	}

	s.log().Info("VOTE granted", "candidate", vr.NodeID, "term", vr.Term)
	return s.buildVoteResponse(true), stepped
}

// ---------------------------------------------------------------------------
// Periodic Reconciliation
// ---------------------------------------------------------------------------

// reconcileLoop runs while the server is alive. When leader, it periodically
// sends REPLICAOF to peers that haven't connected as replicas — ensuring
// orphaned nodes (e.g. a stale old primary) eventually rejoin the cluster.
func (s *Server) reconcileLoop() {
	for {
		if !s.isLeader() {
			ch := s.getRoleChangedCh()
			select {
			case <-ch:
				continue
			case <-s.ctx.Done():
				return
			}
		}
		select {
		case <-s.ctx.Done():
			return
		case <-time.After(reconcileInterval):
			s.reconcilePeers()
		}
	}
}

func (s *Server) reconcilePeers() {
	if !s.isLeader() {
		return
	}

	snapshot := s.getReplicaSnapshot()
	connected := make(map[string]bool, len(snapshot))
	connectedCount := 0
	for nodeID, rc := range snapshot {
		if rc.connected.Load() {
			connected[nodeID] = true
			connectedCount++
		}
	}

	// A leader that had replicas but lost them all is likely stale (fence pending).
	// Don't reconcile — we'd recruit peers back and prevent fence from triggering.
	if len(snapshot) > 0 && connectedCount == 0 {
		return
	}

	peerIDs := s.peerManager.NodeIDs()
	for _, pid := range peerIDs {
		if pid == s.nodeID {
			continue
		}
		if connected[pid] {
			continue
		}
		go func() {
			t, err := s.peerManager.Get(pid)
			if err != nil {
				s.log().Debug("reconcile: peer unreachable", "peer", pid, "error", err)
				return
			}
			req := protocol.Request{
				Cmd:   protocol.CmdReplicaOf,
				Value: []byte(s.listenAddr()),
			}
			payload, _ := protocol.EncodeRequest(req)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if _, err := t.Request(ctx, payload); err != nil {
				s.log().Debug("reconcile: REPLICAOF failed", "peer", pid, "error", err)
			} else {
				s.log().Info("reconcile: sent REPLICAOF", "peer", pid)
			}
		}()
	}
}
