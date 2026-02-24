// election.go — role state machine for leader election.
// Roles, valid transitions, and becomeX() methods live here.

package server

import (
	"context"
	"fmt"
	"kvgo/protocol"
	"kvgo/utils"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"
)

// randomElectionTimeout returns a randomized duration in [electionTimeout, 2*electionTimeout).
// Jitter prevents multiple followers from timing out simultaneously (split vote).
func randomElectionTimeout() time.Duration {
	return electionTimeout + time.Duration(rand.Int64N(int64(electionTimeout)))
}

// Role represents a node's current role in the cluster.
type Role uint32

const (
	RoleFollower Role = iota // zero value = follower (safe default)
	RolePreCandidate
	RoleCandidate
	RoleLeader
)

func (r Role) String() string {
	switch r {
	case RoleFollower:
		return "follower"
	case RolePreCandidate:
		return "pre-candidate"
	case RoleCandidate:
		return "candidate"
	case RoleLeader:
		return "leader"
	default:
		return "unknown"
	}
}

// validTransition encodes the 5 legal arrows from the state diagram.
// Follower→Follower is allowed as a no-op (simplifies step-down on higher term).
func validTransition(from, to Role) bool {
	switch {
	case from == RoleFollower && to == RoleFollower:
		return true
	case from == RoleFollower && to == RolePreCandidate:
		return true
	case from == RoleFollower && to == RoleCandidate:
		return true
	case from == RolePreCandidate && to == RoleFollower:
		return true
	case from == RolePreCandidate && to == RoleCandidate:
		return true
	case from == RoleCandidate && to == RolePreCandidate:
		return true
	case from == RoleCandidate && to == RoleCandidate:
		return true
	case from == RoleCandidate && to == RoleLeader:
		return true
	case from == RoleCandidate && to == RoleFollower:
		return true
	case from == RoleLeader && to == RoleFollower:
		return true
	default:
		return false
	}
}

// notifyRoleChanged closes the current roleChanged channel to wake all waiters
// (e.g. monitorHeartbeat), then installs a fresh channel for subsequent waits.
func (s *Server) notifyRoleChanged() {
	s.roleMu.Lock()
	defer s.roleMu.Unlock()
	if s.roleChanged == nil {
		return
	}
	close(s.roleChanged)
	s.roleChanged = make(chan struct{})
}

func (s *Server) currentRole() Role {
	return Role(s.role.Load())
}

func (s *Server) isLeader() bool {
	return s.currentRole() == RoleLeader
}

func (s *Server) isCandidate() bool {
	return s.currentRole() == RoleCandidate
}

func (s *Server) isFollower() bool {
	return s.currentRole() == RoleFollower
}

func (s *Server) becomePreCandidate() bool {
	cur := s.currentRole()
	to := RolePreCandidate
	if !validTransition(cur, to) {
		s.log().Warn("invalid transition to pre-candidate", "from", cur)
		return false
	}

	if !s.role.CompareAndSwap(uint32(cur), uint32(RolePreCandidate)) {
		s.log().Warn("CAS failed for pre-candidate transition", "from", cur)
		return false
	}
	// No votedFor or term change — PreVote is a dry run.
	// The self-vote is counted implicitly by grantedCnt in runElection.

	if cur != RolePreCandidate {
		s.notifyRoleChanged()
	}

	s.log().Info("became pre-candidate", "term", s.term.Load())
	s.tickPreElection()

	return true
}

func (s *Server) becomeCandidate() bool {
	cur := s.currentRole()
	to := RoleCandidate
	if !validTransition(cur, to) {
		s.log().Warn("invalid transition to candidate", "from", cur)
		return false
	}

	if !s.role.CompareAndSwap(uint32(cur), uint32(RoleCandidate)) {
		s.log().Warn("CAS failed for candidate transition", "from", cur)
		return false
	}
	s.roleMu.Lock()
	s.term.Add(1)
	s.votedFor = s.nodeID // vote itself
	err := s.storeState()
	s.roleMu.Unlock()
	if err != nil {
		s.log().Error("failed to persist state after becoming candidate")
		return false
	}
	if cur != RoleCandidate {
		s.notifyRoleChanged()
	}

	s.log().Info("became candidate", "term", s.term.Load())
	s.tickElection()

	return true
}

func (s *Server) becomeLeader() bool {
	to := RoleLeader
	if !validTransition(s.currentRole(), to) {
		s.log().Warn("invalid transition to leader", "from", s.currentRole())
		return false
	}
	s.role.Store(uint32(to))
	s.notifyRoleChanged()
	if err := s.promote(); err != nil {
		s.log().Error("promote failed after becoming leader", "error", err)
		return false
	}
	s.log().Info("became leader", "term", s.term.Load())
	return true
}

func (s *Server) becomeFollower() bool {
	stepped := s.setRoleFollower()
	if stepped {
		s.notifyRoleChanged()
	}
	return true
}

// setRoleFollower atomically sets the role to follower if it isn't one already.
// Returns true when the role actually changed (caller should notify).
func (s *Server) setRoleFollower() bool {
	cur := s.currentRole()
	if cur == RoleFollower {
		return false
	}
	s.role.Store(uint32(RoleFollower))
	s.log().Info("became follower", "term", s.term.Load())
	return true
}

// electionRound captures the differences between a real election and a pre-election.
type electionRound struct {
	payload    []byte          // encoded request (VoteRequest or PreVoteRequest)
	phase      string          // "election" or "pre-election" (for log messages)
	respStatus protocol.Status // expected response status
	onWin      func() bool     // action on majority: becomeLeader or becomeCandidate
}

func (s *Server) runElection(round electionRound) {
	timeout := randomElectionTimeout()
	peerIds := s.peerManager.NodeIDs()
	quorum := utils.ComputeQuorum(len(peerIds) + 1)

	// A node with no peers can self-elect (quorum=1), which is correct for a
	// genuine single-node cluster. But a partitioned replica that lost its peer
	// list must not self-promote — its ReplicaOf config proves it's part of a
	// multi-node cluster. Abort the election so it waits for reconnection.
	if len(peerIds) == 0 && s.opts.ReplicaOf != "" {
		s.log().Info(round.phase+" skipped: no peers and replica-of is set", "replicaOf", s.opts.ReplicaOf)
		return
	}

	var grantedCnt atomic.Int32
	grantedCnt.Add(1)

	var aborted atomic.Bool

	s.log().Info(round.phase+" started", "term", s.term.Load(), "peers", len(peerIds), "quorum", quorum, "timeout", timeout)

	var wg sync.WaitGroup
	for _, pid := range peerIds {
		wg.Go(func() {
			respTerm, granted, err := s.requestVote(pid, round.payload, timeout, round.respStatus)
			if err != nil {
				s.log().Debug(round.phase+" request failed", "peer", pid, "error", err)
				return
			}

			if granted {
				s.log().Debug(round.phase+" vote granted", "peer", pid)
				grantedCnt.Add(1)
			} else {
				s.log().Debug(round.phase+" vote denied", "peer", pid, "respTerm", respTerm)
				if respTerm > s.term.Load() {
					s.log().Info("discovered higher term, stepping down", "respTerm", respTerm, "myTerm", s.term.Load())
					s.roleMu.Lock()
					s.term.Store(respTerm)
					s.votedFor = ""
					if err := s.storeState(); err != nil {
						s.log().Error("failed to persist state after step-down", "error", err)
					}
					s.roleMu.Unlock()

					s.becomeFollower()
					s.lastHeartbeat = time.Now()

					if pi, ok := s.peerManager.Get(pid); ok {
						go func() {
							if err := s.relocate(pi.Addr); err != nil {
								s.log().Warn("post-stepdown relocate failed", "addr", pi.Addr, "error", err)
							}
						}()
					}

					aborted.Store(true)
				}
			}
		})
	}

	wg.Wait()

	if aborted.Load() {
		s.log().Info(round.phase + " aborted, stepped down to follower")
		return
	}

	s.log().Info(round.phase+" result", "granted", grantedCnt.Load(), "quorum", quorum)

	if grantedCnt.Load() >= int32(quorum) {
		if !round.onWin() {
			s.log().Warn(round.phase + " won but promotion failed, backing off")
			s.lastHeartbeat = time.Now()
			return
		}
	} else {
		s.log().Info(round.phase + " lost, not enough votes")
	}
}

func (s *Server) tickElection() {
	req := s.buildVoteRequest()
	payload, _ := protocol.EncodeRequest(req)
	s.runElection(electionRound{
		payload:    payload,
		phase:      "election",
		respStatus: protocol.StatusVoteResponse,
		onWin:      s.becomeLeader,
	})
}

func (s *Server) tickPreElection() {
	req := protocol.NewPreVoteRequest(s.term.Load()+1, s.nodeID, s.lastSeq.Load())
	payload, _ := protocol.EncodeRequest(req)
	s.runElection(electionRound{
		payload:    payload,
		phase:      "pre-election",
		respStatus: protocol.StatusPreVoteResponse,
		onWin:      s.becomeCandidate,
	})
}

// requestVote sends a VoteRequest to a single peer and parses the response.
// Returns the responder's term, whether the vote was granted, and any error.
func (s *Server) requestVote(peerID string, payload []byte, timeout time.Duration, expectedStatus protocol.Status) (uint64, bool, error) {
	t, err := s.peerManager.GetTransport(peerID)
	if err != nil {
		return 0, false, fmt.Errorf("get peer %s: %w", peerID, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	respPayload, err := t.Request(ctx, payload)
	if err != nil {
		return 0, false, fmt.Errorf("request to %s: %w", peerID, err)
	}

	resp, err := protocol.DecodeResponse(respPayload)
	if err != nil {
		return 0, false, fmt.Errorf("decode response from %s: %w", peerID, err)
	}

	if resp.Status != expectedStatus {
		return 0, false, fmt.Errorf("unexpected response from %s: status=%d", peerID, resp.Status)
	}

	vr, err := protocol.ParseVoteResponseValue(resp.Value)
	if err != nil {
		return 0, false, fmt.Errorf("parse vote response from %s: %w", peerID, err)
	}

	return vr.Term, vr.Granted, nil
}

func (s *Server) buildVoteRequest() protocol.Request {
	return protocol.NewVoteRequest(s.term.Load(), s.nodeID, s.lastSeq.Load())
}

func (s *Server) buildVoteResponse(granted bool) protocol.Response {
	return protocol.NewVoteResponse(s.term.Load(), granted)
}

func (s *Server) buildPreVoteResponse(granted bool) protocol.Response {
	return protocol.NewPreVoteResponse(s.term.Load(), granted)
}
