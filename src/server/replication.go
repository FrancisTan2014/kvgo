package server

import (
	"fmt"
	"kvgo/protocol"
	"net"
	"time"
)

const replicaSendBuffer = 1024 // max queued writes per replica
const heartbeatInterval = 5 * time.Second
const pongTimeout = 2 * time.Second

// replicaConn manages a single replica connection with a dedicated write goroutine.
type replicaConn struct {
	conn      net.Conn
	framer    *protocol.Framer
	sendCh    chan []byte  // buffered channel for outgoing writes
	lastSeq   uint64       // last seq reported by this replica on connect
	hb        *time.Ticker // heartbeat ticker
	lastWrite time.Time
}

func newReplicaConn(conn net.Conn, replicaLastSeq uint64) *replicaConn {
	return &replicaConn{
		conn:    conn,
		framer:  protocol.NewConnFramer(conn),
		sendCh:  make(chan []byte, replicaSendBuffer),
		lastSeq: replicaLastSeq,
		hb:      time.NewTicker(heartbeatInterval),
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
	req := protocol.Request{Op: protocol.OpReplicate, Seq: lastSeq}
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

	if resp.Status != protocol.StatusOK {
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
				s.logf("replication PUT %q <- %d bytes (seq=%d)", key, len(req.Value), req.Seq)
			}
		}
	}
}

// forwardToReplicas sends a write to all connected replicas (non-blocking).
func (s *Server) forwardToReplicas(req protocol.Request, seq uint64) {
	req.Seq = seq
	payload, err := protocol.EncodeRequest(req)
	if err != nil {
		s.logf("failed to encode replica request: %v", err)
		return
	}

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

	for {
		select {
		case payload := <-rc.sendCh:
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
