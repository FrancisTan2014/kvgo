package protocol

import (
	"testing"
)

func TestDiscoveryRequest_RoundTrip(t *testing.T) {
	tests := []struct {
		name   string
		term   uint64
		nodeID string
	}{
		{name: "typical", term: 5, nodeID: "abc123"},
		{name: "term zero", term: 0, nodeID: "node-0"},
		{name: "large term", term: 1<<63 - 1, nodeID: "x"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := NewDiscoveryRequest(tt.term, tt.nodeID)

			if req.Cmd != CmdDiscovery {
				t.Errorf("Cmd = %d, want %d", req.Cmd, CmdDiscovery)
			}

			rv, err := ParseDiscoveryRequestValue(req.Value)
			if err != nil {
				t.Fatalf("ParseDiscoveryRequestValue: %v", err)
			}
			if rv.Term != tt.term {
				t.Errorf("Term = %d, want %d", rv.Term, tt.term)
			}
			if rv.NodeID != tt.nodeID {
				t.Errorf("NodeID = %q, want %q", rv.NodeID, tt.nodeID)
			}
		})
	}
}

func TestDiscoveryRequest_ParseErrors(t *testing.T) {
	tests := []struct {
		name  string
		value []byte
	}{
		{name: "empty", value: []byte{}},
		{name: "single field", value: []byte("42")},
		{name: "too many fields", value: []byte("1\ntwo\nthree")},
		{name: "non-numeric term", value: []byte("abc\nnode1")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseDiscoveryRequestValue(tt.value)
			if err == nil {
				t.Error("expected error, got nil")
			}
		})
	}
}

func TestDiscoveryResponse_RoundTrip(t *testing.T) {
	tests := []struct {
		name       string
		term       uint64
		leaderId   string
		leaderAddr string
	}{
		{name: "typical", term: 3, leaderId: "leader-1", leaderAddr: "10.0.0.1:4050"},
		{name: "term zero", term: 0, leaderId: "id", leaderAddr: "host:1"},
		{name: "ipv6 addr", term: 99, leaderId: "n1", leaderAddr: "[::1]:5000"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := NewDiscoveryResponse(tt.term, tt.leaderId, tt.leaderAddr)

			if resp.Status != StatusDiscoveryResponse {
				t.Errorf("Status = %d, want %d", resp.Status, StatusDiscoveryResponse)
			}

			rv, err := ParseDiscoveryResponseValue(resp.Value)
			if err != nil {
				t.Fatalf("ParseDiscoveryResponseValue: %v", err)
			}
			if rv.Term != tt.term {
				t.Errorf("Term = %d, want %d", rv.Term, tt.term)
			}
			if rv.LeaderId != tt.leaderId {
				t.Errorf("LeaderId = %q, want %q", rv.LeaderId, tt.leaderId)
			}
			if rv.LeaderAddr != tt.leaderAddr {
				t.Errorf("LeaderAddr = %q, want %q", rv.LeaderAddr, tt.leaderAddr)
			}
		})
	}
}

func TestDiscoveryResponse_ParseErrors(t *testing.T) {
	tests := []struct {
		name  string
		value []byte
	}{
		{name: "empty", value: []byte{}},
		{name: "one field", value: []byte("42")},
		{name: "two fields", value: []byte("42\nleader")},
		{name: "four fields", value: []byte("42\na\nb\nc")},
		{name: "non-numeric term", value: []byte("abc\nleader\naddr")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseDiscoveryResponseValue(tt.value)
			if err == nil {
				t.Error("expected error, got nil")
			}
		})
	}
}

func TestDiscoveryResponse_EncodeValidation(t *testing.T) {
	// StatusDiscoveryResponse should be allowed to carry a value
	resp := NewDiscoveryResponse(1, "leader", "addr:1234")
	_, err := EncodeResponse(resp)
	if err != nil {
		t.Errorf("EncodeResponse rejected StatusDiscoveryResponse with value: %v", err)
	}
}
