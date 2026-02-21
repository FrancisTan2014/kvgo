package server

import (
	"kvgo/utils"
	"time"
)

// ---------------------------------------------------------------------------
// Quorum Fence (CheckQuorum)
// ---------------------------------------------------------------------------

// fenceLoop runs on the leader to detect quorum loss.
// When the leader has not heard from a majority of replicas within
// an election timeout, it steps down and attempts to rejoin the cluster.
func (s *Server) fenceLoop() {
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
				// Suppress heartbeatLoop election trigger while we reconnect.
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
