package protocol

import (
	"bytes"
	"errors"
	"testing"
)

// TestRequestRoundTrip tests encoding and decoding of various request types
func TestRequestRoundTrip(t *testing.T) {
	tests := []struct {
		name string
		req  Request
	}{
		{
			name: "GET with key",
			req:  Request{Cmd: CmdGet, Key: []byte("mykey")},
		},
		{
			name: "PUT with key and value",
			req:  Request{Cmd: CmdPut, Key: []byte("k"), Value: []byte("v"), Seq: 123},
		},
		{
			name: "PUT with empty value",
			req:  Request{Cmd: CmdPut, Key: []byte("k"), Value: []byte{}, Seq: 1},
		},
		{
			name: "GET with empty key",
			req:  Request{Cmd: CmdGet, Key: []byte{}},
		},
		{
			name: "REPLICATE with seq",
			req:  Request{Cmd: CmdReplicate, Seq: 456, Value: []byte("replid-123")},
		},
		{
			name: "PING with seq (for heartbeat)",
			req:  Request{Cmd: CmdPing, Seq: 789}, // Seq indicates primary's position
		},
		{
			name: "REPLICAOF with primary address",
			req:  Request{Cmd: CmdReplicaOf, Value: []byte("localhost:6379")},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			payload, err := EncodeRequest(tt.req)
			if err != nil {
				t.Fatalf("EncodeRequest: %v", err)
			}

			got, err := DecodeRequest(payload)
			if err != nil {
				t.Fatalf("DecodeRequest: %v", err)
			}

			if got.Cmd != tt.req.Cmd {
				t.Errorf("Cmd = %v, want %v", got.Cmd, tt.req.Cmd)
			}
			if !bytes.Equal(got.Key, tt.req.Key) {
				t.Errorf("Key = %q, want %q", got.Key, tt.req.Key)
			}
			if !bytes.Equal(got.Value, tt.req.Value) {
				t.Errorf("Value = %q, want %q", got.Value, tt.req.Value)
			}
			if got.Seq != tt.req.Seq {
				t.Errorf("Seq = %d, want %d", got.Seq, tt.req.Seq)
			}
		})
	}
}

// TestResponseRoundTrip tests encoding and decoding of various response types
func TestResponseRoundTrip(t *testing.T) {
	tests := []struct {
		name string
		resp Response
	}{
		{
			name: "StatusOK with value",
			resp: Response{Status: StatusOK, Value: []byte("hello"), Seq: 42},
		},
		{
			name: "StatusOK with empty value",
			resp: Response{Status: StatusOK, Value: []byte{}, Seq: 1},
		},
		{
			name: "StatusNotFound",
			resp: Response{Status: StatusNotFound, Seq: 100},
		},
		{
			name: "StatusError",
			resp: Response{Status: StatusError, Seq: 200},
		},
		{
			name: "StatusReadOnly with primary address",
			resp: Response{Status: StatusReadOnly, Value: []byte("primary:6379"), Seq: 5},
		},
		{
			name: "StatusReplicaTooStale with primary address",
			resp: Response{Status: StatusReplicaTooStale, Value: []byte("primary:6379"), Seq: 10},
		},
		{
			name: "StatusPong",
			resp: Response{Status: StatusPong, Seq: 0},
		},
		{
			name: "StatusFullResync with snapshot",
			resp: Response{Status: StatusFullResync, Value: []byte("snapshot-data"), Seq: 0},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			payload, err := EncodeResponse(tt.resp)
			if err != nil {
				t.Fatalf("EncodeResponse: %v", err)
			}

			got, err := DecodeResponse(payload)
			if err != nil {
				t.Fatalf("DecodeResponse: %v", err)
			}

			if got.Status != tt.resp.Status {
				t.Errorf("Status = %v, want %v", got.Status, tt.resp.Status)
			}
			if !bytes.Equal(got.Value, tt.resp.Value) {
				t.Errorf("Value = %q, want %q", got.Value, tt.resp.Value)
			}
			if got.Seq != tt.resp.Seq {
				t.Errorf("Seq = %d, want %d", got.Seq, tt.resp.Seq)
			}
		})
	}
}

// TestResponseEncodeValidation tests that invalid responses are rejected
func TestResponseEncodeValidation(t *testing.T) {
	tests := []struct {
		name    string
		resp    Response
		wantErr error
	}{
		{
			name:    "StatusNotFound with value (invalid)",
			resp:    Response{Status: StatusNotFound, Value: []byte("no")},
			wantErr: ErrInvalidMessage,
		},
		{
			name:    "StatusError with value (invalid)",
			resp:    Response{Status: StatusError, Value: []byte("boom")},
			wantErr: ErrInvalidMessage,
		},
		{
			name:    "StatusPong with value (invalid)",
			resp:    Response{Status: StatusPong, Value: []byte("x")},
			wantErr: ErrInvalidMessage,
		},
		{
			name:    "StatusCleaning with value (invalid)",
			resp:    Response{Status: StatusCleaning, Value: []byte("x")},
			wantErr: ErrInvalidMessage,
		},
		{
			name:    "unknown status",
			resp:    Response{Status: Status(99), Value: []byte{}},
			wantErr: ErrUnknownStatus,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := EncodeResponse(tt.resp)
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("EncodeResponse error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}
