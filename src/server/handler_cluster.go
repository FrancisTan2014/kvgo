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
	if s.isReplica {
		s.log().Warn("REPLICATE rejected: node is replica")
		return s.responseStatusError(ctx)
	}

	req := &ctx.Request

	strs := strings.Split(string(req.Value), protocol.Delimiter)
	if len(strs) != 2 || strs[0] == "" || strs[1] == "" {
		s.log().Warn("REPLICATE rejected: invalid value format, expected replid and listenAddr")
		return s.responseStatusError(ctx)
	}

	replid := strs[0]
	listenAddr := strs[1]

	rc := newReplicaConn(ctx.StreamTransport, req.Seq, replid, listenAddr)

	// Perform initial sync (blocks), then spawn writer goroutine.
	// Connection continues through handleRequest loop for ACK/NACK/PONG.
	s.serveReplica(rc, ctx)

	return nil
}

func (s *Server) buildReplicateRequest() protocol.Request {
	val := s.replid + protocol.Delimiter + s.listenAddr()
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

	// Update config immediately
	s.connectionMu.Lock()
	s.isReplica = true
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

		s.db.Clear()
		s.startReplicationLoop()
	}()

	return nil
}

// ---------------------------------------------------------------------------
// PROMOTE Command Handler (Replica promotes itself to primary)
// ---------------------------------------------------------------------------

func (s *Server) handlePromote(ctx *RequestContext) error {
	if !s.isReplica {
		s.log().Warn("PROMOTE rejected: already primary")
		return s.responseStatusError(ctx)
	}

	if err := s.promote(); err != nil {
		s.log().Error("PROMOTE failed", "error", err)
		return s.responseStatusError(ctx)
	}

	s.log().Info("promoted to primary")
	return s.responseStatusOk(ctx)
}

func (s *Server) promote() error {
	s.isReplica = false

	// Generate new replid — this node is starting a new timeline
	s.replid = utils.GenerateUniqueID()
	s.seq.Store(0)

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

	return nil
}

// ---------------------------------------------------------------------------
// TOPOLOGY Command Handler (Primary broadcasts topology on replica joining/leaving)
// ---------------------------------------------------------------------------

func (s *Server) handleTopology(ctx *RequestContext) error {
	var peers []string
	if len(ctx.Request.Value) > 0 {
		self := s.listenAddr()
		for _, addr := range strings.Split(string(ctx.Request.Value), protocol.Delimiter) {
			if addr != self {
				peers = append(peers, addr)
			}
		}
	}

	s.peerManager.SavePeers(peers)
	return s.responseStatusOk(ctx)
}

func (s *Server) buildTopologyRequest(replicas map[transport.StreamTransport]*replicaConn) protocol.Request {
	var sb strings.Builder

	// Include the primary itself so replicas can reach it (e.g. for elections)
	sb.WriteString(s.listenAddr())
	sb.WriteString(protocol.Delimiter)

	for _, rc := range replicas {
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
	if s.isReplica {
		return
	}

	snapshot := s.getReplicaSnapshot()
	req := s.buildTopologyRequest(snapshot)
	payload, _ := protocol.EncodeRequest(req)

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
