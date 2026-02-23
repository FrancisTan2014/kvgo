package server

import (
	"errors"
	"kvgo/protocol"
	"kvgo/transport"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// handleDiscovery
// ---------------------------------------------------------------------------

func TestHandleDiscovery_LeaderReturnsSelf(t *testing.T) {
	s := &Server{
		nodeID: "leader-abc",
		opts:   Options{Port: 4050},
	}
	s.role.Store(uint32(RoleLeader))
	s.term.Store(5)

	req := protocol.NewDiscoveryRequest(3, "requester-xyz")
	mock := &mockStreamTransport{}
	ctx := &RequestContext{
		StreamTransport: mock,
		Request:         req,
	}

	if err := s.handleDiscovery(ctx); err != nil {
		t.Fatalf("handleDiscovery: %v", err)
	}

	resp, err := protocol.DecodeResponse(mock.written)
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	if resp.Status != protocol.StatusDiscoveryResponse {
		t.Fatalf("Status = %d, want %d", resp.Status, protocol.StatusDiscoveryResponse)
	}

	rv, err := protocol.ParseDiscoveryResponseValue(resp.Value)
	if err != nil {
		t.Fatalf("ParseDiscoveryResponseValue: %v", err)
	}
	if rv.LeaderId != "leader-abc" {
		t.Errorf("LeaderId = %q, want %q", rv.LeaderId, "leader-abc")
	}
	if rv.LeaderAddr != "127.0.0.1:4050" {
		t.Errorf("LeaderAddr = %q, want %q", rv.LeaderAddr, "127.0.0.1:4050")
	}
	if rv.Term != 5 {
		t.Errorf("Term = %d, want 5", rv.Term)
	}
}

func TestHandleDiscovery_FollowerForwardsKnownLeader(t *testing.T) {
	pm := NewPeerManager(nil, noopLogger)
	pm.MergePeers([]PeerInfo{
		{NodeID: "primary-1", Addr: "10.0.0.1:4050"},
	})

	s := &Server{
		nodeID:        "follower-1",
		primaryNodeID: "primary-1",
		peerManager:   pm,
		opts:          Options{Port: 4051},
	}
	s.role.Store(uint32(RoleFollower))
	s.term.Store(3)

	req := protocol.NewDiscoveryRequest(2, "requester-2")
	mock := &mockStreamTransport{}
	ctx := &RequestContext{
		StreamTransport: mock,
		Request:         req,
	}

	if err := s.handleDiscovery(ctx); err != nil {
		t.Fatalf("handleDiscovery: %v", err)
	}

	resp, err := protocol.DecodeResponse(mock.written)
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	if resp.Status != protocol.StatusDiscoveryResponse {
		t.Fatalf("Status = %d, want %d", resp.Status, protocol.StatusDiscoveryResponse)
	}

	rv, err := protocol.ParseDiscoveryResponseValue(resp.Value)
	if err != nil {
		t.Fatalf("ParseDiscoveryResponseValue: %v", err)
	}
	if rv.LeaderId != "primary-1" {
		t.Errorf("LeaderId = %q, want %q", rv.LeaderId, "primary-1")
	}
	if rv.LeaderAddr != "10.0.0.1:4050" {
		t.Errorf("LeaderAddr = %q, want %q", rv.LeaderAddr, "10.0.0.1:4050")
	}
}

func TestHandleDiscovery_FollowerUnknownLeader(t *testing.T) {
	// primaryNodeID set but not in peerManager — should still respond (empty response)
	pm := NewPeerManager(nil, noopLogger)

	s := &Server{
		nodeID:        "follower-1",
		primaryNodeID: "vanished-leader",
		peerManager:   pm,
		opts:          Options{Port: 4051},
	}
	s.role.Store(uint32(RoleFollower))
	s.term.Store(2)

	req := protocol.NewDiscoveryRequest(1, "requester-3")
	mock := &mockStreamTransport{}
	ctx := &RequestContext{
		StreamTransport: mock,
		Request:         req,
	}

	if err := s.handleDiscovery(ctx); err != nil {
		t.Fatalf("handleDiscovery: %v", err)
	}

	// Should still write a response (zero-value Response)
	if mock.written == nil {
		t.Fatal("expected response to be written")
	}
}

func TestHandleDiscovery_FollowerNoPrimary(t *testing.T) {
	// Follower with no primaryNodeID — e.g. just booted as follower, hasn't received PING yet
	pm := NewPeerManager(nil, noopLogger)

	s := &Server{
		nodeID:        "follower-2",
		primaryNodeID: "",
		peerManager:   pm,
		opts:          Options{Port: 4052},
	}
	s.role.Store(uint32(RoleFollower))
	s.term.Store(1)

	req := protocol.NewDiscoveryRequest(1, "requester-4")
	mock := &mockStreamTransport{}
	ctx := &RequestContext{
		StreamTransport: mock,
		Request:         req,
	}

	if err := s.handleDiscovery(ctx); err != nil {
		t.Fatalf("handleDiscovery: %v", err)
	}

	if mock.written == nil {
		t.Fatal("expected response to be written")
	}
}

func TestHandleDiscovery_MalformedRequest(t *testing.T) {
	s := &Server{
		opts: Options{Port: 4050},
	}
	s.role.Store(uint32(RoleLeader))

	mock := &mockStreamTransport{}
	ctx := &RequestContext{
		StreamTransport: mock,
		Request:         protocol.Request{Cmd: protocol.CmdDiscovery, Value: []byte("garbage")},
	}

	err := s.handleDiscovery(ctx)
	if err == nil {
		t.Error("expected error for malformed request, got nil")
	}
}

// ---------------------------------------------------------------------------
// updateReplicaConn — heartbeat ticker refresh
// ---------------------------------------------------------------------------

func TestUpdateReplicaConn_RefreshesTicker(t *testing.T) {
	oldTransport := &fakeStreamTransport{id: 1}
	rc := newReplicaConn(oldTransport, 10, "old-replid", "node-1", "10.0.0.1:5000")

	// Stop the original ticker to simulate what serveReplicaWriter's defer does
	rc.hb.Stop()

	// Drain any pending tick
	select {
	case <-rc.hb.C:
	default:
	}

	newTransport := &fakeStreamTransport{id: 2}
	rv := &protocol.ReplicateValue{
		Replid:     "new-replid",
		ListenAddr: "10.0.0.2:5000",
		NodeID:     "node-1",
	}

	s := &Server{}
	s.updateReplicaConn(rc, &RequestContext{
		StreamTransport: newTransport,
		Request:         protocol.Request{Seq: 42},
	}, rv)

	// The new ticker should fire within heartbeatInterval + small buffer
	select {
	case <-rc.hb.C:
		// success — ticker is alive
	case <-time.After(heartbeatInterval + 100*time.Millisecond):
		t.Fatal("new ticker did not fire — heartbeat ticker not refreshed on reconnect")
	}

	// Verify other fields updated
	if rc.transport != newTransport {
		t.Error("transport not updated")
	}
	if rc.listenAddr != "10.0.0.2:5000" {
		t.Errorf("listenAddr = %q, want 10.0.0.2:5000", rc.listenAddr)
	}
	if rc.lastSeq != 42 {
		t.Errorf("lastSeq = %d, want 42", rc.lastSeq)
	}
	if rc.lastReplid != "new-replid" {
		t.Errorf("lastReplid = %q, want new-replid", rc.lastReplid)
	}
}

func TestUpdateReplicaConn_DrainsSendCh(t *testing.T) {
	rc := newReplicaConn(&fakeStreamTransport{id: 1}, 0, "", "node-1", "addr:1")

	// Queue a write into old channel
	rc.sendCh <- []byte("stale-write")

	rv := &protocol.ReplicateValue{Replid: "r", ListenAddr: "addr:2", NodeID: "node-1"}
	s := &Server{}
	s.updateReplicaConn(rc, &RequestContext{
		StreamTransport: &fakeStreamTransport{id: 2},
		Request:         protocol.Request{Seq: 1},
	}, rv)

	// New channel should be empty
	select {
	case <-rc.sendCh:
		t.Error("new sendCh should be empty, got data")
	default:
		// good
	}

	if cap(rc.sendCh) != replicaSendBuffer {
		t.Errorf("sendCh cap = %d, want %d", cap(rc.sendCh), replicaSendBuffer)
	}
}

// ---------------------------------------------------------------------------
// discoverCluster
// ---------------------------------------------------------------------------

func TestDiscoverCluster_FindsLeader(t *testing.T) {
	// Build a discovery response from a leader peer
	leaderResp := protocol.NewDiscoveryResponse(5, "leader-1", "10.0.0.1:4050")
	respPayload, _ := protocol.EncodeResponse(leaderResp)

	pm := NewPeerManager(func(addr string) (transport.RequestTransport, error) {
		return &mockRequestTransport{response: respPayload}, nil
	}, noopLogger)

	pm.MergePeers([]PeerInfo{
		{NodeID: "peer-a", Addr: "10.0.0.2:4050"},
	})

	s := &Server{
		nodeID:      "self",
		peerManager: pm,
		opts:        Options{DiscoveryTimeout: time.Second},
	}
	s.term.Store(3)

	leader, term, found := s.discoverCluster()
	if !found {
		t.Fatal("expected to find leader")
	}
	if leader.NodeID != "leader-1" {
		t.Errorf("NodeID = %q, want leader-1", leader.NodeID)
	}
	if leader.Addr != "10.0.0.1:4050" {
		t.Errorf("Addr = %q, want 10.0.0.1:4050", leader.Addr)
	}
	if term != 5 {
		t.Errorf("term = %d, want 5", term)
	}
}

func TestDiscoverCluster_NoPeers(t *testing.T) {
	pm := NewPeerManager(nil, noopLogger)

	s := &Server{
		nodeID:      "self",
		peerManager: pm,
		opts:        Options{DiscoveryTimeout: time.Second},
	}
	s.term.Store(1)

	_, _, found := s.discoverCluster()
	if found {
		t.Error("expected found=false with no peers")
	}
}

func TestDiscoverCluster_AllPeersUnreachable(t *testing.T) {
	pm := NewPeerManager(func(addr string) (transport.RequestTransport, error) {
		return nil, errors.New("connection refused")
	}, noopLogger)

	pm.MergePeers([]PeerInfo{
		{NodeID: "peer-a", Addr: "10.0.0.2:4050"},
		{NodeID: "peer-b", Addr: "10.0.0.3:4050"},
	})

	s := &Server{
		nodeID:      "self",
		peerManager: pm,
		opts:        Options{DiscoveryTimeout: time.Second},
	}
	s.term.Store(1)

	_, _, found := s.discoverCluster()
	if found {
		t.Error("expected found=false when all peers unreachable")
	}
}

func TestDiscoverCluster_PicksHighestTerm(t *testing.T) {
	// Two peers respond — one with term 3, one with term 7
	resp3 := protocol.NewDiscoveryResponse(3, "old-leader", "10.0.0.1:4050")
	payload3, _ := protocol.EncodeResponse(resp3)

	resp7 := protocol.NewDiscoveryResponse(7, "new-leader", "10.0.0.2:4050")
	payload7, _ := protocol.EncodeResponse(resp7)

	callCount := 0
	pm := NewPeerManager(func(addr string) (transport.RequestTransport, error) {
		callCount++
		if addr == "10.0.0.1:4050" {
			return &mockRequestTransport{response: payload3}, nil
		}
		return &mockRequestTransport{response: payload7}, nil
	}, noopLogger)

	pm.MergePeers([]PeerInfo{
		{NodeID: "peer-a", Addr: "10.0.0.1:4050"},
		{NodeID: "peer-b", Addr: "10.0.0.2:4050"},
	})

	s := &Server{
		nodeID:      "self",
		peerManager: pm,
		opts:        Options{DiscoveryTimeout: time.Second},
	}
	s.term.Store(1)

	leader, term, found := s.discoverCluster()
	if !found {
		t.Fatal("expected to find leader")
	}
	if leader.NodeID != "new-leader" {
		t.Errorf("NodeID = %q, want new-leader", leader.NodeID)
	}
	if term != 7 {
		t.Errorf("term = %d, want 7", term)
	}
}

func TestDiscoverCluster_IgnoresBadStatus(t *testing.T) {
	// Peer responds with StatusOK instead of StatusDiscoveryResponse
	badResp := protocol.Response{Status: protocol.StatusOK, Value: []byte("whatever")}
	payload, _ := protocol.EncodeResponse(badResp)

	pm := NewPeerManager(func(addr string) (transport.RequestTransport, error) {
		return &mockRequestTransport{response: payload}, nil
	}, noopLogger)

	pm.MergePeers([]PeerInfo{
		{NodeID: "peer-a", Addr: "10.0.0.2:4050"},
	})

	s := &Server{
		nodeID:      "self",
		peerManager: pm,
		opts:        Options{DiscoveryTimeout: time.Second},
	}
	s.term.Store(1)

	_, _, found := s.discoverCluster()
	if found {
		t.Error("expected found=false when peer returns wrong status")
	}
}

// ---------------------------------------------------------------------------
// PeerManager.Any
// ---------------------------------------------------------------------------

func TestPeerManager_Any(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		pm := NewPeerManager(nil, noopLogger)
		if pm.Any() {
			t.Error("Any() = true on empty PeerManager")
		}
	})

	t.Run("with peers", func(t *testing.T) {
		pm := NewPeerManager(nil, noopLogger)
		pm.MergePeers([]PeerInfo{{NodeID: "n1", Addr: "a:1"}})
		if !pm.Any() {
			t.Error("Any() = false after MergePeers")
		}
	})
}
