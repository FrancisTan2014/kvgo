package server

import (
	"context"
	"errors"
	"fmt"
	"kvgo/protocol"
	"kvgo/transport"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// errRedirect is returned by connectToPrimary when the contacted node
// is a follower and redirects us to the actual leader.
type errRedirect struct {
	addr string
}

func (e *errRedirect) Error() string {
	return fmt.Sprintf("redirected to %s", e.addr)
}

// replicaConn manages a single replica connection with a dedicated write goroutine.
type replicaConn struct {
	transport    transport.StreamTransport
	sendCh       chan []byte  // buffered channel for outgoing writes
	lastReplid   string       // primary replid this replica last followed
	lastSeq      uint64       // last seq reported by this replica on connect
	hb           *time.Ticker // heartbeat ticker
	lastWrite    time.Time
	nodeID       string // replica's unique identity (persisted, stable across restarts)
	listenAddr   string // replica's advertised listen address (for topology broadcast)
	connected    atomic.Bool
	recentActive atomic.Bool // marked on PONG/ACK/NACK; reset each CheckQuorum tick
}

func newReplicaConn(t transport.StreamTransport, replicaLastSeq uint64, lastReplid, nodeID, listenAddr string) *replicaConn {
	return &replicaConn{
		transport:  t,
		sendCh:     make(chan []byte, replicaSendBuffer),
		lastReplid: lastReplid,
		lastSeq:    replicaLastSeq,
		hb:         time.NewTicker(heartbeatInterval),
		nodeID:     nodeID,
		listenAddr: listenAddr,
	}
}

func (s *Server) startReplicationLoop() {
	// Cancel any existing loop
	if s.replCancel != nil {
		s.replCancel()
	}

	// Create new context for this loop
	s.replCtx, s.replCancel = context.WithCancel(s.ctx)
	ctx := s.replCtx

	s.connWg.Go(func() {
		s.replicationLoop(ctx)
	})
}

func (s *Server) replicationLoop(ctx context.Context) {
	var lastRedirFrom string // detect redirect cycles (A→B→A→B)
	for {
		select {
		case <-ctx.Done():
			s.log().Info("replication loop cancelled")
			return
		default:
		}

		if s.isLeader() {
			return // promoted
		}

		f, err := s.connectToPrimary()
		if err != nil {
			var redir *errRedirect
			if errors.As(err, &redir) {
				if redir.addr == s.listenAddr() {
					s.log().Warn("redirect points to self, backing off")
					lastRedirFrom = ""
					select {
					case <-ctx.Done():
						return
					case <-time.After(retryInterval):
					}
					continue
				}
				// Detect redirect cycle: A redirects to B, B redirects to A.
				// Back off to avoid tight loop while peers resolve elections.
				cycle := lastRedirFrom == redir.addr
				lastRedirFrom = s.opts.ReplicaOf
				s.log().Info("redirected to new primary", "address", redir.addr)
				s.connMu.Lock()
				s.opts.ReplicaOf = redir.addr
				s.connMu.Unlock()
				if cycle {
					select {
					case <-ctx.Done():
						return
					case <-time.After(retryInterval):
					}
				}
				continue
			}

			s.log().Warn("connect failed", "error", err)

			// Backoff with cancellation support
			select {
			case <-ctx.Done():
				return
			case <-time.After(retryInterval):
			}
			continue
		}

		lastRedirFrom = ""
		s.receiveFromPrimary(f) // blocks until disconnect or cancel
	}
}

func (s *Server) connectToPrimary() (transport.StreamTransport, error) {
	s.log().Info("connecting to primary", "address", s.opts.ReplicaOf)

	st, err := transport.DialStreamTransport(s.opts.Protocol, s.network(), s.opts.ReplicaOf, 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("connect to primary %s: %w", s.opts.ReplicaOf, err)
	}

	lastSeq := s.lastSeq.Load()
	s.log().Info("connected to primary, sending handshake", "last_seq", lastSeq)

	// Send replicate handshake to register as a replica.
	req := s.buildReplicateRequest()
	payload, err := protocol.EncodeRequest(req)
	if err != nil {
		panic("encode error")
	}

	if err := st.Send(context.Background(), payload); err != nil {
		_ = st.Close()
		return nil, fmt.Errorf("send replicate request: %w", err)
	}

	// Wait for ack from primary.
	respPayload, err := st.Receive(context.Background())
	if err != nil {
		_ = st.Close()
		return nil, fmt.Errorf("read replicate response: %w", err)
	}

	resp, err := protocol.DecodeResponse(respPayload)
	if err != nil {
		_ = st.Close()
		return nil, fmt.Errorf("decode replicate response: %w", err)
	}

	if resp.Status == protocol.StatusFullResync {
		s.log().Info("primary requested full resync, clearing local DB")
		s.db.Clear()
		if len(resp.Value) > 0 {
			s.replid = string(resp.Value)
		}
		// Initialize primarySeq from response to avoid initial staleness
		s.primarySeq = resp.Seq
	} else if resp.Status == protocol.StatusReadOnly && len(resp.Value) > 0 {
		// Redirect: the node we contacted is a follower; it told us who the leader is.
		leaderAddr := string(resp.Value)
		_ = st.Close()
		return nil, &errRedirect{addr: leaderAddr}
	} else if resp.Status != protocol.StatusOK {
		_ = st.Close()
		return nil, fmt.Errorf("primary rejected replication: status %d", resp.Status)
	} else {
		// Partial sync - update primarySeq for accurate lag tracking
		s.primarySeq = resp.Seq
	}

	s.log().Info("replication handshake complete")

	s.connMu.Lock()
	s.primary = st
	s.lastHeartbeat = time.Now() // Initialize heartbeat timer on successful connection
	s.connMu.Unlock()

	return st, nil
}

// receiveFromPrimary receives replication stream from primary.
// Delegates to standard handleRequest dispatcher.
func (s *Server) receiveFromPrimary(t transport.StreamTransport) {
	s.log().Info("receiveFromPrimary started")
	defer func() {
		s.log().Info("receiveFromPrimary exiting")
		if s.primary != nil {
			_ = s.primary.Close()
			s.primary = nil
		}
	}()

	// Use standard request handler
	s.handleRequest(t, s.opts.ReadTimeout)
	s.log().Info("handleRequest returned")
}

// forwardToReplicas sends a write to all connected replicas (non-blocking).
func (s *Server) forwardToReplicas(payload []byte, seq uint64) {
	s.connMu.Lock()
	defer s.connMu.Unlock()

	for _, rc := range s.replicas {
		if !rc.connected.Load() {
			continue
		}

		select {
		case rc.sendCh <- payload:
			// queued
		default:
			// channel full, replica is slow — drop the write
			s.log().Warn("replica send buffer full, dropping write", "replica", rc.listenAddr, "seq", seq)
		}
	}
}

// serveReplica performs initial sync, then spawns a writer goroutine.
// The writer forwards replicated writes and heartbeats to the replica.
// ACK/NACK/PONG travel back through the peer channel (separate connection).
func (s *Server) serveReplica(rc *replicaConn) bool {
	// Check timeline compatibility
	if rc.lastReplid != "" && rc.lastReplid != s.replid {
		s.log().Error("incompatible replid detected — replica was following different primary",
			"replica", rc.listenAddr,
			"replica_replid", rc.lastReplid,
			"primary_replid", s.replid,
			"replica_lastSeq", rc.lastSeq)
		// For now, force full resync on incompatible timeline
	}

	fullSyncMode, err := s.respondSyncMode(rc)
	if err != nil {
		s.log().Error("Failed to send sync mode response", "replica", rc.listenAddr)
		return false
	} else {
		s.addNewReplica(rc)
	}

	if fullSyncMode {
		s.fullResync(rc)
	} else {
		s.partialSync(rc)
	}

	// Mark connected only after sync completes — writes forwarded before this point go direct
	rc.connected.Store(true)

	// Spawn writer goroutine to handle async write forwarding + heartbeats
	s.connWg.Go(func() {
		s.serveReplicaWriter(rc)
	})

	s.broadcastTopology()

	return true
}

func (s *Server) addNewReplica(rc *replicaConn) {
	s.connMu.Lock()
	s.replicas[rc.nodeID] = rc
	s.connMu.Unlock()
}

func (s *Server) respondSyncMode(rc *replicaConn) (bool, error) {
	exists := s.existsInBacklog(rc.lastSeq)
	fullSyncMode := rc.lastReplid != s.replid || !exists

	status := protocol.StatusOK
	if fullSyncMode {
		status = protocol.StatusFullResync
	}

	resp, err := protocol.EncodeResponse(protocol.Response{
		Status: status,
		Value:  []byte(s.replid),
		Seq:    s.seq.Load(), // Include primary's current seq so replica can track lag immediately
	})

	if err != nil {
		panic("encode error")
	}

	return fullSyncMode, rc.transport.Send(context.Background(), resp)
}

// serveReplicaWriter handles async write forwarding and heartbeats for a replica.
// Runs in a dedicated goroutine until connection closes or channel is closed.
func (s *Server) serveReplicaWriter(rc *replicaConn) {
	s.log().Info("serveReplicaWriter started", "replica", rc.listenAddr)

	// snapshot for `defer`, in-case we close the reconnected replica connection
	t := rc.transport
	reconnecting := false

	defer rc.hb.Stop()
	defer func() {
		if reconnecting {
			// Channel closed by updateReplicaConn — it owns cleanup
			return
		}
		// Clean up: remove from replicas map and close connection.
		rc.connected.Store(false)
		_ = t.Close()
		s.broadcastTopology()
		s.log().Info("replica disconnected", "replica", rc.listenAddr)
	}()

	for {
		select {
		case payload, ok := <-rc.sendCh:
			if !ok {
				reconnecting = true
				// Channel closed
				return
			}
			sendCtx, sendCancel := context.WithTimeout(context.Background(), s.opts.WriteTimeout)
			err := rc.transport.Send(sendCtx, payload)
			sendCancel()
			if err != nil {
				s.log().Error("replica write failed", "replica", rc.listenAddr, "error", err)
				return
			}
			rc.lastWrite = time.Now()

		case <-rc.hb.C:
			// Send PING if idle to detect dead connections.
			// Uses Request() so the PONG comes back as a Response on the
			// same multiplexed stream — no reader goroutine needed.
			if time.Since(rc.lastWrite) > heartbeatInterval {
				ping, _ := protocol.EncodeRequest(protocol.NewPingRequest(s.seq.Load(), s.term.Load()))

				// Type-assert to RequestTransport (MultiplexedTransport implements both).
				rt, ok := rc.transport.(transport.RequestTransport)
				if !ok {
					s.log().Error("replica transport does not support Request")
					return
				}

				pingCtx, pingCancel := context.WithTimeout(context.Background(), s.pingTimeout())
				resp, err := rt.Request(pingCtx, ping)
				pingCancel()
				if err != nil {
					s.log().Error("replica ping failed", "replica", rc.listenAddr, "error", err)
					return
				}
				rc.lastWrite = time.Now()
				rc.recentActive.Store(true)

				// Process PONG (encoded as Response with term in Value).
				s.processPongResponse(resp, rc)
			}
		}
	}
}

func (s *Server) fullResync(rc *replicaConn) {
	s.log().Info("full sync started", "replica", rc.listenAddr)

	s.db.Range(func(key string, value []byte) bool {
		req := protocol.Request{
			Cmd:   protocol.CmdPut,
			Key:   []byte(key),
			Value: value,
			Seq:   s.seq.Load(),
		}
		payload, err := protocol.EncodeRequest(req)
		if err != nil {
			s.log().Error("replica encode failed", "replica", rc.listenAddr, "error", err)
			return false
		}

		if err := rc.transport.Send(context.Background(), payload); err != nil {
			s.log().Error("replica write failed", "replica", rc.listenAddr, "error", err)
			return false
		}

		return true
	})

	s.log().Info("full sync completed", "replica", rc.listenAddr)
}

func (s *Server) partialSync(rc *replicaConn) {
	s.log().Info("partial sync started", "replica", rc.listenAddr)

	err := s.forwardBacklog(rc.lastSeq, func(e backlogEntry) error {
		if err := rc.transport.Send(context.Background(), e.payload); err != nil {
			s.log().Error("replica write failed", "replica", rc.listenAddr, "error", err)
			return err
		}
		return nil
	})

	if err == nil {
		s.log().Info("partial sync completed", "replica", rc.listenAddr)
	} else {
		s.log().Error("partial sync failed", "replica", rc.listenAddr)
	}
}

func (s *Server) getMetaPath() string {
	return filepath.Join(s.opts.DataDir, "replication.meta")
}

func (s *Server) restoreState() error {
	data, err := os.ReadFile(s.getMetaPath())
	if os.IsNotExist(err) {
		// First run, no state to restore
		return nil
	}
	if err != nil {
		return fmt.Errorf("read replication.meta: %w", err)
	}

	// Parse lines: replid:xxx\nlastSeq:yyy
	for line := range strings.SplitSeq(string(data), "\n") {
		if nodeID, ok := strings.CutPrefix(line, "nodeID:"); ok {
			s.nodeID = nodeID
		}
		if replid, ok := strings.CutPrefix(line, "replid:"); ok {
			s.replid = replid
		}
		if lastSeqStr, ok := strings.CutPrefix(line, "lastSeq:"); ok {
			seq, _ := strconv.ParseUint(lastSeqStr, 10, 64)
			s.lastSeq.Store(seq)
		}
		if lastTermStr, ok := strings.CutPrefix(line, "term:"); ok {
			term, _ := strconv.ParseUint(lastTermStr, 10, 64)
			s.term.Store(term)
		}
		if votedFor, ok := strings.CutPrefix(line, "votedFor:"); ok {
			s.votedFor = votedFor
		}
		if peersStr, ok := strings.CutPrefix(line, "peers:"); ok {
			if peersStr != "" {
				var peers []PeerInfo
				for _, entry := range strings.Split(peersStr, ",") {
					nodeID, addr, found := strings.Cut(entry, "@")
					if found && nodeID != "" && addr != "" {
						peers = append(peers, PeerInfo{NodeID: nodeID, Addr: addr})
					}
				}
				if s.peerManager != nil {
					s.peerManager.MergePeers(peers)
				}
			}
		}
	}

	s.log().Info("restored state", "node_id", s.nodeID, "replid", s.replid, "last_seq", s.lastSeq.Load(), "term", s.term.Load(), "voted_for", s.votedFor)
	return nil
}

func (s *Server) storeState() error {
	var err error
	if s.metaFile == nil {
		s.metaFile, err = os.OpenFile(s.getMetaPath(), os.O_CREATE|os.O_RDWR, 0644)
		if err != nil {
			return err
		}
	}

	// Seek to start and truncate before writing
	if _, err = s.metaFile.Seek(0, 0); err != nil {
		return err
	}
	if err = s.metaFile.Truncate(0); err != nil {
		return err
	}

	var peerStr string
	if s.peerManager != nil {
		var parts []string
		for _, pi := range s.peerManager.PeerInfos() {
			parts = append(parts, pi.NodeID+"@"+pi.Addr)
		}
		peerStr = strings.Join(parts, ",")
	}

	content := fmt.Sprintf("nodeID:%s\nreplid:%s\nlastSeq:%d\nterm:%d\nvotedFor:%s\npeers:%s", s.nodeID, s.replid, s.lastSeq.Load(), s.term.Load(), s.votedFor, peerStr)
	_, err = s.metaFile.Write([]byte(content))
	if err != nil {
		return err
	}

	return s.metaFile.Sync()
}
