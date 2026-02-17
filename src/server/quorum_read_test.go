package server

import (
	"kvgo/protocol"
	"net"
	"testing"
	"time"
)

// TestQuorumReadFromReplica_ResponseTypes tests that helper accepts OK and NotFound.
func TestQuorumReadFromReplica_ResponseTypes(t *testing.T) {
	tests := []struct {
		name       string
		respStatus protocol.Status
		respValue  []byte
		respSeq    uint64
		wantOk     bool
	}{
		{
			name:       "StatusOK accepted",
			respStatus: protocol.StatusOK,
			respValue:  []byte("test-value"),
			respSeq:    42,
			wantOk:     true,
		},
		{
			name:       "StatusNotFound accepted",
			respStatus: protocol.StatusNotFound,
			respValue:  nil,
			respSeq:    50,
			wantOk:     true,
		},
		{
			name:       "StatusError rejected",
			respStatus: protocol.StatusError,
			wantOk:     false,
		},
		{
			name:       "StatusReadOnly rejected",
			respStatus: protocol.StatusReadOnly,
			respValue:  []byte("primary:6379"),
			wantOk:     false,
		},
		{
			name:       "StatusQuorumFailed rejected",
			respStatus: protocol.StatusQuorumFailed,
			wantOk:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &Server{
				opts: Options{
					QuorumReadTimeout: 100 * time.Millisecond,
				},
			}

			// Create mock response
			respPayload, _ := protocol.EncodeResponse(protocol.Response{
				Status: tt.respStatus,
				Value:  tt.respValue,
				Seq:    tt.respSeq,
			})

			// Create mock transport
			mockTransport := &mockRequestTransport{
				response: respPayload,
			}

			_, _, ok := s.quorumReadFromReplica(mockTransport, []byte("request"), "mock-addr")

			if ok != tt.wantOk {
				t.Errorf("ok = %v, want %v", ok, tt.wantOk)
			}
		})
	}
}

// TestDoGet_SeqSelection tests seq selection logic is already covered by TestHandleGet_ReplicaStaleness
// Integration testing with full request flow will be done in Episode 028+ with service discovery

// ---------------------------------------------------------------------------
// Mock types for testing
// ---------------------------------------------------------------------------

// mockReadWriter implements io.Reader and io.Writer for testing Framer
type mockReadWriter struct {
	readData []byte
	readPos  int
	readErr  error
	writeErr error
}

func (m *mockReadWriter) Read(p []byte) (n int, err error) {
	if m.readErr != nil {
		return 0, m.readErr
	}
	if m.readPos >= len(m.readData) {
		return 0, &net.OpError{Op: "read", Err: &timeoutError{}}
	}
	// Write frame header + data
	frameLen := len(m.readData) - m.readPos
	if len(p) < 4 {
		return 0, &net.OpError{Op: "read", Err: &timeoutError{}}
	}
	// Simulate frame protocol: [4 bytes length][data]
	p[0] = byte(frameLen)
	p[1] = byte(frameLen >> 8)
	p[2] = byte(frameLen >> 16)
	p[3] = byte(frameLen >> 24)
	n = 4
	if len(p) > 4 {
		copied := copy(p[4:], m.readData[m.readPos:])
		n += copied
		m.readPos += copied
	}
	return n, nil
}

func (m *mockReadWriter) Write(p []byte) (n int, err error) {
	if m.writeErr != nil {
		return 0, m.writeErr
	}
	return len(p), nil
}

func (m *mockReadWriter) SetReadDeadline(t time.Time) error  { return nil }
func (m *mockReadWriter) SetWriteDeadline(t time.Time) error { return nil }

type mockConn struct {
	id int
}

func (m *mockConn) Read(b []byte) (n int, err error)   { return 0, nil }
func (m *mockConn) Write(b []byte) (n int, err error)  { return len(b), nil }
func (m *mockConn) Close() error                       { return nil }
func (m *mockConn) LocalAddr() net.Addr                { return &net.TCPAddr{} }
func (m *mockConn) RemoteAddr() net.Addr               { return &net.TCPAddr{} }
func (m *mockConn) SetDeadline(t time.Time) error      { return nil }
func (m *mockConn) SetReadDeadline(t time.Time) error  { return nil }
func (m *mockConn) SetWriteDeadline(t time.Time) error { return nil }

type timeoutError struct{}

func (e *timeoutError) Error() string   { return "timeout" }
func (e *timeoutError) Timeout() bool   { return true }
func (e *timeoutError) Temporary() bool { return true }
