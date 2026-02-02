package server

import (
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

func (s *Server) connectToPrimary() error {
	if s.opts.ReplicaOf == "" {
		return nil
	}

	s.logf("connecting to primary at %s", s.opts.ReplicaOf)

	conn, err := net.DialTimeout(s.network(), s.opts.ReplicaOf, 10*time.Second)
	if err != nil {
		return fmt.Errorf("connect to primary %s: %w", s.opts.ReplicaOf, err)
	}

	lastSeq := s.lastSeq.Load()
	s.logf("connected to primary, sending replicate handshake (last_seq=%d)", lastSeq)

	// Send replicate handshake to register as a replica.
	f := protocol.NewConnFramer(conn)
	req := protocol.Request{Op: protocol.OpReplicate, Seq: lastSeq, Value: []byte(s.replid)}
	payload, err := protocol.EncodeRequest(req)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("encode replicate request: %w", err)
	}

	if err := f.Write(payload); err != nil {
		_ = conn.Close()
		return fmt.Errorf("send replicate request: %w", err)
	}

	// Wait for ack from primary.
	respPayload, err := f.Read()
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("read replicate response: %w", err)
	}

	resp, err := protocol.DecodeResponse(respPayload)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("decode replicate response: %w", err)
	}

	if resp.Status == protocol.StatusFullResync {
		s.logf("primary requested full resync, clearing local DB")
		s.db.Clear()
		if len(resp.Value) > 0 {
			s.replid = string(resp.Value)
		}
	} else if resp.Status != protocol.StatusOK {
		_ = conn.Close()
		return fmt.Errorf("primary rejected replication: status %d", resp.Status)
	}

	s.logf("replication handshake complete, receiving writes")

	// Receive forwarded writes from primary in background.
	s.wg.Go(func() {
		defer conn.Close()
		s.receiveFromPrimary(f)
	})

	s.primary = conn
	return nil
}

func (s *Server) receiveFromPrimary(f *protocol.Framer) {
	for {
		payload, err := f.Read()
		if err != nil {
			s.logf("replication stream closed: %v", err)
			return
		}

		req, err := protocol.DecodeRequest(payload)
		if err != nil {
			s.logf("replication decode error: %v", err)
			return
		}

		// Apply write locally (fire-and-forget from primary's perspective).
		if req.Op == protocol.OpPut {
			key := string(req.Key)
			if err := s.db.Put(key, req.Value); err != nil {
				s.logf("replication PUT %q failed: %v (seq=%d)", key, err, req.Seq)
			} else {
				s.lastSeq.Store(req.Seq)
				if err = s.storeState(); err != nil {
					s.logf("error occurred on storing replica state: %v", err)
				}
				s.logf("replication PUT %q <- %d bytes (seq=%d)", key, len(req.Value), req.Seq)
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
			s.logf("replica %s send buffer full, dropping write (seq=%d)", rc.conn.RemoteAddr(), seq)
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
		s.logf("replica %s disconnected", rc.conn.RemoteAddr())
	}()

	seqIndex := s.getSeqIndex(rc.lastSeq)
	if rc.lastReplid != s.replid || seqIndex == SeqNotFound {
		s.fullResync(rc)
	} else {
		s.partialSync(rc, seqIndex)
	}

	for {
		select {
		case payload, ok := <-rc.sendCh:
			if !ok {
				// Channel closed
				return
			}
			if err := rc.conn.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
				s.logf("replica %s set deadline failed: %v", rc.conn.RemoteAddr(), err)
				return
			}
			if err := rc.framer.Write(payload); err != nil {
				s.logf("replica %s write failed: %v", rc.conn.RemoteAddr(), err)
				return
			}
			rc.lastWrite = time.Now()

		case <-rc.hb.C:
			// heartbeat: ping replica if idle
			if time.Since(rc.lastWrite) > heartbeatInterval {
				ping, _ := protocol.EncodeRequest(protocol.Request{Op: protocol.OpPing})
				if err := rc.framer.Write(ping); err != nil {
					s.logf("replica %s ping failed: %v", rc.conn.RemoteAddr(), err)
					return
				}

				s.logf("replica %s ping", rc.conn.RemoteAddr())
				if _, err := rc.framer.ReadWithTimeout(pongTimeout); err != nil {
					s.logf("replica %s no pong within %v, disconnecting", rc.conn.RemoteAddr(), pongTimeout)
					return
				}
			}
		}
	}
}

func (s *Server) fullResync(rc *replicaConn) {
	s.logf("full sync started for replica %s", rc.conn.RemoteAddr())

	// No deadline during full sync (could be large DB)
	_ = rc.conn.SetWriteDeadline(time.Time{})

	fullSyncResp, _ := protocol.EncodeResponse(protocol.Response{Status: protocol.StatusFullResync, Value: []byte(s.replid)})
	_ = rc.framer.Write(fullSyncResp)

	seq := s.seq.Load()
	s.db.Range(func(key string, value []byte) bool {
		req := protocol.Request{
			Op:    protocol.OpPut,
			Key:   []byte(key),
			Value: value,
			Seq:   seq,
		}
		payload, err := protocol.EncodeRequest(req)
		if err != nil {
			s.logf("replica %s encode failed: %v", rc.conn.RemoteAddr(), err)
			return false
		}

		if err := rc.framer.Write(payload); err != nil {
			s.logf("replica %s write failed: %v", rc.conn.RemoteAddr(), err)
			return false
		}

		return true
	})

	s.logf("full sync completed for replica %s", rc.conn.RemoteAddr())
}

func (s *Server) partialSync(rc *replicaConn, seqIndex int) {
	s.logf("partial sync started for replica %s", rc.conn.RemoteAddr())

	_ = rc.conn.SetWriteDeadline(time.Time{})

	psyncResp, _ := protocol.EncodeResponse(protocol.Response{Status: protocol.StatusOK})
	_ = rc.framer.Write(psyncResp)
	for _, payload := range s.replBacklog[seqIndex+1:] {
		if err := rc.framer.Write(payload); err != nil {
			s.logf("replica %s write failed: %v", rc.conn.RemoteAddr(), err)
		}
	}
	s.logf("partial sync completed for replica %s", rc.conn.RemoteAddr())
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

	s.logf("restored state: replid=%s lastSeq=%d", s.replid, s.lastSeq.Load())
	return nil
}

func (s *Server) storeState() error {
	var err error
	if s.metaFile == nil {
		s.metaFile, err = os.OpenFile(s.getMetaPath(), os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0644)
		if err != nil {
			return err
		}
	}

	content := fmt.Sprintf("replid:%s\nlastSeq:%d", s.replid, s.lastSeq.Load())
	_, err = s.metaFile.Write([]byte(content))
	if err != nil {
		return err
	}

	return s.metaFile.Sync()
}
