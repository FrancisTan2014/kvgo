package server

import (
	"fmt"
	"kvgo/protocol"
	"kvgo/transport"
	"kvgo/utils"
	"strings"
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

	strs := strings.Split(string(req.Value), protocol.Delimiter)
	if len(strs) != 3 || strs[0] == "" || strs[1] == "" || strs[2] == "" {
		s.log().Warn("REPLICATE rejected: invalid value format, expected replid, listenAddr, and nodeID")
		return s.responseStatusError(ctx)
	}

	replid := strs[0]
	listenAddr := strs[1]
	nodeID := strs[2]

	rc := newReplicaConn(ctx.StreamTransport, req.Seq, replid, nodeID, listenAddr)

	// Perform initial sync (blocks), then spawn writer goroutine.
	// Connection ownership transfers to serveReplica — caller must not close it.
	ctx.takenOver = s.serveReplica(rc)

	return nil
}

func (s *Server) buildReplicateRequest() protocol.Request {
	val := s.replid + protocol.Delimiter + s.listenAddr() + protocol.Delimiter + s.nodeID
	return protocol.Request{
		Cmd:   protocol.CmdReplicate,
		Seq:   s.lastSeq.Load(),
		Value: []byte(val),
	}
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
	s.log().Info("switching primary", "address", primaryAddr)

	if s.currentRole() != RoleFollower {
		if !s.becomeFollower() {
			return fmt.Errorf("relocate: invalid transition from %s to follower", s.currentRole())
		}
	}

	// Update config immediately
	s.connectionMu.Lock()
	s.opts.ReplicaOf = primaryAddr
	s.connectionMu.Unlock()

	// Do cleanup async to avoid blocking client response
	go func() {
		s.connectionMu.Lock()
		// Cancel old replication loop
		if s.replCancel != nil {
			s.replCancel()
		}

		for c, r := range s.replicas {
			close(r.sendCh)
			_ = r.transport.Close()
			delete(s.replicas, c)
		}

		// Clear peers when switching primary (topology will be re-sent by new primary)
		s.peerManager.SavePeers(nil)

		if s.primary != nil {
			_ = s.primary.Close()
			s.primary = nil
		}

		if s.backlogCancel != nil {
			s.backlogCancel()
		}
		s.connectionMu.Unlock()

		s.startReplicationLoop()
	}()

	return nil
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
			if _, err := t.Request(payload, 2*time.Second); err != nil {
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
	if len(ctx.Request.Value) > 0 {
		selfID := s.nodeID
		replicaOf := s.opts.ReplicaOf
		for _, entry := range strings.Split(string(ctx.Request.Value), protocol.Delimiter) {
			nodeID, addr, ok := strings.Cut(entry, "@")
			if !ok || nodeID == "" || addr == "" {
				continue
			}
			// Identify the primary by matching the listen address we replicate from.
			if addr == replicaOf {
				s.primaryNodeID = nodeID
			}
			if nodeID != selfID {
				peers = append(peers, PeerInfo{NodeID: nodeID, Addr: addr})
			}
		}
	}

	s.peerManager.SavePeers(peers)
	return nil // TOPOLOGY is a one-way notification; no response expected by primary's reader.
}

func (s *Server) buildTopologyRequest(replicas map[transport.StreamTransport]*replicaConn) protocol.Request {
	var sb strings.Builder

	// Include the primary itself so replicas can reach it (e.g. for elections)
	sb.WriteString(s.nodeID)
	sb.WriteString("@")
	sb.WriteString(s.listenAddr())
	sb.WriteString(protocol.Delimiter)

	for _, rc := range replicas {
		sb.WriteString(rc.nodeID)
		sb.WriteString("@")
		sb.WriteString(rc.listenAddr)
		sb.WriteString(protocol.Delimiter)
	}

	topology := strings.TrimRight(sb.String(), protocol.Delimiter)
	return protocol.Request{
		Cmd:   protocol.CmdTopology,
		Value: []byte(topology),
	}
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
	s.peerManager.SavePeers(peers)

	for _, rc := range snapshot {
		rc.sendCh <- payload
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

		pr, err := t.Request(payload, timeout)
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
	vr, err := parseVoteRequest(ctx.Request.Value)
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
			if addr, ok := s.peerManager.Addr(vr.nodeID); ok {
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
func (s *Server) evaluateVoteLocked(vr voteRequest) (protocol.Response, bool) {
	myTerm := s.term.Load()
	stepped := false

	// Stale term — reject without updating anything.
	if vr.term < myTerm {
		s.log().Debug("VOTE rejected: stale term", "candidate", vr.nodeID, "candidateTerm", vr.term, "myTerm", myTerm)
		return s.buildVoteResponse(false), false
	}

	// Higher term — step down and update our term before evaluating the vote.
	if vr.term > myTerm {
		s.term.Store(vr.term)
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
	if s.votedFor != "" && s.votedFor != vr.nodeID {
		s.log().Debug("VOTE rejected: already voted", "candidate", vr.nodeID, "votedFor", s.votedFor)
		return s.buildVoteResponse(false), stepped
	}

	// Log completeness check — candidate must be at least as up-to-date.
	if vr.lastSeq < s.lastSeq.Load() {
		s.log().Debug("VOTE rejected: candidate log behind", "candidate", vr.nodeID, "candidateLastSeq", vr.lastSeq, "myLastSeq", s.lastSeq.Load())
		return s.buildVoteResponse(false), stepped
	}

	// Grant the vote.
	s.votedFor = vr.nodeID
	s.lastHeartbeat = time.Now() // reset election timer so we don't immediately campaign
	if err := s.storeState(); err != nil {
		s.log().Error("failed to persist votedFor", "error", err)
	}

	s.log().Info("VOTE granted", "candidate", vr.nodeID, "term", vr.term)
	return s.buildVoteResponse(true), stepped
}
