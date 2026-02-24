package server

import (
	"context"
	"fmt"
	"kvgo/protocol"
	"kvgo/transport"
	"kvgo/utils"
	"sync"
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
	rc.hb = time.NewTicker(heartbeatInterval) // old ticker stopped by exiting writer's defer
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

	// Serialize all relocate calls. Without this, concurrent callers
	// (e.g. VOTE step-down + broadcastPromotion) each spawn a
	// teardown+restart cycle, the second teardown closes the DB
	// while the first resync is writing.
	s.relocateMu.Lock()
	defer s.relocateMu.Unlock()

	// If we're already following this primary, skip teardown+restart.
	s.connMu.RLock()
	alreadyFollowing := s.isFollower() && s.opts.ReplicaOf == primaryAddr
	s.connMu.RUnlock()
	if alreadyFollowing {
		s.log().Debug("relocate: already following target, skipping", "address", primaryAddr)
		return nil
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

	// Tear down old replication state (cancels context, closes primary
	// transport to unblock receiveFromPrimary). Then set the new address
	// and start a fresh loop. startReplicationLoop waits for the old
	// loop goroutine to fully exit via replDone before launching the
	// new one, preventing concurrent loops.
	s.teardownReplicationState()

	s.connMu.Lock()
	s.opts.ReplicaOf = primaryAddr
	s.connMu.Unlock()

	s.startReplicationLoop()

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
			t, err := s.peerManager.GetTransport(pid)
			if err != nil {
				s.log().Debug("broadcastPromotion: peer unreachable", "peer", pid, "error", err)
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), promotionBroadcastTimeout)
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

func (s *Server) handlePreVoteRequest(ctx *RequestContext) error {
	vr, err := protocol.ParseVoteRequestValue(ctx.Request.Value)
	if err != nil {
		s.log().Warn("PREVOTE rejected: malformed request", "error", err)
		return s.writeResponse(ctx.StreamTransport, s.buildPreVoteResponse(false))
	}

	s.roleMu.Lock()
	resp := s.evaluatePreVoteLocked(vr)
	s.roleMu.Unlock()

	return s.writeResponse(ctx.StreamTransport, resp)
}

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
			if pi, ok := s.peerManager.Get(vr.NodeID); ok {
				go func() {
					if err := s.relocate(pi.Addr); err != nil {
						s.log().Warn("post-stepdown relocate failed", "addr", pi.Addr, "error", err)
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
		stepped = s.setRoleFollower()
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

// evaluatePreVoteLocked decides whether to grant a pre-vote.
// PreVote is side-effect-free: no term bump, no votedFor, no persistence.
// Adds a lease check: reject if we've heard from a leader recently
// (prevents a partitioned node from disrupting a healthy cluster).
// Caller must hold s.roleMu.
func (s *Server) evaluatePreVoteLocked(vr protocol.VoteRequestValue) protocol.Response {
	myTerm := s.term.Load()

	// Stale term — reject.
	if vr.Term < myTerm {
		s.log().Debug("PREVOTE rejected: stale term", "candidate", vr.NodeID, "candidateTerm", vr.Term, "myTerm", myTerm)
		return s.buildPreVoteResponse(false)
	}

	// Log completeness check — candidate must be at least as up-to-date.
	if vr.LastSeq < s.lastSeq.Load() {
		s.log().Debug("PREVOTE rejected: candidate log behind", "candidate", vr.NodeID, "candidateLastSeq", vr.LastSeq, "myLastSeq", s.lastSeq.Load())
		return s.buildPreVoteResponse(false)
	}

	// Lease check: if we've heard from the leader within the election timeout,
	// the cluster is healthy — reject to prevent disruption.
	if time.Since(s.lastHeartbeat) < electionTimeout {
		s.log().Debug("PREVOTE rejected: leader lease active", "candidate", vr.NodeID, "lastHeartbeat", time.Since(s.lastHeartbeat))
		return s.buildPreVoteResponse(false)
	}

	s.log().Info("PREVOTE granted", "candidate", vr.NodeID, "term", vr.Term)
	return s.buildPreVoteResponse(true)
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
			t, err := s.peerManager.GetTransport(pid)
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

// ---------------------------------------------------------------------------
// Auto Discovery On Restart
// ---------------------------------------------------------------------------

// discoverCluster probes all known peers for the current leader on restart.
func (s *Server) discoverCluster() (PeerInfo, uint64, bool) {
	peerIDs := s.peerManager.NodeIDs()
	myTerm := s.term.Load()
	s.log().Info("discovering cluster", "peers", len(peerIDs), "term", myTerm)

	req := protocol.NewDiscoveryRequest(myTerm, s.nodeID)
	payload, _ := protocol.EncodeRequest(req)

	ctx, cancel := context.WithTimeout(context.Background(), s.opts.DiscoveryTimeout)
	defer cancel()

	var wg sync.WaitGroup
	var mu sync.Mutex
	var leaderId, leaderAddr string
	highestTerm := myTerm

	for _, pid := range peerIDs {
		wg.Go(func() {
			rv, ok := s.probePeer(ctx, pid, payload)
			if !ok {
				return
			}

			mu.Lock()
			defer mu.Unlock()

			if rv.Term >= highestTerm && rv.LeaderId != "" && rv.LeaderAddr != "" {
				highestTerm = rv.Term
				leaderId = rv.LeaderId
				leaderAddr = rv.LeaderAddr
			}
		})
	}

	wg.Wait()

	if leaderId != "" {
		s.log().Info("discovered leader", "leader", leaderId, "addr", leaderAddr, "term", highestTerm)
		return PeerInfo{NodeID: leaderId, Addr: leaderAddr}, highestTerm, true
	} else {
		s.log().Info("no leader discovered, waiting for election")
		return PeerInfo{}, highestTerm, false
	}
}

// probePeer sends a discovery request to a single peer and returns the parsed response.
func (s *Server) probePeer(ctx context.Context, peerID string, payload []byte) (protocol.DiscoveryResponseValue, bool) {
	p, err := s.peerManager.GetTransport(peerID)
	if err != nil {
		s.log().Debug("discovery: peer unreachable", "peer", peerID, "error", err)
		return protocol.DiscoveryResponseValue{}, false
	}

	respPayload, err := p.Request(ctx, payload)
	if err != nil {
		s.log().Debug("discovery: request failed", "peer", peerID, "error", err)
		return protocol.DiscoveryResponseValue{}, false
	}

	resp, err := protocol.DecodeResponse(respPayload)
	if err != nil {
		s.log().Debug("discovery: invalid response", "peer", peerID, "error", err)
		return protocol.DiscoveryResponseValue{}, false
	}

	if resp.Status != protocol.StatusDiscoveryResponse {
		s.log().Debug("discovery: unexpected status", "peer", peerID, "status", resp.Status)
		return protocol.DiscoveryResponseValue{}, false
	}

	rv, err := protocol.ParseDiscoveryResponseValue(resp.Value)
	if err != nil {
		s.log().Debug("discovery: invalid response value", "peer", peerID, "error", err)
		return protocol.DiscoveryResponseValue{}, false
	}

	return rv, true
}

// handleDiscovery replies with the current leader's identity and address.
func (s *Server) handleDiscovery(ctx *RequestContext) error {
	rv, err := protocol.ParseDiscoveryRequestValue(ctx.Request.Value)
	if err != nil {
		s.log().Debug("discovery: invalid request", "error", err)
		return err
	}

	s.log().Info("discovery: request received", "nodeId", rv.NodeID, "term", rv.Term)

	var resp protocol.Response
	if s.isLeader() {
		resp = protocol.NewDiscoveryResponse(s.term.Load(), s.nodeID, s.listenAddr())
	} else if s.primaryNodeID != "" {
		if pi, ok := s.peerManager.Get(s.primaryNodeID); ok {
			resp = protocol.NewDiscoveryResponse(s.term.Load(), pi.NodeID, pi.Addr)
		} else {
			s.log().Debug("discovery: leader known but address missing", "leader", s.primaryNodeID)
		}
	}

	return s.writeResponse(ctx.StreamTransport, resp)
}

// ---------------------------------------------------------------------------
// Transfer Leader To A Target Follower
// ---------------------------------------------------------------------------

func (s *Server) handleTransferLeader(ctx *RequestContext) error {
	target := string(ctx.Request.Value)
	if target == "" {
		s.log().Warn("TRANSFER reject: empty target received")
		return s.responseStatusError(ctx)
	}

	replica, exists := s.getReplicaSnapshot()[target]
	if !exists || !replica.connected.Load() {
		s.log().Warn("TRANSFER reject: target replica not connected", "target", target)
		return s.responseStatusError(ctx)
	}

	pt, err := s.peerManager.GetTransport(target)
	if err != nil {
		s.log().Warn("TRANSFER reject: cannot reach target peer", "target", target, "error", err)
		return s.responseStatusError(ctx)
	}

	req := protocol.NewTimeoutNowRequest(s.seq.Load())
	reqPayload, _ := protocol.EncodeRequest(req)
	c, cancel := context.WithTimeout(context.Background(), transferCatchUpTimeout)
	defer cancel()

	s.transferring.Store(true) // reject all writes
	respPayload, err := pt.Request(c, reqPayload)
	if err != nil {
		s.transferring.Store(false)
		s.log().Warn("TRANSFER failed: request error", "target", target, "error", err)
		return s.responseStatusError(ctx)
	}

	resp, err := protocol.DecodeResponse(respPayload)
	if err != nil {
		s.transferring.Store(false)
		s.log().Warn("TRANSFER failed: invalid response", "target", target, "error", err)
		return s.responseStatusError(ctx)
	}

	if resp.Status != protocol.StatusOK {
		s.transferring.Store(false)
		s.log().Warn("TRANSFER failed: non-ok status", "status", resp.Status, "target", target)
		return s.responseStatusError(ctx)
	}

	go func() {
		time.Sleep(electionTimeout)
		if s.transferring.CompareAndSwap(true, false) {
			s.log().Warn("TRANSFER timed out, resuming writes", "target", target)
		}
	}()

	s.log().Info("TRANSFER initiated", "target", target, "seq", s.seq.Load())
	return s.responseStatusOk(ctx)
}

func (s *Server) handleTimeoutNow(ctx *RequestContext) error {
	s.fenced.Store(false)

	targetSeq := ctx.Request.Seq
	s.log().Info("TIMEOUT_NOW received", "targetSeq", targetSeq, "lastSeq", s.lastSeq.Load())

	// fast path: already caught up — campaign immediately
	if s.lastSeq.Load() >= targetSeq {
		s.log().Info("TIMEOUT_NOW: already caught up, campaigning")
		go func() {
			if !s.becomeCandidate() {
				s.log().Warn("TIMEOUT_NOW: becomeCandidate failed")
			}
		}()
		return s.responseStatusOk(ctx)
	}

	s.transferMu.Lock()
	// create channel before atomic store — PUT goroutines that observe
	// pendingTransferSeq > 0 are guaranteed to see the channel
	// (atomic store provides happens-before)
	s.seqReachedCh = make(chan struct{}, 1)
	s.transferMu.Unlock()
	defer func() {
		s.transferMu.Lock()
		defer s.transferMu.Unlock()
		s.seqReachedCh = nil
		s.pendingTransferSeq.Store(0)
	}()

	s.pendingTransferSeq.Store(int64(targetSeq))
	c, cancel := context.WithTimeout(context.Background(), transferCatchUpTimeout)
	defer cancel()

	// set-then-check: if the last PUT landed between the first check and the Store above,
	// lastSeq already reached the target — catch it here before blocking on the channel
	if s.lastSeq.Load() >= targetSeq {
		s.log().Info("TIMEOUT_NOW: caught up after store, campaigning")
		go func() {
			if !s.becomeCandidate() {
				s.log().Warn("TIMEOUT_NOW: becomeCandidate failed")
			}
		}()
		return s.responseStatusOk(ctx)
	}

	s.log().Info("TIMEOUT_NOW: waiting for catch-up", "lastSeq", s.lastSeq.Load(), "targetSeq", targetSeq)

	select {
	case <-s.seqReachedCh:
		s.log().Info("TIMEOUT_NOW: caught up via replication, campaigning")
		go func() {
			if !s.becomeCandidate() {
				s.log().Warn("TIMEOUT_NOW: becomeCandidate failed")
			}
		}()
		return s.responseStatusOk(ctx)
	case <-c.Done():
		s.log().Warn("TIMEOUT_NOW: catch-up timed out", "targetSeq", targetSeq, "lastSeq", s.lastSeq.Load())
		return s.responseStatusError(ctx)
	}
}
