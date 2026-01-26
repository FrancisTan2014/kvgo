package server

import (
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
	defer s.Shutdown()

	conn, err := net.Dial("tcp", s.Addr())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()

	f := protocol.NewConnFramer(conn)

	putPayload, err := protocol.EncodeRequest(protocol.Request{Op: protocol.OpPut, Key: []byte("k"), Value: []byte("v")})
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

	getPayload, err := protocol.EncodeRequest(protocol.Request{Op: protocol.OpGet, Key: []byte("k")})
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
	defer s1.Shutdown()

	s2, err := NewServer(Options{Port: 0, DataDir: dir})
	if err != nil {
		t.Fatalf("New s2: %v", err)
	}
	if err := s2.Start(); err == nil {
		_ = s2.Shutdown()
		t.Fatalf("expected lock error")
	}
}
