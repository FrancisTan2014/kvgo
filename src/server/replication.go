package server

import (
	"context"
	"fmt"
	"kvgo/protocol"
	"kvgo/transport"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const replicaSendBuffer = 1024 // max queued writes per replica
const heartbeatInterval = 5 * time.Second
const pongTimeout = 2 * time.Second
const retryInterval = 100 * time.Millisecond

// replicaConn manages a single replica connection with a dedicated write goroutine.
type replicaConn struct {
	transport  transport.StreamTransport
	sendCh     chan []byte  // buffered channel for outgoing writes
	lastReplid string       // primary replid this replica last followed
	lastSeq    uint64       // last seq reported by this replica on connect
	hb         *time.Ticker // heartbeat ticker
	lastWrite  time.Time
	listenAddr string
}

func newReplicaConn(t transport.StreamTransport, replicaLastSeq uint64, lastReplid string, listenAddr string) *replicaConn {
	return &replicaConn{
		transport:  t,
		sendCh:     make(chan []byte, replicaSendBuffer),
		lastReplid: lastReplid,
		lastSeq:    replicaLastSeq,
		hb:         time.NewTicker(heartbeatInterval),
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

	s.wg.Go(func() {
		s.replicationLoop(ctx)
	})
}

func (s *Server) replicationLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			s.log().Info("replication loop cancelled")
			return
		default:
		}

		if !s.isReplica {
			return // promoted
		}

		f, err := s.connectToPrimary()
		if err != nil {
			s.log().Warn("connect failed", "error", err)

			// Backoff with cancellation support
			select {
			case <-ctx.Done():
				return
			case <-time.After(retryInterval):
			}
			continue
		}

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

	if err := st.Send(payload); err != nil {
		_ = st.Close()
		return nil, fmt.Errorf("send replicate request: %w", err)
	}

	// Wait for ack from primary.
	respPayload, err := st.Receive()
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
	} else if resp.Status != protocol.StatusOK {
		_ = st.Close()
		return nil, fmt.Errorf("primary rejected replication: status %d", resp.Status)
	} else {
		// Partial sync - update primarySeq for accurate lag tracking
		s.primarySeq = resp.Seq
	}

	s.log().Info("replication handshake complete")

	s.connectionMu.Lock()
	s.primary = st
	s.lastHeartbeat = time.Now() // Initialize heartbeat timer on successful connection
	s.connectionMu.Unlock()

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
	s.connectionMu.Lock()
	defer s.connectionMu.Unlock()

	for _, rc := range s.replicas {
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
// After this returns, the connection continues through normal handleRequest loop.
func (s *Server) serveReplica(rc *replicaConn, ctx *RequestContext) {
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
		return
	} else {
		s.addNewReplica(rc, ctx)
	}

	if fullSyncMode {
		s.fullResync(rc)
	} else {
		s.partialSync(rc)
	}

	// Spawn writer goroutine to handle async write forwarding + heartbeats
	s.wg.Go(func() {
		s.serveReplicaWriter(rc)
		s.broadcastTopology()
	})
}

func (s *Server) addNewReplica(rc *replicaConn, ctx *RequestContext) {
	s.connectionMu.Lock()
	s.replicas[rc.transport] = rc
	s.connectionMu.Unlock()

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

	return fullSyncMode, rc.transport.Send(resp)
}

// serveReplicaWriter handles async write forwarding and heartbeats for a replica.
// Runs in a dedicated goroutine until connection closes or channel is closed.
func (s *Server) serveReplicaWriter(rc *replicaConn) {
	s.log().Info("serveReplicaWriter started", "replica", rc.listenAddr)
	defer rc.hb.Stop()
	defer func() {
		// Clean up: remove from replicas map and close connection.
		s.connectionMu.Lock()
		delete(s.replicas, rc.transport)
		s.connectionMu.Unlock()
		_ = rc.transport.Close()
		s.broadcastTopology()
		s.log().Info("replica disconnected", "replica", rc.listenAddr)
	}()

	for {
		select {
		case payload, ok := <-rc.sendCh:
			if !ok {
				// Channel closed - replica removed
				return
			}
			if err := rc.transport.SendWithTimeout(payload, 5*time.Second); err != nil {
				s.log().Error("replica write failed", "replica", rc.listenAddr, "error", err)
				return
			}
			rc.lastWrite = time.Now()

		case <-rc.hb.C:
			// Send PING if idle to detect dead connections
			if time.Since(rc.lastWrite) > heartbeatInterval {
				ping, _ := protocol.EncodeRequest(protocol.Request{Cmd: protocol.CmdPing, Seq: s.seq.Load()})
				if err := rc.transport.Send(ping); err != nil {
					s.log().Error("replica ping failed", "replica", rc.listenAddr, "error", err)
					return
				}
				rc.lastWrite = time.Now()
				s.log().Debug("replica ping sent", "replica", rc.listenAddr)
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

		if err := rc.transport.Send(payload); err != nil {
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
		if err := rc.transport.Send(e.payload); err != nil {
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
		if replid, ok := strings.CutPrefix(line, "replid:"); ok {
			s.replid = replid
		}
		if lastSeqStr, ok := strings.CutPrefix(line, "lastSeq:"); ok {
			seq, _ := strconv.ParseUint(lastSeqStr, 10, 64)
			s.lastSeq.Store(seq)
		}
	}

	s.log().Info("restored state", "replid", s.replid, "last_seq", s.lastSeq.Load())
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

	content := fmt.Sprintf("replid:%s\nlastSeq:%d", s.replid, s.lastSeq.Load())
	_, err = s.metaFile.Write([]byte(content))
	if err != nil {
		return err
	}

	return s.metaFile.Sync()
}
