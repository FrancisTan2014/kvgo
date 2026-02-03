package server

import (
	"context"
	"kvgo/protocol"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestServer_PutGet(t *testing.T) {
	dir := t.TempDir()

	s, err := NewServer(Options{Port: 0, DataDir: dir, ReadTimeout: 200 * time.Millisecond, WriteTimeout: 200 * time.Millisecond})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Shutdown(context.Background())

	conn, err := net.Dial("tcp", s.Addr())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()

	f := protocol.NewConnFramer(conn)

	putPayload, err := protocol.EncodeRequest(protocol.Request{Cmd: protocol.CmdPut, Key: []byte("k"), Value: []byte("v")})
	if err != nil {
		t.Fatalf("EncodeRequest put: %v", err)
	}
	if err := f.WriteWithTimeout(putPayload, 200*time.Millisecond); err != nil {
		t.Fatalf("write put: %v", err)
	}
	respPayload, err := f.ReadWithTimeout(200 * time.Millisecond)
	if err != nil {
		t.Fatalf("read put resp: %v", err)
	}
	resp, err := protocol.DecodeResponse(respPayload)
	if err != nil {
		t.Fatalf("DecodeResponse put: %v", err)
	}
	if resp.Status != protocol.StatusOK {
		t.Fatalf("put status: got %v", resp.Status)
	}

	getPayload, err := protocol.EncodeRequest(protocol.Request{Cmd: protocol.CmdGet, Key: []byte("k")})
	if err != nil {
		t.Fatalf("EncodeRequest get: %v", err)
	}
	if err := f.WriteWithTimeout(getPayload, 200*time.Millisecond); err != nil {
		t.Fatalf("write get: %v", err)
	}
	respPayload, err = f.ReadWithTimeout(200 * time.Millisecond)
	if err != nil {
		t.Fatalf("read get resp: %v", err)
	}
	resp, err = protocol.DecodeResponse(respPayload)
	if err != nil {
		t.Fatalf("DecodeResponse get: %v", err)
	}
	if resp.Status != protocol.StatusOK || string(resp.Value) != "v" {
		t.Fatalf("get mismatch: status=%v value=%q", resp.Status, resp.Value)
	}
}

func TestServer_Networks(t *testing.T) {
	tests := []struct {
		name    string
		network string
		host    string // for tcp: leave empty to use default; for unix: socket path
		skip    func() bool
	}{
		{
			name:    "tcp (default)",
			network: "", // empty defaults to tcp
		},
		{
			name:    "tcp explicit",
			network: NetworkTCP,
		},
		{
			name:    "tcp4",
			network: NetworkTCP4,
		},
		{
			name:    "tcp6",
			network: NetworkTCP6,
			host:    "::1",
			skip: func() bool {
				// Skip if IPv6 localhost is not available.
				ln, err := net.Listen("tcp6", "[::1]:0")
				if err != nil {
					return true
				}
				ln.Close()
				return false
			},
		},
		{
			name:    "unix",
			network: NetworkUnix,
			host:    "", // filled in per-test with temp path
			skip: func() bool {
				// Skip on systems that don't support unix sockets.
				dir := os.TempDir()
				sock := filepath.Join(dir, "kvgo-test-unix.sock")
				defer os.Remove(sock)
				ln, err := net.Listen("unix", sock)
				if err != nil {
					return true
				}
				ln.Close()
				return false
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.skip != nil && tc.skip() {
				t.Skipf("skipping %s: not supported on this system", tc.name)
			}

			dir := t.TempDir()
			host := tc.host

			// For unix, create a socket path in temp dir.
			if tc.network == NetworkUnix {
				host = filepath.Join(dir, "server.sock")
			}

			opts := Options{
				Network:      tc.network,
				Host:         host,
				Port:         0, // ephemeral port for tcp*
				DataDir:      dir,
				ReadTimeout:  200 * time.Millisecond,
				WriteTimeout: 200 * time.Millisecond,
			}

			s, err := NewServer(opts)
			if err != nil {
				t.Fatalf("NewServer: %v", err)
			}
			if err := s.Start(); err != nil {
				t.Fatalf("Start: %v", err)
			}
			defer s.Shutdown(context.Background())

			// Dial using the actual network and address.
			network := tc.network
			if network == "" {
				network = NetworkTCP
			}
			conn, err := net.Dial(network, s.Addr())
			if err != nil {
				t.Fatalf("Dial: %v", err)
			}
			defer conn.Close()

			// Simple Put/Get round-trip.
			f := protocol.NewConnFramer(conn)

			putPayload, _ := protocol.EncodeRequest(protocol.Request{Cmd: protocol.CmdPut, Key: []byte("net-test"), Value: []byte("ok")})
			if err := f.WriteWithTimeout(putPayload, 200*time.Millisecond); err != nil {
				t.Fatalf("write put: %v", err)
			}
			respPayload, err := f.ReadWithTimeout(200 * time.Millisecond)
			if err != nil {
				t.Fatalf("read put resp: %v", err)
			}
			resp, _ := protocol.DecodeResponse(respPayload)
			if resp.Status != protocol.StatusOK {
				t.Fatalf("put status: %v", resp.Status)
			}

			getPayload, _ := protocol.EncodeRequest(protocol.Request{Cmd: protocol.CmdGet, Key: []byte("net-test")})
			if err := f.WriteWithTimeout(getPayload, 200*time.Millisecond); err != nil {
				t.Fatalf("write get: %v", err)
			}
			respPayload, err = f.ReadWithTimeout(200 * time.Millisecond)
			if err != nil {
				t.Fatalf("read get resp: %v", err)
			}
			resp, _ = protocol.DecodeResponse(respPayload)
			if resp.Status != protocol.StatusOK || string(resp.Value) != "ok" {
				t.Fatalf("get mismatch: status=%v value=%q", resp.Status, resp.Value)
			}
		})
	}
}

func TestServer_LockPreventsSecondInstance(t *testing.T) {
	dir := t.TempDir()
	// Pre-create dir to match real usage.
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(dir, "dummy"), []byte("x"), 0o600)

	s1, err := NewServer(Options{Port: 0, DataDir: dir})
	if err != nil {
		t.Fatalf("New s1: %v", err)
	}
	if err := s1.Start(); err != nil {
		t.Fatalf("Start s1: %v", err)
	}
	defer s1.Shutdown(context.Background())

	s2, err := NewServer(Options{Port: 0, DataDir: dir})
	if err != nil {
		t.Fatalf("New s2: %v", err)
	}
	if err := s2.Start(); err == nil {
		_ = s2.Shutdown(context.Background())
		t.Fatalf("expected lock error")
	}
}

// TestServer_PingPong verifies that the server responds to ping requests
// with a pong status. This is the foundation for heartbeat-based dead replica detection.
func TestServer_PingPong(t *testing.T) {
	dir := t.TempDir()

	s, err := NewServer(Options{Port: 0, DataDir: dir, ReadTimeout: 200 * time.Millisecond, WriteTimeout: 200 * time.Millisecond})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Shutdown(context.Background())

	conn, err := net.Dial("tcp", s.Addr())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()

	f := protocol.NewConnFramer(conn)

	// Send ping request
	pingPayload, err := protocol.EncodeRequest(protocol.Request{Cmd: protocol.CmdPing})
	if err != nil {
		t.Fatalf("EncodeRequest ping: %v", err)
	}
	if err := f.WriteWithTimeout(pingPayload, 200*time.Millisecond); err != nil {
		t.Fatalf("write ping: %v", err)
	}

	// Read pong response
	respPayload, err := f.ReadWithTimeout(200 * time.Millisecond)
	if err != nil {
		t.Fatalf("read pong resp: %v", err)
	}
	resp, err := protocol.DecodeResponse(respPayload)
	if err != nil {
		t.Fatalf("DecodeResponse pong: %v", err)
	}
	if resp.Status != protocol.StatusPong {
		t.Fatalf("expected StatusPong (%d), got %d", protocol.StatusPong, resp.Status)
	}
}
