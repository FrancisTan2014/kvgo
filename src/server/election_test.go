package server

import (
	"fmt"
	"kvgo/protocol"
	"testing"
)

func TestValidTransition(t *testing.T) {
	tests := []struct {
		from Role
		to   Role
		want bool
	}{
		// Legal transitions
		{RoleFollower, RoleFollower, true},   // no-op (step-down when already follower)
		{RoleFollower, RoleCandidate, true},  // election timeout
		{RoleCandidate, RoleCandidate, true}, // retry with new term
		{RoleCandidate, RoleLeader, true},    // won election
		{RoleCandidate, RoleFollower, true},  // discovered higher term
		{RoleLeader, RoleFollower, true},     // discovered higher term

		// Illegal transitions
		{RoleFollower, RoleLeader, false},  // can't skip candidate
		{RoleLeader, RoleCandidate, false}, // leader doesn't campaign
		{RoleLeader, RoleLeader, false},    // already leader
	}

	for _, tt := range tests {
		name := fmt.Sprintf("%s→%s", tt.from, tt.to)
		t.Run(name, func(t *testing.T) {
			got := validTransition(tt.from, tt.to)
			if got != tt.want {
				t.Errorf("validTransition(%s, %s) = %v, want %v", tt.from, tt.to, got, tt.want)
			}
		})
	}
}

func TestRoleString(t *testing.T) {
	tests := []struct {
		role Role
		want string
	}{
		{RoleFollower, "follower"},
		{RoleCandidate, "candidate"},
		{RoleLeader, "leader"},
		{Role(99), "unknown"},
	}
	for _, tt := range tests {
		if got := tt.role.String(); got != tt.want {
			t.Errorf("Role(%d).String() = %q, want %q", tt.role, got, tt.want)
		}
	}
}

func TestBecomeCandidate_IllegalTransitions(t *testing.T) {
	// Leader cannot become candidate
	s := &Server{}
	s.role.Store(uint32(RoleLeader))
	if s.becomeCandidate() {
		t.Error("becomeCandidate() from Leader should return false")
	}
}

func TestBecomeLeader_IllegalTransitions(t *testing.T) {
	tests := []struct {
		name string
		from Role
	}{
		{"from follower", RoleFollower},
		{"from leader", RoleLeader},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &Server{}
			s.role.Store(uint32(tt.from))
			if s.becomeLeader() {
				t.Errorf("becomeLeader() from %s should return false", tt.from)
			}
		})
	}
}

func TestBecomeFollower_LegalTransitions(t *testing.T) {
	tests := []struct {
		name string
		from Role
	}{
		{"from candidate", RoleCandidate},
		{"from leader", RoleLeader},
		{"from follower (no-op)", RoleFollower},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &Server{}
			s.role.Store(uint32(tt.from))
			if !s.becomeFollower() {
				t.Errorf("becomeFollower() from %s should return true", tt.from)
			}
			if s.currentRole() != RoleFollower {
				t.Errorf("role after becomeFollower() = %s, want follower", s.currentRole())
			}
		})
	}
}

func TestParseVoteRequest(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    protocol.VoteRequestValue
		wantErr bool
	}{
		{
			name:  "valid request",
			input: "5\nnode-abc\n100",
			want:  protocol.VoteRequestValue{Term: 5, NodeID: "node-abc", LastSeq: 100},
		},
		{
			name:  "term zero",
			input: "0\nnode-1\n0",
			want:  protocol.VoteRequestValue{Term: 0, NodeID: "node-1", LastSeq: 0},
		},
		{
			name:    "too few fields",
			input:   "5\nnode-abc",
			wantErr: true,
		},
		{
			name:    "too many fields",
			input:   "5\nnode-abc\n100\nextra",
			wantErr: true,
		},
		{
			name:    "invalid term",
			input:   "abc\nnode-1\n100",
			wantErr: true,
		},
		{
			name:    "invalid lastSeq",
			input:   "5\nnode-1\nabc",
			wantErr: true,
		},
		{
			name:    "empty input",
			input:   "",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := protocol.ParseVoteRequestValue([]byte(tt.input))
			if (err != nil) != tt.wantErr {
				t.Fatalf("ParseVoteRequestValue() error = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr {
				if got.Term != tt.want.Term || got.NodeID != tt.want.NodeID || got.LastSeq != tt.want.LastSeq {
					t.Errorf("ParseVoteRequestValue() = %+v, want %+v", got, tt.want)
				}
			}
		})
	}
}

func TestBuildAndParseVoteRequest_RoundTrip(t *testing.T) {
	s := &Server{nodeID: "node-42"}
	s.term.Store(7)
	s.lastSeq.Store(999)

	req := s.buildVoteRequest()

	if req.Cmd != protocol.CmdVoteRequest {
		t.Fatalf("cmd = %d, want CmdVoteRequest(%d)", req.Cmd, protocol.CmdVoteRequest)
	}

	vr, err := protocol.ParseVoteRequestValue(req.Value)
	if err != nil {
		t.Fatalf("ParseVoteRequestValue() error: %v", err)
	}
	if vr.Term != 7 {
		t.Errorf("term = %d, want 7", vr.Term)
	}
	if vr.NodeID != "node-42" {
		t.Errorf("nodeID = %q, want %q", vr.NodeID, "node-42")
	}
	if vr.LastSeq != 999 {
		t.Errorf("lastSeq = %d, want 999", vr.LastSeq)
	}
}

func TestBuildVoteResponse_Encoding(t *testing.T) {
	tests := []struct {
		name    string
		term    uint64
		granted bool
	}{
		{"granted", 5, true},
		{"denied", 10, false},
		{"term zero granted", 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &Server{}
			s.term.Store(tt.term)
			resp := s.buildVoteResponse(tt.granted)

			if resp.Status != protocol.StatusVoteResponse {
				t.Fatalf("status = %d, want StatusVoteResponse", resp.Status)
			}
			if len(resp.Value) != 9 {
				t.Fatalf("value len = %d, want 9", len(resp.Value))
			}

			vr, err := protocol.ParseVoteResponseValue(resp.Value)
			if err != nil {
				t.Fatalf("ParseVoteResponseValue() error: %v", err)
			}

			if vr.Term != tt.term {
				t.Errorf("term = %d, want %d", vr.Term, tt.term)
			}

			if vr.Granted != tt.granted {
				t.Errorf("granted = %v, want %v", vr.Granted, tt.granted)
			}
		})
	}
}

func TestVoteResponse_RoundTrip(t *testing.T) {
	// Encode a vote response, then decode it the same way requestVote does.
	s := &Server{}
	s.term.Store(42)
	resp := s.buildVoteResponse(true)

	payload, err := protocol.EncodeResponse(resp)
	if err != nil {
		t.Fatalf("EncodeResponse() error: %v", err)
	}

	decoded, err := protocol.DecodeResponse(payload)
	if err != nil {
		t.Fatalf("DecodeResponse() error: %v", err)
	}

	if decoded.Status != protocol.StatusVoteResponse {
		t.Fatalf("status = %d, want StatusVoteResponse", decoded.Status)
	}

	vr, vrErr := protocol.ParseVoteResponseValue(decoded.Value)
	if vrErr != nil {
		t.Fatalf("ParseVoteResponseValue() error: %v", vrErr)
	}
	if vr.Term != 42 {
		t.Errorf("round-trip term = %d, want 42", vr.Term)
	}
	if !vr.Granted {
		t.Errorf("round-trip granted = false, want true")
	}
}

func TestHandleVoteRequest(t *testing.T) {
	tests := []struct {
		name        string
		myTerm      uint64
		myLastSeq   uint64
		myVotedFor  string
		myRole      Role
		reqTerm     uint64
		reqNodeID   string
		reqLastSeq  uint64
		wantGranted bool
		wantTerm    uint64 // expected term after handling
		wantRole    Role   // expected role after handling
	}{
		{
			name:        "grant vote: same term, not yet voted",
			myTerm:      5,
			myLastSeq:   100,
			myVotedFor:  "",
			myRole:      RoleFollower,
			reqTerm:     5,
			reqNodeID:   "candidate-1",
			reqLastSeq:  100,
			wantGranted: true,
			wantTerm:    5,
			wantRole:    RoleFollower,
		},
		{
			name:        "grant vote: higher term from candidate",
			myTerm:      3,
			myLastSeq:   50,
			myVotedFor:  "old-candidate",
			myRole:      RoleFollower,
			reqTerm:     5,
			reqNodeID:   "candidate-1",
			reqLastSeq:  50,
			wantGranted: true,
			wantTerm:    5,
			wantRole:    RoleFollower,
		},
		{
			name:        "grant vote: candidate more up-to-date",
			myTerm:      5,
			myLastSeq:   50,
			myVotedFor:  "",
			myRole:      RoleFollower,
			reqTerm:     5,
			reqNodeID:   "candidate-1",
			reqLastSeq:  100,
			wantGranted: true,
			wantTerm:    5,
			wantRole:    RoleFollower,
		},
		{
			name:        "grant vote: re-vote for same candidate",
			myTerm:      5,
			myLastSeq:   100,
			myVotedFor:  "candidate-1",
			myRole:      RoleFollower,
			reqTerm:     5,
			reqNodeID:   "candidate-1",
			reqLastSeq:  100,
			wantGranted: true,
			wantTerm:    5,
			wantRole:    RoleFollower,
		},
		{
			name:        "reject: stale term",
			myTerm:      10,
			myLastSeq:   100,
			myVotedFor:  "",
			myRole:      RoleFollower,
			reqTerm:     5,
			reqNodeID:   "candidate-1",
			reqLastSeq:  100,
			wantGranted: false,
			wantTerm:    10,
			wantRole:    RoleFollower,
		},
		{
			name:        "reject: already voted for different candidate",
			myTerm:      5,
			myLastSeq:   100,
			myVotedFor:  "candidate-2",
			myRole:      RoleFollower,
			reqTerm:     5,
			reqNodeID:   "candidate-1",
			reqLastSeq:  100,
			wantGranted: false,
			wantTerm:    5,
			wantRole:    RoleFollower,
		},
		{
			name:        "reject: candidate log behind",
			myTerm:      5,
			myLastSeq:   200,
			myVotedFor:  "",
			myRole:      RoleFollower,
			reqTerm:     5,
			reqNodeID:   "candidate-1",
			reqLastSeq:  100,
			wantGranted: false,
			wantTerm:    5,
			wantRole:    RoleFollower,
		},
		{
			name:        "higher term causes leader step-down",
			myTerm:      3,
			myLastSeq:   50,
			myVotedFor:  "",
			myRole:      RoleLeader,
			reqTerm:     5,
			reqNodeID:   "candidate-1",
			reqLastSeq:  50,
			wantGranted: true,
			wantTerm:    5,
			wantRole:    RoleFollower,
		},
		{
			name:        "higher term causes candidate step-down",
			myTerm:      3,
			myLastSeq:   50,
			myVotedFor:  "myself",
			myRole:      RoleCandidate,
			reqTerm:     5,
			reqNodeID:   "candidate-1",
			reqLastSeq:  50,
			wantGranted: true,
			wantTerm:    5,
			wantRole:    RoleFollower,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &Server{
				votedFor: tt.myVotedFor,
				opts:     Options{DataDir: t.TempDir()},
			}
			defer func() {
				if s.metaFile != nil {
					s.metaFile.Close()
				}
			}()
			s.term.Store(tt.myTerm)
			s.lastSeq.Store(tt.myLastSeq)
			s.role.Store(uint32(tt.myRole))

			// Build request value
			reqValue := fmt.Sprintf("%d\n%s\n%d", tt.reqTerm, tt.reqNodeID, tt.reqLastSeq)
			vr, err := protocol.ParseVoteRequestValue([]byte(reqValue))
			if err != nil {
				t.Fatalf("ParseVoteRequestValue: %v", err)
			}

			// Call the actual production logic under the same lock contract.
			s.roleMu.Lock()
			resp, _ := s.evaluateVoteLocked(vr)
			s.roleMu.Unlock()

			// Decode the response to check granted/term.
			vresp, vrErr := protocol.ParseVoteResponseValue(resp.Value)
			if vrErr != nil {
				t.Fatalf("ParseVoteResponseValue: %v", vrErr)
			}

			if vresp.Granted != tt.wantGranted {
				t.Errorf("granted = %v, want %v", vresp.Granted, tt.wantGranted)
			}
			if s.term.Load() != tt.wantTerm {
				t.Errorf("term after = %d, want %d", s.term.Load(), tt.wantTerm)
			}
			if s.currentRole() != tt.wantRole {
				t.Errorf("role after = %s, want %s", s.currentRole(), tt.wantRole)
			}
		})
	}
}

func TestRandomElectionTimeout(t *testing.T) {
	min := electionTimeout
	max := 2 * electionTimeout

	for range 100 {
		d := randomElectionTimeout()
		if d < min || d >= max {
			t.Fatalf("randomElectionTimeout() = %v, want [%v, %v)", d, min, max)
		}
	}
}
