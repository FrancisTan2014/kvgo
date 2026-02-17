// election.go — role state machine for leader election.
// Roles, valid transitions, and becomeX() methods live here.

package server

import (
	"encoding/binary"
	"fmt"
	"kvgo/protocol"
	"kvgo/utils"
	"math/rand/v2"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const electionTimeout = 10 * heartbeatInterval

// randomElectionTimeout returns a randomized duration in [electionTimeout, 2*electionTimeout).
// Jitter prevents multiple followers from timing out simultaneously (split vote).
func randomElectionTimeout() time.Duration {
	return electionTimeout + time.Duration(rand.Int64N(int64(electionTimeout)))
}

// Role represents a node's current role in the cluster.
type Role uint32

const (
	RoleFollower Role = iota // zero value = follower (safe default)
	RoleCandidate
	RoleLeader
)

func (r Role) String() string {
	switch r {
	case RoleFollower:
		return "follower"
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
	case from == RoleFollower && to == RoleCandidate:
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
	cur := s.currentRole()
	to := RoleFollower
	if !validTransition(cur, to) {
		s.log().Warn("invalid transition to follower", "from", cur)
		return false
	}
	s.role.Store(uint32(to))
	if cur != RoleFollower {
		s.notifyRoleChanged()
	}
	s.log().Info("became follower", "term", s.term.Load())
	return true
}

func (s *Server) tickElection() {
	req := s.buildVoteRequest()
	payload, _ := protocol.EncodeRequest(req)

	timeout := randomElectionTimeout()
	peerIds := s.peerManager.NodeIDs()
	quorum := utils.ComputeQuorum(len(peerIds) + 1)

	var grantedCnt atomic.Int32
	grantedCnt.Add(1)

	var aborted atomic.Bool // Higher term detected

	s.log().Info("election started", "term", s.term.Load(), "peers", len(peerIds), "quorum", quorum, "timeout", timeout)

	var wg sync.WaitGroup
	for _, pid := range peerIds {
		wg.Go(func() {
			respTerm, granted, err := s.requestVote(pid, payload, timeout)
			if err != nil {
				s.log().Debug("vote request failed", "peer", pid, "error", err)
				return
			}

			if granted {
				s.log().Debug("vote granted", "peer", pid)
				grantedCnt.Add(1)
			} else {
				s.log().Debug("vote denied", "peer", pid, "respTerm", respTerm)
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

					// After stepping down, reconnect via the peer that denied us.
					if addr, ok := s.peerManager.Addr(pid); ok {
						go func() {
							if err := s.relocate(addr); err != nil {
								s.log().Warn("post-stepdown relocate failed", "addr", addr, "error", err)
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
		s.log().Info("election aborted, stepped down to follower")
		return
	}

	s.log().Info("election result", "granted", grantedCnt.Load(), "quorum", quorum)

	if grantedCnt.Load() >= int32(quorum) {
		if !s.becomeLeader() {
			s.log().Warn("won election but becomeLeader failed, backing off")
			s.lastHeartbeat = time.Now() // back off before retrying
			return
		}
	} else {
		s.log().Info("election lost, not enough votes")
	}
}

// requestVote sends a VoteRequest to a single peer and parses the response.
// Returns the responder's term, whether the vote was granted, and any error.
func (s *Server) requestVote(peerID string, payload []byte, timeout time.Duration) (uint64, bool, error) {
	t, err := s.peerManager.Get(peerID)
	if err != nil {
		return 0, false, fmt.Errorf("get peer %s: %w", peerID, err)
	}

	respPayload, err := t.Request(payload, timeout)
	if err != nil {
		return 0, false, fmt.Errorf("request to %s: %w", peerID, err)
	}

	resp, err := protocol.DecodeResponse(respPayload)
	if err != nil {
		return 0, false, fmt.Errorf("decode response from %s: %w", peerID, err)
	}

	if resp.Status != protocol.StatusVoteResponse || len(resp.Value) != 9 {
		return 0, false, fmt.Errorf("unexpected response from %s: status=%d len=%d", peerID, resp.Status, len(resp.Value))
	}

	respTerm := binary.LittleEndian.Uint64(resp.Value[0:8])
	granted := resp.Value[8] == 1
	return respTerm, granted, nil
}

func (s *Server) buildVoteRequest() protocol.Request {
	payload := fmt.Sprintf("%d%s%s%s%d",
		s.term.Load(),
		protocol.Delimiter,
		s.nodeID,
		protocol.Delimiter,
		s.lastSeq.Load())
	return protocol.Request{
		Cmd:   protocol.CmdVoteRequest,
		Value: []byte(payload),
	}
}

type voteRequest struct {
	term    uint64
	nodeID  string
	lastSeq uint64
}

func parseVoteRequest(value []byte) (voteRequest, error) {
	parts := strings.Split(string(value), protocol.Delimiter)
	if len(parts) != 3 {
		return voteRequest{}, fmt.Errorf("expected 3 fields, got %d", len(parts))
	}

	term, err := strconv.ParseUint(parts[0], 10, 64)
	if err != nil {
		return voteRequest{}, fmt.Errorf("invalid term: %w", err)
	}

	lastSeq, err := strconv.ParseUint(parts[2], 10, 64)
	if err != nil {
		return voteRequest{}, fmt.Errorf("invalid lastSeq: %w", err)
	}

	return voteRequest{term: term, nodeID: parts[1], lastSeq: lastSeq}, nil
}

func (s *Server) buildVoteResponse(granted bool) protocol.Response {
	// [term u64 LE][granted u8]
	buf := make([]byte, 9)
	binary.LittleEndian.PutUint64(buf, s.term.Load())
	if granted {
		buf[8] = 1
	}
	return protocol.Response{
		Status: protocol.StatusVoteResponse,
		Value:  buf,
	}
}
