package server

import (
	"context"
	"kvgo/protocol"
	"net"
	"testing"
	"time"
)

// TestStrongConsistencyRead verifies that a replica blocks on GET until it catches up
// to the requested sequence number (read-your-writes guarantee).
func TestStrongConsistencyRead(t *testing.T) {
	// Start primary
	primaryDir := t.TempDir()
	primary, err := NewServer(Options{
		Port:         0,
		DataDir:      primaryDir,
		ReadTimeout:  1 * time.Second,
		WriteTimeout: 1 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewServer primary: %v", err)
	}
	if err := primary.Start(); err != nil {
		t.Fatalf("Start primary: %v", err)
	}
	defer primary.Shutdown(context.Background())

	// Start replica
	replicaDir := t.TempDir()
	replica, err := NewServer(Options{
		Port:              0,
		DataDir:           replicaDir,
		ReplicaOf:         primary.Addr(),
		ReadTimeout:       1 * time.Second,
		WriteTimeout:      1 * time.Second,
		StrongReadTimeout: 500 * time.Millisecond, // generous timeout for test
	})
	if err != nil {
		t.Fatalf("NewServer replica: %v", err)
	}
	if err := replica.Start(); err != nil {
		t.Fatalf("Start replica: %v", err)
	}
	defer replica.Shutdown(context.Background())

	// Give replication a moment to connect
	time.Sleep(100 * time.Millisecond)

	// Connect to primary
	primaryConn, err := net.Dial("tcp", primary.Addr())
	if err != nil {
		t.Fatalf("Dial primary: %v", err)
	}
	defer primaryConn.Close()
	primaryFramer := protocol.NewConnFramer(primaryConn)

	// PUT key=foo value=bar to primary
	putReq := protocol.Request{Cmd: protocol.CmdPut, Key: []byte("foo"), Value: []byte("bar")}
	putPayload, err := protocol.EncodeRequest(putReq)
	if err != nil {
		t.Fatalf("EncodeRequest put: %v", err)
	}
	if err := primaryFramer.WriteWithTimeout(putPayload, 1*time.Second); err != nil {
		t.Fatalf("write put: %v", err)
	}

	// Read PUT response to get seq
	putRespPayload, err := primaryFramer.ReadWithTimeout(1 * time.Second)
	if err != nil {
		t.Fatalf("read put resp: %v", err)
	}
	putResp, err := protocol.DecodeResponse(putRespPayload)
	if err != nil {
		t.Fatalf("DecodeResponse put: %v", err)
	}
	if putResp.Status != protocol.StatusOK {
		t.Fatalf("put status: got %v, want StatusOK", putResp.Status)
	}
	if putResp.Seq == 0 {
		t.Fatalf("put response missing Seq")
	}

	t.Logf("PUT returned Seq=%d", putResp.Seq)

	// Connect to replica
	replicaConn, err := net.Dial("tcp", replica.Addr())
	if err != nil {
		t.Fatalf("Dial replica: %v", err)
	}
	defer replicaConn.Close()
	replicaFramer := protocol.NewConnFramer(replicaConn)

	// Strong consistency GET: include WaitForSeq
	getReq := protocol.Request{Cmd: protocol.CmdGet, Key: []byte("foo"), WaitForSeq: putResp.Seq}
	getPayload, err := protocol.EncodeRequest(getReq)
	if err != nil {
		t.Fatalf("EncodeRequest get: %v", err)
	}

	t.Logf("Sending strong GET with WaitForSeq=%d", putResp.Seq)
	start := time.Now()

	if err := replicaFramer.WriteWithTimeout(getPayload, 1*time.Second); err != nil {
		t.Fatalf("write get: %v", err)
	}

	// Read GET response (may block until replica catches up)
	getRespPayload, err := replicaFramer.ReadWithTimeout(1 * time.Second)
	if err != nil {
		t.Fatalf("read get resp: %v", err)
	}

	elapsed := time.Since(start)
	t.Logf("Strong GET completed in %v", elapsed)

	getResp, err := protocol.DecodeResponse(getRespPayload)
	if err != nil {
		t.Fatalf("DecodeResponse get: %v", err)
	}

	// Should get OK with correct value (replica caught up)
	if getResp.Status != protocol.StatusOK {
		t.Fatalf("get status: got %v, want StatusOK", getResp.Status)
	}
	if string(getResp.Value) != "bar" {
		t.Fatalf("get value: got %q, want %q", getResp.Value, "bar")
	}
	if getResp.Seq < putResp.Seq {
		t.Fatalf("get response Seq=%d < put Seq=%d (replica didn't catch up)", getResp.Seq, putResp.Seq)
	}

	t.Logf("✓ Strong consistency read verified: got value=%q, Seq=%d", getResp.Value, getResp.Seq)
}

// TestEventualConsistencyRead verifies that a GET without WaitForSeq returns immediately
// (eventual consistency, default behavior).
func TestEventualConsistencyRead(t *testing.T) {
	// Start primary
	primaryDir := t.TempDir()
	primary, err := NewServer(Options{
		Port:         0,
		DataDir:      primaryDir,
		ReadTimeout:  1 * time.Second,
		WriteTimeout: 1 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewServer primary: %v", err)
	}
	if err := primary.Start(); err != nil {
		t.Fatalf("Start primary: %v", err)
	}
	defer primary.Shutdown(context.Background())

	// Start replica
	replicaDir := t.TempDir()
	replica, err := NewServer(Options{
		Port:         0,
		DataDir:      replicaDir,
		ReplicaOf:    primary.Addr(),
		ReadTimeout:  1 * time.Second,
		WriteTimeout: 1 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewServer replica: %v", err)
	}
	if err := replica.Start(); err != nil {
		t.Fatalf("Start replica: %v", err)
	}
	defer replica.Shutdown(context.Background())

	// Give replication a moment to connect
	time.Sleep(100 * time.Millisecond)

	// Connect to primary and PUT
	primaryConn, err := net.Dial("tcp", primary.Addr())
	if err != nil {
		t.Fatalf("Dial primary: %v", err)
	}
	defer primaryConn.Close()
	primaryFramer := protocol.NewConnFramer(primaryConn)

	putReq := protocol.Request{Cmd: protocol.CmdPut, Key: []byte("key1"), Value: []byte("value1")}
	putPayload, err := protocol.EncodeRequest(putReq)
	if err != nil {
		t.Fatalf("EncodeRequest put: %v", err)
	}
	if err := primaryFramer.WriteWithTimeout(putPayload, 1*time.Second); err != nil {
		t.Fatalf("write put: %v", err)
	}
	if _, err := primaryFramer.ReadWithTimeout(1 * time.Second); err != nil {
		t.Fatalf("read put resp: %v", err)
	}

	// Give replication time to propagate
	time.Sleep(500 * time.Millisecond)

	// Connect to replica
	replicaConn, err := net.Dial("tcp", replica.Addr())
	if err != nil {
		t.Fatalf("Dial replica: %v", err)
	}
	defer replicaConn.Close()
	replicaFramer := protocol.NewConnFramer(replicaConn)

	// Eventual consistency GET: WaitForSeq=0 (default)
	getReq := protocol.Request{Cmd: protocol.CmdGet, Key: []byte("key1"), WaitForSeq: 0}
	getPayload, err := protocol.EncodeRequest(getReq)
	if err != nil {
		t.Fatalf("EncodeRequest get: %v", err)
	}

	start := time.Now()
	if err := replicaFramer.WriteWithTimeout(getPayload, 1*time.Second); err != nil {
		t.Fatalf("write get: %v", err)
	}

	getRespPayload, err := replicaFramer.ReadWithTimeout(1 * time.Second)
	if err != nil {
		t.Fatalf("read get resp: %v", err)
	}

	elapsed := time.Since(start)

	getResp, err := protocol.DecodeResponse(getRespPayload)
	if err != nil {
		t.Fatalf("DecodeResponse get: %v", err)
	}

	// Should complete quickly (no waiting)
	if elapsed > 50*time.Millisecond {
		t.Fatalf("eventual GET took %v, expected < 50ms (should not block)", elapsed)
	}

	if getResp.Status != protocol.StatusOK {
		t.Fatalf("get status: got %v, want StatusOK", getResp.Status)
	}
	if string(getResp.Value) != "value1" {
		t.Fatalf("get value: got %q, want %q", getResp.Value, "value1")
	}

	t.Logf("✓ Eventual consistency read verified: %v (fast path)", elapsed)
}

// TestStrongReadTimeout verifies that strong reads timeout and redirect when replica is too slow.
func TestStrongReadTimeout(t *testing.T) {
	// Start primary
	primaryDir := t.TempDir()
	primary, err := NewServer(Options{
		Port:         0,
		DataDir:      primaryDir,
		ReadTimeout:  1 * time.Second,
		WriteTimeout: 1 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewServer primary: %v", err)
	}
	if err := primary.Start(); err != nil {
		t.Fatalf("Start primary: %v", err)
	}
	defer primary.Shutdown(context.Background())

	// Start replica with SHORT timeout
	replicaDir := t.TempDir()
	replica, err := NewServer(Options{
		Port:              0,
		DataDir:           replicaDir,
		ReplicaOf:         primary.Addr(),
		ReadTimeout:       1 * time.Second,
		WriteTimeout:      1 * time.Second,
		StrongReadTimeout: 10 * time.Millisecond, // very short for test
	})
	if err != nil {
		t.Fatalf("NewServer replica: %v", err)
	}
	if err := replica.Start(); err != nil {
		t.Fatalf("Start replica: %v", err)
	}
	defer replica.Shutdown(context.Background())

	// Don't wait for replication to connect - we want it to be behind

	// Connect to replica
	replicaConn, err := net.Dial("tcp", replica.Addr())
	if err != nil {
		t.Fatalf("Dial replica: %v", err)
	}
	defer replicaConn.Close()
	replicaFramer := protocol.NewConnFramer(replicaConn)

	// Request a sequence number far in the future (replica will never reach it)
	getReq := protocol.Request{Cmd: protocol.CmdGet, Key: []byte("missing"), WaitForSeq: 999999}
	getPayload, err := protocol.EncodeRequest(getReq)
	if err != nil {
		t.Fatalf("EncodeRequest get: %v", err)
	}

	t.Logf("Sending strong GET with impossible WaitForSeq=999999")
	start := time.Now()

	if err := replicaFramer.WriteWithTimeout(getPayload, 1*time.Second); err != nil {
		t.Fatalf("write get: %v", err)
	}

	getRespPayload, err := replicaFramer.ReadWithTimeout(1 * time.Second)
	if err != nil {
		t.Fatalf("read get resp: %v", err)
	}

	elapsed := time.Since(start)
	t.Logf("Strong GET timed out after %v", elapsed)

	getResp, err := protocol.DecodeResponse(getRespPayload)
	if err != nil {
		t.Fatalf("DecodeResponse get: %v", err)
	}

	// Should get StatusReadOnly (redirect to primary)
	if getResp.Status != protocol.StatusReadOnly {
		t.Fatalf("get status: got %v, want StatusReadOnly (timeout redirect)", getResp.Status)
	}

	primaryAddr := string(getResp.Value)
	if primaryAddr != primary.Addr() {
		t.Fatalf("redirect address: got %q, want %q", primaryAddr, primary.Addr())
	}

	// Verify timeout happened around StrongReadTimeout
	if elapsed < 5*time.Millisecond || elapsed > 100*time.Millisecond {
		t.Fatalf("timeout elapsed=%v, expected ~10ms (StrongReadTimeout)", elapsed)
	}

	t.Logf("✓ Timeout redirect verified: elapsed=%v, redirect_to=%s", elapsed, primaryAddr)
}
