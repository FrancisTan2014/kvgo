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
			opts:        Options{Host: "127.0.0.1", Port: 5000},
		}

		self := s.listenAddr() // "127.0.0.1:5000"
		topology := "10.0.0.1:5001" + protocol.Delimiter + self + protocol.Delimiter + "10.0.0.2:5002"

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

		addrs := pm.Addrs()
		sort.Strings(addrs)
		if len(addrs) != 2 {
			t.Fatalf("len = %d, want 2", len(addrs))
		}
		if addrs[0] != "10.0.0.1:5001" || addrs[1] != "10.0.0.2:5002" {
			t.Errorf("addrs = %v, want [10.0.0.1:5001 10.0.0.2:5002]", addrs)
		}
	})

	t.Run("empty topology clears peers", func(t *testing.T) {
		pm := NewPeerManager(nil, noopLogger)
		pm.SavePeers([]string{"a:1", "b:2"})

		s := &Server{peerManager: pm}

		ctx := &RequestContext{
			StreamTransport: &mockStreamTransport{},
			Request:         protocol.Request{Cmd: protocol.CmdTopology},
		}

		err := s.handleTopology(ctx)
		if err != nil {
			t.Fatalf("handleTopology: %v", err)
		}

		if len(pm.Addrs()) != 0 {
			t.Errorf("peers not cleared, got %v", pm.Addrs())
		}
	})
}

func TestBuildTopologyRequest(t *testing.T) {
	s := &Server{}

	t1 := &fakeStreamTransport{id: 1}
	t2 := &fakeStreamTransport{id: 2}

	replicas := map[transport.StreamTransport]*replicaConn{
		t1: {transport: t1, listenAddr: "10.0.0.1:5001"},
		t2: {transport: t2, listenAddr: "10.0.0.2:5002"},
	}

	req := s.buildTopologyRequest(replicas)
	if req.Cmd != protocol.CmdTopology {
		t.Errorf("cmd = %d, want CmdTopology", req.Cmd)
	}

	// Value contains all addresses delimited by newline
	val := string(req.Value)
	// Map iteration order is random, so check both addresses are present
	for _, addr := range []string{"10.0.0.1:5001", "10.0.0.2:5002"} {
		found := false
		for _, part := range splitTopology(val) {
			if part == addr {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("topology %q missing %s", val, addr)
		}
	}
}

func TestBroadcastTopology(t *testing.T) {
	t.Run("sends to all replicas", func(t *testing.T) {
		s := &Server{
			isReplica: false,
			replicas:  make(map[transport.StreamTransport]*replicaConn),
		}

		ch1 := make(chan []byte, 1)
		ch2 := make(chan []byte, 1)
		t1 := &fakeStreamTransport{id: 1}
		t2 := &fakeStreamTransport{id: 2}

		s.replicas[t1] = &replicaConn{transport: t1, listenAddr: "r1:5001", sendCh: ch1}
		s.replicas[t2] = &replicaConn{transport: t2, listenAddr: "r2:5002", sendCh: ch2}

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
			isReplica: true,
			replicas:  make(map[transport.StreamTransport]*replicaConn),
		}

		ch := make(chan []byte, 1)
		t1 := &fakeStreamTransport{id: 1}
		s.replicas[t1] = &replicaConn{transport: t1, listenAddr: "r:1", sendCh: ch}

		s.broadcastTopology()

		select {
		case <-ch:
			t.Error("replica should not broadcast topology")
		default:
			// expected
		}
	})

	t.Run("topology contains all replica addresses", func(t *testing.T) {
		s := &Server{
			isReplica: false,
			replicas:  make(map[transport.StreamTransport]*replicaConn),
		}

		ch1 := make(chan []byte, 1)
		ch2 := make(chan []byte, 1)
		ch3 := make(chan []byte, 1)
		t1 := &fakeStreamTransport{id: 1}
		t2 := &fakeStreamTransport{id: 2}
		t3 := &fakeStreamTransport{id: 3}

		s.replicas[t1] = &replicaConn{transport: t1, listenAddr: "10.0.0.1:5001", sendCh: ch1}
		s.replicas[t2] = &replicaConn{transport: t2, listenAddr: "10.0.0.2:5002", sendCh: ch2}
		s.replicas[t3] = &replicaConn{transport: t3, listenAddr: "10.0.0.3:5003", sendCh: ch3}

		s.broadcastTopology()

		// Pick any replica's payload — all get the same content
		payload := <-ch1
		req, _ := protocol.DecodeRequest(payload)
		parts := splitTopology(string(req.Value))
		sort.Strings(parts)

		want := []string{"10.0.0.1:5001", "10.0.0.2:5002", "10.0.0.3:5003"}
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
