package server

import (
	"context"
	"fmt"
	"kvgo/protocol"
	"net"
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
	conn       net.Conn
	framer     *protocol.Framer
	sendCh     chan []byte  // buffered channel for outgoing writes
	lastReplid string       // primary replid this replica last followed
	lastSeq    uint64       // last seq reported by this replica on connect
	hb         *time.Ticker // heartbeat ticker
	lastWrite  time.Time
}

func newReplicaConn(conn net.Conn, replicaLastSeq uint64, lastReplid string) *replicaConn {
	return &replicaConn{
		conn:       conn,
		framer:     protocol.NewConnFramer(conn),
		sendCh:     make(chan []byte, replicaSendBuffer),
		lastReplid: lastReplid,
		lastSeq:    replicaLastSeq,
		hb:         time.NewTicker(heartbeatInterval),
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

		s.receiveFromPrimary(ctx, f) // blocks until disconnect or cancel
	}
}

func (s *Server) connectToPrimary() (*protocol.Framer, error) {
	s.log().Info("connecting to primary", "address", s.opts.ReplicaOf)

	conn, err := net.DialTimeout(s.network(), s.opts.ReplicaOf, 10*time.Second)
	if err != nil {
		return nil, fmt.Errorf("connect to primary %s: %w", s.opts.ReplicaOf, err)
	}

	lastSeq := s.lastSeq.Load()
	s.log().Info("connected to primary, sending handshake", "last_seq", lastSeq)

	// Send replicate handshake to register as a replica.
	f := protocol.NewConnFramer(conn)
	req := protocol.Request{Cmd: protocol.CmdReplicate, Seq: lastSeq, Value: []byte(s.replid)}
	payload, err := protocol.EncodeRequest(req)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("encode replicate request: %w", err)
	}

	if err := f.Write(payload); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("send replicate request: %w", err)
	}

	// Wait for ack from primary.
	respPayload, err := f.Read()
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("read replicate response: %w", err)
	}

	resp, err := protocol.DecodeResponse(respPayload)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("decode replicate response: %w", err)
	}

	if resp.Status == protocol.StatusFullResync {
		s.log().Info("primary requested full resync, clearing local DB")
		s.db.Clear()
		if len(resp.Value) > 0 {
			s.replid = string(resp.Value)
		}
	} else if resp.Status != protocol.StatusOK {
		_ = conn.Close()
		return nil, fmt.Errorf("primary rejected replication: status %d", resp.Status)
	}

	s.log().Info("replication handshake complete")

	s.primary = conn
	return f, nil
}

func (s *Server) receiveFromPrimary(ctx context.Context, f *protocol.Framer) {
	defer func() {
		if s.primary != nil {
			_ = s.primary.Close()
			s.primary = nil
		}
	}()

	for {
		select {
		case <-ctx.Done():
			s.log().Info("replication receive cancelled")
			return
		default:
		}

		payload, err := f.Read()
		if err != nil {
			s.log().Info("replication stream closed", "error", err)
			return
		}

		req, err := protocol.DecodeRequest(payload)
		if err != nil {
			s.log().Error("replication decode error", "error", err)
			return
		}

		// Apply write locally (fire-and-forget from primary's perspective).
		if req.Cmd == protocol.CmdPut {
			key := string(req.Key)
			if err := s.db.Put(key, req.Value); err != nil {
				s.log().Error("replication PUT failed", "key", key, "seq", req.Seq, "error", err)
			} else {
				s.lastSeq.Store(req.Seq)
				if err = s.storeState(); err != nil {
					s.log().Error("failed to store replica state", "error", err)
				}
			}
		}
	}
}

// forwardToReplicas sends a write to all connected replicas (non-blocking).
func (s *Server) forwardToReplicas(payload []byte, seq uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, rc := range s.replicas {
		select {
		case rc.sendCh <- payload:
			// queued
		default:
			// channel full, replica is slow — drop the write
			s.log().Warn("replica send buffer full, dropping write", "replica", rc.conn.RemoteAddr(), "seq", seq)
		}
	}
}

// serveReplica runs a dedicated goroutine per replica that forwards writes
// and sends heartbeats when idle to detect dead connections.
func (s *Server) serveReplica(rc *replicaConn) {
	defer rc.hb.Stop()
	defer func() {
		// Clean up: remove from replicas map and close connection.
		s.mu.Lock()
		delete(s.replicas, rc.conn)
		s.mu.Unlock()
		_ = rc.conn.Close()
		s.log().Info("replica disconnected", "replica", rc.conn.RemoteAddr())
	}()

	exists := s.existsInBacklog(rc.lastSeq)
	if rc.lastReplid != s.replid || !exists {
		s.fullResync(rc)
	} else {
		s.partialSync(rc)
	}

	for {
		select {
		case payload, ok := <-rc.sendCh:
			if !ok {
				// Channel closed
				return
			}
			if err := rc.conn.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
				s.log().Error("replica set deadline failed", "replica", rc.conn.RemoteAddr(), "error", err)
				return
			}
			if err := rc.framer.Write(payload); err != nil {
				s.log().Error("replica write failed", "replica", rc.conn.RemoteAddr(), "error", err)
				return
			}
			rc.lastWrite = time.Now()

		case <-rc.hb.C:
			// heartbeat: ping replica if idle
			if time.Since(rc.lastWrite) > heartbeatInterval {
				ping, _ := protocol.EncodeRequest(protocol.Request{Cmd: protocol.CmdPing})
				if err := rc.framer.Write(ping); err != nil {
					s.log().Error("replica ping failed", "replica", rc.conn.RemoteAddr(), "error", err)
					return
				}

				s.log().Debug("replica ping sent", "replica", rc.conn.RemoteAddr())
				if _, err := rc.framer.ReadWithTimeout(pongTimeout); err != nil {
					s.log().Warn("replica no pong, disconnecting", "replica", rc.conn.RemoteAddr(), "timeout", pongTimeout)
					return
				}
			}
		}
	}
}

func (s *Server) fullResync(rc *replicaConn) {
	s.log().Info("full sync started", "replica", rc.conn.RemoteAddr())

	// No deadline during full sync (could be large DB)
	_ = rc.conn.SetWriteDeadline(time.Time{})

	fullSyncResp, _ := protocol.EncodeResponse(protocol.Response{Status: protocol.StatusFullResync, Value: []byte(s.replid)})
	_ = rc.framer.Write(fullSyncResp)

	seq := s.seq.Load()
	s.db.Range(func(key string, value []byte) bool {
		req := protocol.Request{
			Cmd:   protocol.CmdPut,
			Key:   []byte(key),
			Value: value,
			Seq:   seq,
		}
		payload, err := protocol.EncodeRequest(req)
		if err != nil {
			s.log().Error("replica encode failed", "replica", rc.conn.RemoteAddr(), "error", err)
			return false
		}

		if err := rc.framer.Write(payload); err != nil {
			s.log().Error("replica write failed", "replica", rc.conn.RemoteAddr(), "error", err)
			return false
		}

		return true
	})

	s.log().Info("full sync completed", "replica", rc.conn.RemoteAddr())
}

func (s *Server) partialSync(rc *replicaConn) {
	s.log().Info("partial sync started", "replica", rc.conn.RemoteAddr())

	_ = rc.conn.SetWriteDeadline(time.Time{})

	psyncResp, _ := protocol.EncodeResponse(protocol.Response{Status: protocol.StatusOK})
	_ = rc.framer.Write(psyncResp)

	err := s.forwardBacklog(rc.lastSeq, func(e backlogEntry) error {
		if err := rc.framer.Write(e.payload); err != nil {
			s.log().Error("replica write failed", "replica", rc.conn.RemoteAddr(), "error", err)
			return err
		}
		return nil
	})

	if err == nil {
		s.log().Info("partial sync completed", "replica", rc.conn.RemoteAddr())
	} else {
		s.log().Error("partial sync failed", "replica", rc.conn.RemoteAddr())
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

// ---------------------------------------------------------------------------
// Replication Command Handlers
// ---------------------------------------------------------------------------

// handleReplicate handles the REPLICATE command from a replica wanting to sync.
// This handler takes over the connection - serveReplica blocks until disconnection.
func (s *Server) handleReplicate(ctx *RequestContext) error {
	if s.isReplica {
		s.log().Warn("REPLICATE rejected: node is replica")
		return s.writeResponse(ctx.Framer, protocol.Response{Status: protocol.StatusError})
	}

	req, err := protocol.DecodeRequest(ctx.Payload)
	if err != nil {
		s.log().Error("failed to decode replicate request", "replica", ctx.Conn.RemoteAddr())
		return s.writeResponse(ctx.Framer, protocol.Response{Status: protocol.StatusError})
	}

	replid := string(req.Value)
	rc := newReplicaConn(ctx.Conn, req.Seq, replid)
	s.mu.Lock()
	s.replicas[ctx.Conn] = rc
	s.mu.Unlock()

	// Mark connection as taken over BEFORE blocking call.
	// serveReplica will handle all communication until replica disconnects.
	ctx.takenOver = true

	// serveReplica blocks until replica disconnects.
	// Connection cleanup happens inside serveReplica's defer.
	s.serveReplica(rc)

	return nil
}

// handleReplicaOf handles the REPLICAOF command to dynamically change replication target.
func (s *Server) handleReplicaOf(ctx *RequestContext) error {
	req, err := protocol.DecodeRequest(ctx.Payload)
	if err != nil {
		return s.writeResponse(ctx.Framer, protocol.Response{Status: protocol.StatusError})
	}

	if err := s.relocate(string(req.Value)); err != nil {
		s.log().Error("REPLICAOF failed", "error", err)
		return s.writeResponse(ctx.Framer, protocol.Response{Status: protocol.StatusError})
	}
	return s.writeResponse(ctx.Framer, protocol.Response{Status: protocol.StatusOK})
}
