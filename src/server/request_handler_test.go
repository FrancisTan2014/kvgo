package server

import (
	"context"
	"encoding/binary"
	"io"
	"kvgo/engine"
	"kvgo/protocol"
	"os"
	"testing"
	"time"
)

// TestHandleGet_BasicOperation tests the GET handler with actual DB operations
func TestHandleGet_BasicOperation(t *testing.T) {
	dir := t.TempDir()
	db, err := engine.NewDB(dir, context.Background())
	if err != nil {
		t.Fatalf("NewDB: %v", err)
	}
	defer db.Close()

	// Populate DB
	if err := db.Put("existing-key", []byte("existing-value")); err != nil {
		t.Fatalf("Put: %v", err)
	}

	tests := []struct {
		name       string
		key        string
		wantStatus protocol.Status
		wantValue  string
	}{
		{
			name:       "existing key returns value",
			key:        "existing-key",
			wantStatus: protocol.StatusOK,
			wantValue:  "existing-value",
		},
		{
			name:       "non-existent key returns not found",
			key:        "missing-key",
			wantStatus: protocol.StatusNotFound,
			wantValue:  "",
		},
		{
			name:       "empty key",
			key:        "",
			wantStatus: protocol.StatusNotFound,
			wantValue:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &Server{
				db: db,
			}
			s.role.Store(uint32(RoleLeader))
			s.seq.Store(10) // Primary uses seq, not lastSeq

			req := protocol.Request{Cmd: protocol.CmdGet, Key: []byte(tt.key)}

			// Use mock transport
			mockTransport := &mockStreamTransport{}

			ctx := &RequestContext{
				StreamTransport: mockTransport,
				Request:         req,
			}

			err := s.handleGet(ctx)
			if err != nil {
				t.Fatalf("handleGet: %v", err)
			}

			// Decode captured response
			capturedResp, err := protocol.DecodeResponse(mockTransport.written)
			if err != nil {
				t.Fatalf("DecodeResponse: %v", err)
			}

			if capturedResp.Status != tt.wantStatus {
				t.Errorf("status = %v, want %v", capturedResp.Status, tt.wantStatus)
			}
			if string(capturedResp.Value) != tt.wantValue {
				t.Errorf("value = %q, want %q", capturedResp.Value, tt.wantValue)
			}
			if capturedResp.Seq != 10 {
				t.Errorf("seq = %d, want 10", capturedResp.Seq)
			}
		})
	}
}

// TestHandlePut_BasicOperation tests the PUT handler with actual DB operations
func TestHandlePut_BasicOperation(t *testing.T) {
	dir := t.TempDir()
	db, err := engine.NewDB(dir, context.Background())
	if err != nil {
		t.Fatalf("NewDB: %v", err)
	}
	defer db.Close()

	s := &Server{
		db:   db,
		opts: Options{},
	}
	s.role.Store(uint32(RoleLeader))
	s.seq.Store(100)

	tests := []struct {
		name    string
		key     string
		value   string
		wantSeq uint64
	}{
		{
			name:    "simple put",
			key:     "key1",
			value:   "value1",
			wantSeq: 101,
		},
		{
			name:    "overwrite existing",
			key:     "key1",
			value:   "value1-updated",
			wantSeq: 102,
		},
		{
			name:    "empty value",
			key:     "key2",
			value:   "",
			wantSeq: 103,
		},
		{
			name:    "unicode key and value",
			key:     "用户",
			value:   "数据",
			wantSeq: 104,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := protocol.Request{
				Cmd:   protocol.CmdPut,
				Key:   []byte(tt.key),
				Value: []byte(tt.value),
			}

			mockTransport := &mockStreamTransport{}

			ctx := &RequestContext{
				StreamTransport: mockTransport,
				Request:         req,
			}

			err := s.handlePut(ctx)
			if err != nil {
				t.Fatalf("handlePut: %v", err)
			}

			// Decode captured response
			capturedResp, err := protocol.DecodeResponse(mockTransport.written)
			if err != nil {
				t.Fatalf("DecodeResponse: %v", err)
			}

			// Verify response
			if capturedResp.Status != protocol.StatusOK {
				t.Errorf("status = %v, want StatusOK", capturedResp.Status)
			}
			if capturedResp.Seq != tt.wantSeq {
				t.Errorf("seq = %d, want %d", capturedResp.Seq, tt.wantSeq)
			}

			// Verify data was actually written to DB
			val, ok := db.Get(tt.key)
			if !ok {
				t.Errorf("key %q not found in DB after PUT", tt.key)
			}
			if string(val) != tt.value {
				t.Errorf("DB value = %q, want %q", val, tt.value)
			}
		})
	}
}

// TestHandlePut_ReplicaRejection tests that replicas reject PUT requests
func TestHandlePut_ReplicaRejection(t *testing.T) {
	dir := t.TempDir()
	db, err := engine.NewDB(dir, context.Background())
	if err != nil {
		t.Fatalf("NewDB: %v", err)
	}
	defer db.Close()

	s := &Server{
		db:   db,
		opts: Options{ReplicaOf: "primary:6379"},
	}

	req := protocol.Request{
		Cmd:   protocol.CmdPut,
		Key:   []byte("test"),
		Value: []byte("value"),
	}

	mockTransport := &mockStreamTransport{}

	ctx := &RequestContext{
		StreamTransport: mockTransport,
		Request:         req,
	}

	err = s.handlePut(ctx)
	if err != nil {
		t.Fatalf("handlePut: %v", err)
	}

	capturedResp, err := protocol.DecodeResponse(mockTransport.written)
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}

	err = s.handlePut(ctx)
	if err != nil {
		t.Fatalf("handlePut: %v", err)
	}

	if capturedResp.Status != protocol.StatusReadOnly {
		t.Errorf("status = %v, want StatusReadOnly", capturedResp.Status)
	}
	if string(capturedResp.Value) != "primary:6379" {
		t.Errorf("value = %q, want primary address", capturedResp.Value)
	}

	// Verify data was NOT written to DB
	if _, ok := db.Get("test"); ok {
		t.Error("replica should not write to DB")
	}
}

// TestHandlePing_HeartbeatUpdate tests PING updates heartbeat and primarySeq
func TestHandlePing_HeartbeatUpdate(t *testing.T) {
	termBytes := func(term uint64) []byte {
		b := make([]byte, 8)
		binary.LittleEndian.PutUint64(b, term)
		return b
	}

	tests := []struct {
		name           string
		role           Role
		myTerm         uint64
		pingSeq        uint64
		pingTerm       uint64
		wantSeqUpdated bool
		wantHeartbeat  bool
	}{
		{
			name:           "replica updates heartbeat and seq (same term)",
			role:           RoleFollower,
			myTerm:         5,
			pingSeq:        123,
			pingTerm:       5,
			wantSeqUpdated: true,
			wantHeartbeat:  true,
		},
		{
			name:           "primary doesn't update heartbeat",
			role:           RoleLeader,
			myTerm:         5,
			pingSeq:        456,
			pingTerm:       5,
			wantSeqUpdated: false,
			wantHeartbeat:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &Server{
				lastHeartbeat: time.Now().Add(-1 * time.Hour), // Old timestamp
				primarySeq:    0,
				opts:          Options{Logger: nil}, // Use noop logger
			}
			s.role.Store(uint32(tt.role))
			s.term.Store(tt.myTerm)

			req := protocol.Request{Cmd: protocol.CmdPing, Seq: tt.pingSeq, Value: termBytes(tt.pingTerm)}

			mockTransport := &mockStreamTransport{}

			ctx := &RequestContext{
				StreamTransport: mockTransport,
				Request:         req,
			}

			beforeHeartbeat := s.lastHeartbeat
			err := s.handlePing(ctx)
			if err != nil {
				t.Fatalf("handlePing: %v", err)
			}

			// Replica sends PONG as Response, primary doesn't send anything
			if tt.role == RoleFollower {
				// Decode captured response (PONG is now a Response with StatusPong)
				capturedResp, err := protocol.DecodeResponse(mockTransport.written)
				if err != nil {
					t.Fatalf("DecodeResponse: %v", err)
				}

				// Verify response
				if capturedResp.Status != protocol.StatusPong {
					t.Errorf("status = %v, want StatusPong", capturedResp.Status)
				}
			} else {
				// Primary shouldn't send anything
				if mockTransport.written != nil {
					t.Errorf("primary should not send response, but sent: %v", mockTransport.written)
				}
			}

			// Verify heartbeat update
			heartbeatUpdated := s.lastHeartbeat.After(beforeHeartbeat)
			if heartbeatUpdated != tt.wantHeartbeat {
				t.Errorf("heartbeat updated = %v, want %v", heartbeatUpdated, tt.wantHeartbeat)
			}

			// Verify primarySeq update
			if tt.wantSeqUpdated {
				if s.primarySeq != tt.pingSeq {
					t.Errorf("primarySeq = %d, want %d", s.primarySeq, tt.pingSeq)
				}
			} else {
				if s.primarySeq != 0 {
					t.Errorf("primarySeq = %d, want 0 (should not update)", s.primarySeq)
				}
			}
		})
	}
}

// TestHandlePing_TermFencing tests stale primary fencing via PING term check
func TestHandlePing_TermFencing(t *testing.T) {
	termBytes := func(term uint64) []byte {
		b := make([]byte, 8)
		binary.LittleEndian.PutUint64(b, term)
		return b
	}

	tests := []struct {
		name          string
		myTerm        uint64
		myRole        Role
		pingTerm      uint64
		wantHeartbeat bool   // should lastHeartbeat be updated?
		wantTerm      uint64 // expected term after handling
		wantRole      Role
	}{
		{
			name:          "reject stale term — no heartbeat update",
			myTerm:        10,
			myRole:        RoleFollower,
			pingTerm:      5,
			wantHeartbeat: false,
			wantTerm:      10,
			wantRole:      RoleFollower,
		},
		{
			name:          "higher term — adopt and update heartbeat",
			myTerm:        3,
			myRole:        RoleFollower,
			pingTerm:      7,
			wantHeartbeat: true,
			wantTerm:      7,
			wantRole:      RoleFollower,
		},
		{
			name:          "higher term — candidate steps down",
			myTerm:        3,
			myRole:        RoleCandidate,
			pingTerm:      7,
			wantHeartbeat: true,
			wantTerm:      7,
			wantRole:      RoleFollower,
		},
		{
			name:          "no term in value — treated as term 0",
			myTerm:        5,
			myRole:        RoleFollower,
			pingTerm:      0, // empty Value
			wantHeartbeat: false,
			wantTerm:      5,
			wantRole:      RoleFollower,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &Server{
				lastHeartbeat: time.Now().Add(-1 * time.Hour),
				opts:          Options{Logger: nil},
			}
			s.role.Store(uint32(tt.myRole))
			s.term.Store(tt.myTerm)

			var value []byte
			if tt.pingTerm > 0 {
				value = termBytes(tt.pingTerm)
			}

			req := protocol.Request{Cmd: protocol.CmdPing, Seq: 100, Value: value}
			mock := &mockStreamTransport{}
			ctx := &RequestContext{StreamTransport: mock, Request: req}

			before := s.lastHeartbeat
			err := s.handlePing(ctx)
			if err != nil {
				t.Fatalf("handlePing: %v", err)
			}

			heartbeatUpdated := s.lastHeartbeat.After(before)
			if heartbeatUpdated != tt.wantHeartbeat {
				t.Errorf("heartbeat updated = %v, want %v", heartbeatUpdated, tt.wantHeartbeat)
			}
			if s.term.Load() != tt.wantTerm {
				t.Errorf("term = %d, want %d", s.term.Load(), tt.wantTerm)
			}
			if s.currentRole() != tt.wantRole {
				t.Errorf("role = %s, want %s", s.currentRole(), tt.wantRole)
			}

			// PONG is always sent as Response (even on stale rejection, carrying our higher term)
			if mock.written != nil {
				pong, err := protocol.DecodeResponse(mock.written)
				if err != nil {
					t.Fatalf("DecodeResponse: %v", err)
				}
				if pong.Status != protocol.StatusPong {
					t.Errorf("response status = %v, want StatusPong", pong.Status)
				}
				// PONG carries our term
				if len(pong.Value) >= 8 {
					pongTerm := binary.LittleEndian.Uint64(pong.Value[:8])
					if pongTerm != tt.wantTerm {
						t.Errorf("pong term = %d, want %d", pongTerm, tt.wantTerm)
					}
				}
			}
		})
	}
}

// TestProcessPongResponse_TermFencing tests stale primary steps down on higher-term PONG
func TestProcessPongResponse_TermFencing(t *testing.T) {
	termBytes := func(term uint64) []byte {
		b := make([]byte, 8)
		binary.LittleEndian.PutUint64(b, term)
		return b
	}

	tests := []struct {
		name     string
		myTerm   uint64
		myRole   Role
		pongTerm uint64
		wantTerm uint64
		wantRole Role
	}{
		{
			name:     "same term — no change",
			myTerm:   5,
			myRole:   RoleLeader,
			pongTerm: 5,
			wantTerm: 5,
			wantRole: RoleLeader,
		},
		{
			name:     "lower term — no change",
			myTerm:   5,
			myRole:   RoleLeader,
			pongTerm: 3,
			wantTerm: 5,
			wantRole: RoleLeader,
		},
		{
			name:     "higher term — leader steps down",
			myTerm:   5,
			myRole:   RoleLeader,
			pongTerm: 10,
			wantTerm: 10,
			wantRole: RoleFollower,
		},
		{
			name:     "higher term — candidate steps down",
			myTerm:   5,
			myRole:   RoleCandidate,
			pongTerm: 8,
			wantTerm: 8,
			wantRole: RoleFollower,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &Server{
				opts:        Options{Logger: nil},
				peerManager: NewPeerManager(nil, noopLogger),
			}
			s.role.Store(uint32(tt.myRole))
			s.term.Store(tt.myTerm)
			s.roleChanged = make(chan struct{})

			resp := protocol.Response{Status: protocol.StatusPong, Value: termBytes(tt.pongTerm)}
			payload, _ := protocol.EncodeResponse(resp)

			rc := &replicaConn{listenAddr: "127.0.0.1:9999"}
			s.processPongResponse(payload, rc)

			if s.term.Load() != tt.wantTerm {
				t.Errorf("term = %d, want %d", s.term.Load(), tt.wantTerm)
			}
			if s.currentRole() != tt.wantRole {
				t.Errorf("role = %s, want %s", s.currentRole(), tt.wantRole)
			}
		})
	}
}

// TestRequestDispatch tests command routing to correct handlers
func TestRequestDispatch(t *testing.T) {
	tests := []struct {
		name    string
		cmd     protocol.Cmd
		wantErr bool
	}{
		{name: "GET", cmd: protocol.CmdGet, wantErr: false},
		{name: "PUT", cmd: protocol.CmdPut, wantErr: false},
		{name: "PING", cmd: protocol.CmdPing, wantErr: false},
		{name: "REPLICATE", cmd: protocol.CmdReplicate, wantErr: false},
		{name: "PROMOTE", cmd: protocol.CmdPromote, wantErr: false},
		{name: "REPLICAOF", cmd: protocol.CmdReplicaOf, wantErr: false},
		{name: "CLEANUP", cmd: protocol.CmdCleanup, wantErr: false},
	}

	dir := t.TempDir()
	db, err := engine.NewDB(dir, context.Background())
	if err != nil {
		t.Fatalf("NewDB: %v", err)
	}
	defer db.Close()

	s := &Server{
		db:              db,
		requestHandlers: make(map[protocol.Cmd]HandlerFunc),
	}
	s.role.Store(uint32(RoleLeader))
	s.registerRequestHandlers()

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := s.requestHandlers[tt.cmd]
			if handler == nil {
				t.Errorf("no handler registered for %v", tt.cmd)
			}
		})
	}
}

func TestStoreRestoreState_Peers(t *testing.T) {
	dir := t.TempDir()
	pm := NewPeerManager(nil, noopLogger)
	pm.SavePeers([]PeerInfo{{NodeID: "n1", Addr: "10.0.0.1:4000"}, {NodeID: "n2", Addr: "10.0.0.2:4001"}})

	s := &Server{
		opts:        Options{DataDir: dir},
		peerManager: pm,
	}
	s.nodeID = "self"
	s.replid = "rid1"
	s.lastSeq.Store(42)
	s.term.Store(5)
	s.votedFor = "n1"

	if err := s.storeState(); err != nil {
		t.Fatalf("storeState: %v", err)
	}
	if s.metaFile != nil {
		_ = s.metaFile.Close()
	}

	// Create a fresh server and restore
	pm2 := NewPeerManager(nil, noopLogger)
	s2 := &Server{
		opts:        Options{DataDir: dir},
		peerManager: pm2,
	}
	if err := s2.restoreState(); err != nil {
		t.Fatalf("restoreState: %v", err)
	}

	if s2.nodeID != "self" {
		t.Errorf("nodeID = %q, want self", s2.nodeID)
	}
	if s2.term.Load() != 5 {
		t.Errorf("term = %d, want 5", s2.term.Load())
	}
	if s2.votedFor != "n1" {
		t.Errorf("votedFor = %q, want n1", s2.votedFor)
	}

	// Verify peers were restored
	restoredPeers := pm2.PeerInfos()
	if len(restoredPeers) != 2 {
		t.Fatalf("restored peers = %d, want 2", len(restoredPeers))
	}
	peerMap := make(map[string]string)
	for _, pi := range restoredPeers {
		peerMap[pi.NodeID] = pi.Addr
	}
	if peerMap["n1"] != "10.0.0.1:4000" || peerMap["n2"] != "10.0.0.2:4001" {
		t.Errorf("restored peers = %v, want n1->10.0.0.1:4000 n2->10.0.0.2:4001", peerMap)
	}
}

func TestStoreRestoreState_EmptyPeers(t *testing.T) {
	dir := t.TempDir()
	pm := NewPeerManager(nil, noopLogger)
	// No peers saved — peerManager is empty

	s := &Server{
		opts:        Options{DataDir: dir},
		peerManager: pm,
	}
	s.nodeID = "self"
	s.replid = "rid1"
	s.term.Store(3)

	if err := s.storeState(); err != nil {
		t.Fatalf("storeState: %v", err)
	}
	if s.metaFile != nil {
		_ = s.metaFile.Close()
	}

	pm2 := NewPeerManager(nil, noopLogger)
	s2 := &Server{
		opts:        Options{DataDir: dir},
		peerManager: pm2,
	}
	if err := s2.restoreState(); err != nil {
		t.Fatalf("restoreState: %v", err)
	}

	if len(pm2.PeerInfos()) != 0 {
		t.Errorf("restored peers = %v, want empty", pm2.PeerInfos())
	}
}

func TestRestoreState_MalformedPeers(t *testing.T) {
	dir := t.TempDir()

	// Write a meta file with malformed peer entries
	content := "nodeID:self\nreplid:rid1\nlastSeq:10\nterm:2\nvotedFor:\npeers:n1@a:1,,@bad,noatsign,n2@b:2"
	if err := os.WriteFile(dir+"/replication.meta", []byte(content), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	pm := NewPeerManager(nil, noopLogger)
	s := &Server{
		opts:        Options{DataDir: dir},
		peerManager: pm,
	}
	if err := s.restoreState(); err != nil {
		t.Fatalf("restoreState: %v", err)
	}

	// Only n1 and n2 should survive
	infos := pm.PeerInfos()
	if len(infos) != 2 {
		t.Fatalf("restored peers = %d, want 2", len(infos))
	}
	peerMap := make(map[string]string)
	for _, pi := range infos {
		peerMap[pi.NodeID] = pi.Addr
	}
	if peerMap["n1"] != "a:1" || peerMap["n2"] != "b:2" {
		t.Errorf("restored peers = %v, want n1->a:1 n2->b:2", peerMap)
	}
}

func TestRestoreState_NilPeerManager(t *testing.T) {
	dir := t.TempDir()

	content := "nodeID:self\nreplid:rid1\nlastSeq:10\nterm:2\nvotedFor:\npeers:n1@a:1,n2@b:2"
	if err := os.WriteFile(dir+"/replication.meta", []byte(content), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	s := &Server{
		opts:        Options{DataDir: dir},
		peerManager: nil, // nil peerManager — should not panic
	}
	if err := s.restoreState(); err != nil {
		t.Fatalf("restoreState: %v", err)
	}

	// Just verify no panic and other fields restored
	if s.nodeID != "self" {
		t.Errorf("nodeID = %q, want self", s.nodeID)
	}
	if s.term.Load() != 2 {
		t.Errorf("term = %d, want 2", s.term.Load())
	}
}

// mockBuffer captures writes and provides reads for testing
type mockBuffer struct {
	written []byte
	framed  []byte // raw framed data
}

func (m *mockBuffer) Write(p []byte) (n int, err error) {
	m.framed = append(m.framed, p...)

	// Strip frame header (4 bytes) if we have enough data
	if len(m.framed) >= 4 {
		// Extract payload (skip 4-byte length prefix)
		m.written = m.framed[4:]
	}
	return len(p), nil
}

func (m *mockBuffer) Read(p []byte) (n int, err error) {
	return 0, io.EOF
}
