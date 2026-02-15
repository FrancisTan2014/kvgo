package server

import (
	"context"
	"io"
	"kvgo/engine"
	"kvgo/protocol"
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
				db:        db,
				isReplica: false,
			}
			s.seq.Store(10) // Primary uses seq, not lastSeq

			req := protocol.Request{Cmd: protocol.CmdGet, Key: []byte(tt.key)}

			// Use mock transport
			mockTransport := &mockStreamTransport{}

			ctx := &RequestContext{
				StreamTransport: mockTransport,
				Request:   req,
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
		db:        db,
		isReplica: false,
		opts:      Options{},
	}
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
				Request:   req,
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
		db:        db,
		isReplica: true,
		opts:      Options{ReplicaOf: "primary:6379"},
	}

	req := protocol.Request{
		Cmd:   protocol.CmdPut,
		Key:   []byte("test"),
		Value: []byte("value"),
	}

	mockTransport := &mockStreamTransport{}

	ctx := &RequestContext{
		StreamTransport: mockTransport,
		Request:   req,
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
	tests := []struct {
		name           string
		isReplica      bool
		pingSeq        uint64
		wantSeqUpdated bool
		wantHeartbeat  bool
	}{
		{
			name:           "replica updates heartbeat and seq",
			isReplica:      true,
			pingSeq:        123,
			wantSeqUpdated: true,
			wantHeartbeat:  true,
		},
		{
			name:           "primary doesn't update heartbeat",
			isReplica:      false,
			pingSeq:        456,
			wantSeqUpdated: false,
			wantHeartbeat:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &Server{
				isReplica:     tt.isReplica,
				lastHeartbeat: time.Now().Add(-1 * time.Hour), // Old timestamp
				primarySeq:    0,
				opts:          Options{Logger: nil}, // Use noop logger
			}

			req := protocol.Request{Cmd: protocol.CmdPing, Seq: tt.pingSeq}

			mockTransport := &mockStreamTransport{}

			ctx := &RequestContext{
				StreamTransport: mockTransport,
				Request:   req,
			}

			beforeHeartbeat := s.lastHeartbeat
			err := s.handlePing(ctx)
			if err != nil {
				t.Fatalf("handlePing: %v", err)
			}

			// Replica sends PONG, primary doesn't send anything
			if tt.isReplica {
				// Decode captured response (PONG is sent as a Request, not Response)
				capturedReq, err := protocol.DecodeRequest(mockTransport.written)
				if err != nil {
					t.Fatalf("DecodeRequest: %v", err)
				}

				// Verify response
				if capturedReq.Cmd != protocol.CmdPong {
					t.Errorf("cmd = %v, want CmdPong", capturedReq.Cmd)
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
		isReplica:       false,
	}
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
