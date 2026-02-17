package server

import (
	"kvgo/protocol"
	"kvgo/transport"
	"sort"
	"testing"
)

// ---------------------------------------------------------------------------
// Topology Tests
// ---------------------------------------------------------------------------

func TestHandleTopology(t *testing.T) {
	t.Run("stores peers excluding self", func(t *testing.T) {
		pm := NewPeerManager(nil, noopLogger)
		s := &Server{
			peerManager: pm,
			nodeID:      "self-node",
			opts:        Options{Host: "127.0.0.1", Port: 5000},
		}

		topology := "n1@10.0.0.1:5001" + protocol.Delimiter + "self-node@127.0.0.1:5000" + protocol.Delimiter + "n2@10.0.0.2:5002"

		ctx := &RequestContext{
			StreamTransport: &mockStreamTransport{},
			Request: protocol.Request{
				Cmd:   protocol.CmdTopology,
				Value: []byte(topology),
			},
		}

		err := s.handleTopology(ctx)
		if err != nil {
			t.Fatalf("handleTopology: %v", err)
		}

		ids := pm.NodeIDs()
		sort.Strings(ids)
		if len(ids) != 2 {
			t.Fatalf("len = %d, want 2", len(ids))
		}
		if ids[0] != "n1" || ids[1] != "n2" {
			t.Errorf("ids = %v, want [n1 n2]", ids)
		}
	})

	t.Run("malformed entries are skipped", func(t *testing.T) {
		pm := NewPeerManager(nil, noopLogger)
		s := &Server{peerManager: pm, nodeID: "self"}

		// Mix of valid, missing-@, empty-nodeID, empty-addr
		topology := "n1@10.0.0.1:5001" + protocol.Delimiter + "garbage-no-at" + protocol.Delimiter + "@empty-id" + protocol.Delimiter + "no-addr@" + protocol.Delimiter + "n2@10.0.0.2:5002"

		ctx := &RequestContext{
			StreamTransport: &mockStreamTransport{},
			Request:         protocol.Request{Cmd: protocol.CmdTopology, Value: []byte(topology)},
		}

		err := s.handleTopology(ctx)
		if err != nil {
			t.Fatalf("handleTopology: %v", err)
		}

		ids := pm.NodeIDs()
		sort.Strings(ids)
		if len(ids) != 2 {
			t.Fatalf("len = %d, want 2, got %v", len(ids), ids)
		}
		if ids[0] != "n1" || ids[1] != "n2" {
			t.Errorf("ids = %v, want [n1 n2]", ids)
		}
	})

	t.Run("empty topology clears peers", func(t *testing.T) {
		pm := NewPeerManager(nil, noopLogger)
		pm.SavePeers([]PeerInfo{{NodeID: "n1", Addr: "a:1"}, {NodeID: "n2", Addr: "b:2"}})

		s := &Server{peerManager: pm, nodeID: "self"}

		ctx := &RequestContext{
			StreamTransport: &mockStreamTransport{},
			Request:         protocol.Request{Cmd: protocol.CmdTopology},
		}

		err := s.handleTopology(ctx)
		if err != nil {
			t.Fatalf("handleTopology: %v", err)
		}

		if len(pm.NodeIDs()) != 0 {
			t.Errorf("peers not cleared, got %v", pm.NodeIDs())
		}
	})
}

func TestBuildTopologyRequest(t *testing.T) {
	s := &Server{
		nodeID: "primary-node",
		opts:   Options{Host: "10.0.0.100", Port: 4000},
	}

	t1 := &fakeStreamTransport{id: 1}
	t2 := &fakeStreamTransport{id: 2}

	replicas := map[transport.StreamTransport]*replicaConn{
		t1: {transport: t1, nodeID: "n1", listenAddr: "10.0.0.1:5001"},
		t2: {transport: t2, nodeID: "n2", listenAddr: "10.0.0.2:5002"},
	}

	req := s.buildTopologyRequest(replicas)
	if req.Cmd != protocol.CmdTopology {
		t.Errorf("cmd = %d, want CmdTopology", req.Cmd)
	}

	// Value contains nodeID@addr entries delimited by newline
	val := string(req.Value)
	wantEntries := []string{"primary-node@10.0.0.100:4000", "n1@10.0.0.1:5001", "n2@10.0.0.2:5002"}
	for _, want := range wantEntries {
		found := false
		for _, part := range splitTopology(val) {
			if part == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("topology %q missing %s", val, want)
		}
	}
}

func TestBroadcastTopology(t *testing.T) {
	t.Run("saves peers to primary peerManager", func(t *testing.T) {
		pm := NewPeerManager(nil, noopLogger)
		s := &Server{
			replicas:    make(map[transport.StreamTransport]*replicaConn),
			nodeID:      "primary",
			opts:        Options{Host: "10.0.0.100", Port: 4000},
			peerManager: pm,
		}
		s.role.Store(uint32(RoleLeader))

		ch1 := make(chan []byte, 1)
		t1 := &fakeStreamTransport{id: 1}
		s.replicas[t1] = &replicaConn{transport: t1, nodeID: "n1", listenAddr: "10.0.0.1:5001", sendCh: ch1}

		s.broadcastTopology()

		// Primary should have saved n1 as a peer
		infos := pm.PeerInfos()
		if len(infos) != 1 {
			t.Fatalf("PeerInfos len = %d, want 1", len(infos))
		}
		if infos[0].NodeID != "n1" || infos[0].Addr != "10.0.0.1:5001" {
			t.Errorf("peer = %+v, want {n1 10.0.0.1:5001}", infos[0])
		}
	})

	t.Run("filters replicas with empty nodeID or listenAddr", func(t *testing.T) {
		pm := NewPeerManager(nil, noopLogger)
		s := &Server{
			replicas:    make(map[transport.StreamTransport]*replicaConn),
			nodeID:      "primary",
			opts:        Options{Host: "10.0.0.100", Port: 4000},
			peerManager: pm,
		}
		s.role.Store(uint32(RoleLeader))

		ch1 := make(chan []byte, 1)
		ch2 := make(chan []byte, 1)
		ch3 := make(chan []byte, 1)
		t1 := &fakeStreamTransport{id: 1}
		t2 := &fakeStreamTransport{id: 2}
		t3 := &fakeStreamTransport{id: 3}

		s.replicas[t1] = &replicaConn{transport: t1, nodeID: "n1", listenAddr: "10.0.0.1:5001", sendCh: ch1}
		s.replicas[t2] = &replicaConn{transport: t2, nodeID: "", listenAddr: "10.0.0.2:5002", sendCh: ch2} // empty nodeID
		s.replicas[t3] = &replicaConn{transport: t3, nodeID: "n3", listenAddr: "", sendCh: ch3}            // empty addr

		s.broadcastTopology()

		infos := pm.PeerInfos()
		if len(infos) != 1 {
			t.Fatalf("PeerInfos len = %d, want 1 (only n1)", len(infos))
		}
		if infos[0].NodeID != "n1" {
			t.Errorf("peer = %+v, want n1", infos[0])
		}
	})

	t.Run("sends to all replicas", func(t *testing.T) {
		s := &Server{
			replicas:    make(map[transport.StreamTransport]*replicaConn),
			nodeID:      "primary",
			opts:        Options{Host: "10.0.0.100", Port: 4000},
			peerManager: NewPeerManager(nil, noopLogger),
		}
		s.role.Store(uint32(RoleLeader))

		ch1 := make(chan []byte, 1)
		ch2 := make(chan []byte, 1)
		t1 := &fakeStreamTransport{id: 1}
		t2 := &fakeStreamTransport{id: 2}

		s.replicas[t1] = &replicaConn{transport: t1, nodeID: "n1", listenAddr: "r1:5001", sendCh: ch1}
		s.replicas[t2] = &replicaConn{transport: t2, nodeID: "n2", listenAddr: "r2:5002", sendCh: ch2}

		s.broadcastTopology()

		// Both replicas should receive topology payload
		select {
		case payload := <-ch1:
			req, _ := protocol.DecodeRequest(payload)
			if req.Cmd != protocol.CmdTopology {
				t.Errorf("ch1 cmd = %d, want CmdTopology", req.Cmd)
			}
		default:
			t.Error("replica 1 did not receive topology")
		}

		select {
		case payload := <-ch2:
			req, _ := protocol.DecodeRequest(payload)
			if req.Cmd != protocol.CmdTopology {
				t.Errorf("ch2 cmd = %d, want CmdTopology", req.Cmd)
			}
		default:
			t.Error("replica 2 did not receive topology")
		}
	})

	t.Run("skipped on replica", func(t *testing.T) {
		s := &Server{
			replicas: make(map[transport.StreamTransport]*replicaConn),
		}

		ch := make(chan []byte, 1)
		t1 := &fakeStreamTransport{id: 1}
		s.replicas[t1] = &replicaConn{transport: t1, nodeID: "n1", listenAddr: "r:1", sendCh: ch}

		s.broadcastTopology()

		select {
		case <-ch:
			t.Error("replica should not broadcast topology")
		default:
			// expected
		}
	})

	t.Run("topology contains primary and all replica entries", func(t *testing.T) {
		s := &Server{
			replicas:    make(map[transport.StreamTransport]*replicaConn),
			nodeID:      "primary",
			opts:        Options{Host: "10.0.0.100", Port: 4000},
			peerManager: NewPeerManager(nil, noopLogger),
		}
		s.role.Store(uint32(RoleLeader))

		ch1 := make(chan []byte, 1)
		ch2 := make(chan []byte, 1)
		ch3 := make(chan []byte, 1)
		t1 := &fakeStreamTransport{id: 1}
		t2 := &fakeStreamTransport{id: 2}
		t3 := &fakeStreamTransport{id: 3}

		s.replicas[t1] = &replicaConn{transport: t1, nodeID: "n1", listenAddr: "10.0.0.1:5001", sendCh: ch1}
		s.replicas[t2] = &replicaConn{transport: t2, nodeID: "n2", listenAddr: "10.0.0.2:5002", sendCh: ch2}
		s.replicas[t3] = &replicaConn{transport: t3, nodeID: "n3", listenAddr: "10.0.0.3:5003", sendCh: ch3}

		s.broadcastTopology()

		// Pick any replica's payload — all get the same content
		payload := <-ch1
		req, _ := protocol.DecodeRequest(payload)
		parts := splitTopology(string(req.Value))
		sort.Strings(parts)

		want := []string{"n1@10.0.0.1:5001", "n2@10.0.0.2:5002", "n3@10.0.0.3:5003", "primary@10.0.0.100:4000"}
		if len(parts) != len(want) {
			t.Fatalf("parts = %v, want %v", parts, want)
		}
		for i, p := range parts {
			if p != want[i] {
				t.Errorf("parts[%d] = %q, want %q", i, p, want[i])
			}
		}
	})
}

// ---------------------------------------------------------------------------
// Peer Handshake Tests
// ---------------------------------------------------------------------------

func TestHandlePeerHandshake(t *testing.T) {
	t.Run("responds OK and sets takenOver", func(t *testing.T) {
		s := &Server{
			requestHandlers: make(map[protocol.Cmd]HandlerFunc),
		}

		ctx := &RequestContext{
			StreamTransport: &mockStreamTransport{},
			Request:         protocol.Request{Cmd: protocol.CmdPeerHandshake},
		}

		err := s.handlePeerHandshake(ctx)
		if err != nil {
			t.Fatalf("handlePeerHandshake: %v", err)
		}

		if !ctx.takenOver {
			t.Error("takenOver should be true after handshake")
		}

		mockTrans := ctx.StreamTransport.(*mockStreamTransport)
		resp, _ := protocol.DecodeResponse(mockTrans.written)
		if resp.Status != protocol.StatusOK {
			t.Errorf("status = %d, want StatusOK", resp.Status)
		}
	})
}

func TestHandleRequest_TakenOver(t *testing.T) {
	s := &Server{
		requestHandlers: make(map[protocol.Cmd]HandlerFunc),
	}
	// Register a handler that sets takenOver
	s.requestHandlers[protocol.CmdPeerHandshake] = func(s *Server, ctx *RequestContext) error {
		ctx.takenOver = true
		return s.responseStatusOk(ctx)
	}

	handshakeReq := protocol.Request{Cmd: protocol.CmdPeerHandshake}
	payload, _ := protocol.EncodeRequest(handshakeReq)

	mock := &mockStreamTransport{receiveData: payload}
	takenOver := s.handleRequest(mock, 0)

	if !takenOver {
		t.Error("handleRequest should return takenOver=true")
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func splitTopology(val string) []string {
	if val == "" {
		return nil
	}
	var parts []string
	for _, p := range split(val, protocol.Delimiter) {
		if p != "" {
			parts = append(parts, p)
		}
	}
	return parts
}

func split(s, sep string) []string {
	var result []string
	for len(s) > 0 {
		i := indexOf(s, sep)
		if i < 0 {
			result = append(result, s)
			break
		}
		result = append(result, s[:i])
		s = s[i+len(sep):]
	}
	return result
}

func indexOf(s, sub string) int {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
