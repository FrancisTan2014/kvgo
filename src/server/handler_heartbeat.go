package server

import (
	"kvgo/protocol"
	"kvgo/utils"
	"time"
)

// ---------------------------------------------------------------------------
// PING/PONG Handlers (Heartbeat)
// ---------------------------------------------------------------------------

// handlePing processes PING requests, which serve as heartbeat messages in replication.
//
// Primary → Replica: Sends PING with current seq and term via Request().
// Replica responds with a standard Response carrying its term in Value.
// Primary reads the term from the Response to detect stale-primary fencing.
func (s *Server) handlePing(ctx *RequestContext) error {
	req := &ctx.Request

	if s.isLeader() {
		// Primary shouldn't receive PING (replicas send PONG)
		return nil
	}

	// Extract sender's term from Value.
	senderTerm := protocol.ParsePingTerm(req.Value)

	myTerm := s.term.Load()

	// Reject heartbeats from a stale (lower-term) leader.
	if senderTerm < myTerm {
		s.log().Debug("PING rejected: stale term", "senderTerm", senderTerm, "myTerm", myTerm)
		return s.respondPong(ctx, myTerm)
	}

	// Higher term from sender — step down (if candidate) and adopt the new term.
	if senderTerm > myTerm {
		s.log().Info("PING: discovered higher term, updating", "senderTerm", senderTerm, "myTerm", myTerm)
		s.roleMu.Lock()
		s.term.Store(senderTerm)
		s.votedFor = ""
		if err := s.storeState(); err != nil {
			s.log().Error("failed to persist state on term bump", "error", err)
		}
		s.roleMu.Unlock()
		s.becomeFollower()
	}

	s.lastHeartbeat = time.Now()
	s.primarySeq = req.Seq
	s.fenced.Store(false) // connected to a real leader, safe to participate in elections again

	return s.respondPong(ctx, s.term.Load())
}

// respondPong sends a PONG as a standard Response with term in Value.
// Using writeResponse (not a CmdPong request) lets the primary use
// Request() on the multiplexed transport and receive the PONG synchronously.
func (s *Server) respondPong(ctx *RequestContext, term uint64) error {
	return s.writeResponse(ctx.StreamTransport, protocol.NewPongResponse(term))
}

// processPongResponse extracts the replica's term from a PONG Response
// and steps down if the replica has a higher term (stale primary fencing).
func (s *Server) processPongResponse(respPayload []byte, rc *replicaConn) {
	resp, err := protocol.DecodeResponse(respPayload)
	if err != nil {
		s.log().Warn("failed to decode PONG response", "replica", rc.listenAddr, "error", err)
		return
	}

	respTerm := protocol.ParsePongTerm(resp.Value)

	myTerm := s.term.Load()
	if respTerm > myTerm {
		wasLeader := s.isLeader()
		s.log().Info("PONG: discovered higher term from replica, stepping down", "respTerm", respTerm, "myTerm", myTerm)
		s.roleMu.Lock()
		s.term.Store(respTerm)
		s.votedFor = ""
		if err := s.storeState(); err != nil {
			s.log().Error("failed to persist state after step-down", "error", err)
		}
		s.roleMu.Unlock()

		// After stepping down, reconnect to the cluster via any known peer.
		// Set lastHeartbeat first to suppress monitorHeartbeat election trigger
		// while the replication loop connects.
		if wasLeader {
			s.lastHeartbeat = time.Now()
		}
		s.becomeFollower()

		if wasLeader {
			if addr, ok := s.peerManager.AnyAddr(); ok {
				go func() {
					if err := s.relocate(addr); err != nil {
						s.log().Warn("post-stepdown relocate failed", "addr", addr, "error", err)
					}
				}()
			}
		}
	}
}

// getRoleChangedCh returns the current role-change notification channel.
// The caller selects on the returned channel; becomeX() closes it to wake all waiters.
// Must snapshot under lock — the field is swapped on every role change.
func (s *Server) getRoleChangedCh() <-chan struct{} {
	s.roleMu.Lock()
	defer s.roleMu.Unlock()
	return s.roleChanged
}

// monitorHeartbeat runs on followers to detect primary failure.
// When lastHeartbeat exceeds the election timeout, triggers becomeCandidate.
func (s *Server) monitorHeartbeat() {
	for {
		if s.isLeader() {
			ch := s.getRoleChangedCh()
			select {
			case <-ch: // woken by role change
				continue
			case <-s.ctx.Done():
				return
			}
		}

		select {
		case <-s.ctx.Done():
			return
		case <-time.After(heartbeatInterval):
			if s.fenced.Load() {
				continue // fenced node must not trigger elections; wait for leader contact
			}
			if time.Since(s.lastHeartbeat) >= randomElectionTimeout() {
				_ = s.becomeCandidate() // follower → candidate, or candidate retries with new term
			}
		}
	}
}

func (s *Server) monitorFence() {
	for {
		if !s.isLeader() {
			ch := s.getRoleChangedCh()
			select {
			case <-ch: // woken by role change
				continue
			case <-s.ctx.Done():
				return
			}
		}

		select {
		case <-s.ctx.Done():
			return
		case <-time.After(randomElectionTimeout()):
			if s.fenceDetected() {
				s.log().Warn("fence detected: lost quorum, stepping down")
				// Suppress monitorHeartbeat election trigger while we reconnect.
				// Without this, the stale lastHeartbeat causes an immediate election
				// that cancels our replication loop before it can connect.
				s.lastHeartbeat = time.Now()
				s.fenced.Store(true)
				if addr, ok := s.peerManager.AnyAddr(); ok {
					if err := s.relocate(addr); err != nil {
						s.log().Error("fence relocate failed", "addr", addr, "error", err)
					}
				} else {
					s.log().Error("fence detected but no peers available")
					s.primarySeq = s.lastSeq.Load() // prevent false staleness
					s.becomeFollower()
					s.teardownReplicationState()
				}
			}
		}
	}
}

func (s *Server) fenceDetected() bool {
	s.connMu.RLock()
	defer s.connMu.RUnlock()

	if len(s.replicas) == 0 {
		// standalone primary — quorum check not applicable
		return false
	}

	activeReplicas := 1 // self
	for _, rc := range s.replicas {
		if rc.recentActive.Load() {
			activeReplicas++
		}
		rc.recentActive.Store(false)
	}

	quorum := utils.ComputeQuorum(len(s.replicas) + 1)
	return activeReplicas < quorum
}
