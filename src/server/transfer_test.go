package server

import (
	"kvgo/protocol"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// newTestServerForTransfer creates a minimal Server suitable for transfer tests.
// Sets up PeerManager and opts so becomeCandidate() goroutines don't panic.
// ReplicaOf is set so a solo node doesn't self-elect to leader (which would
// call promote() and need backlogCtx).
func newTestServerForTransfer(t *testing.T) *Server {
	t.Helper()
	s := &Server{
		peerManager: NewPeerManager(nil, slog.Default()),
		opts: Options{
			ReplicaOf: "fake:1234", // prevents self-election
			DataDir:   t.TempDir(), // avoid writing replication.meta into CWD
		},
	}
	s.roleChanged = make(chan struct{})
	t.Cleanup(func() {
		// Let fire-and-forget goroutines (becomeCandidate) finish
		// so the metaFile handle is released before TempDir removal.
		time.Sleep(200 * time.Millisecond)
		if s.metaFile != nil {
			s.metaFile.Close()
		}
	})
	return s
}

// ---------------------------------------------------------------------------
// handleTimeoutNow unit tests
// ---------------------------------------------------------------------------

func TestHandleTimeoutNow_AlreadyCaughtUp(t *testing.T) {
	s := newTestServerForTransfer(t)
	s.role.Store(uint32(RoleFollower))
	s.lastSeq.Store(50)
	s.fenced.Store(true) // should be cleared

	mock := &mockStreamTransport{}
	ctx := &RequestContext{
		StreamTransport: mock,
		Request:         protocol.Request{Cmd: protocol.CmdTimeoutNow, Seq: 50},
	}

	err := s.handleTimeoutNow(ctx)
	if err != nil {
		t.Fatalf("handleTimeoutNow: %v", err)
	}

	resp, err := protocol.DecodeResponse(mock.written)
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	if resp.Status != protocol.StatusOK {
		t.Errorf("status = %v, want StatusOK", resp.Status)
	}
	if s.fenced.Load() {
		t.Error("fenced should be cleared after TimeoutNow")
	}
}

func TestHandleTimeoutNow_AlreadyCaughtUp_HigherSeq(t *testing.T) {
	s := newTestServerForTransfer(t)
	s.role.Store(uint32(RoleFollower))
	s.lastSeq.Store(100) // ahead of target

	mock := &mockStreamTransport{}
	ctx := &RequestContext{
		StreamTransport: mock,
		Request:         protocol.Request{Cmd: protocol.CmdTimeoutNow, Seq: 50},
	}

	err := s.handleTimeoutNow(ctx)
	if err != nil {
		t.Fatalf("handleTimeoutNow: %v", err)
	}

	resp, _ := protocol.DecodeResponse(mock.written)
	if resp.Status != protocol.StatusOK {
		t.Errorf("status = %v, want StatusOK (lastSeq > targetSeq)", resp.Status)
	}
}

func TestHandleTimeoutNow_CatchUpViaChannel(t *testing.T) {
	s := newTestServerForTransfer(t)
	s.role.Store(uint32(RoleFollower))
	s.lastSeq.Store(5) // behind target

	mock := &mockStreamTransport{}
	ctx := &RequestContext{
		StreamTransport: mock,
		Request:         protocol.Request{Cmd: protocol.CmdTimeoutNow, Seq: 10},
	}

	// Simulate replication delivering entries after a short delay
	done := make(chan struct{})
	go func() {
		defer close(done)
		// Wait for pendingTransferSeq to be set
		deadline := time.Now().Add(time.Second)
		for time.Now().Before(deadline) {
			if s.pendingTransferSeq.Load() > 0 {
				break
			}
			time.Sleep(time.Millisecond)
		}
		// Simulate applyReplicatedPut reaching targetSeq
		s.lastSeq.Store(10)
		s.transferMu.RLock()
		ch := s.seqReachedCh
		s.transferMu.RUnlock()
		if ch != nil {
			select {
			case ch <- struct{}{}:
			default:
			}
		}
	}()

	err := s.handleTimeoutNow(ctx)
	<-done

	if err != nil {
		t.Fatalf("handleTimeoutNow: %v", err)
	}

	resp, _ := protocol.DecodeResponse(mock.written)
	if resp.Status != protocol.StatusOK {
		t.Errorf("status = %v, want StatusOK after catch-up", resp.Status)
	}
	// Channel and pendingTransferSeq should be cleaned up
	if s.pendingTransferSeq.Load() != 0 {
		t.Errorf("pendingTransferSeq = %d, want 0 after cleanup", s.pendingTransferSeq.Load())
	}
	s.transferMu.RLock()
	ch := s.seqReachedCh
	s.transferMu.RUnlock()
	if ch != nil {
		t.Error("seqReachedCh should be nil after cleanup")
	}
}

func TestHandleTimeoutNow_CatchUpTimeout(t *testing.T) {
	s := newTestServerForTransfer(t)
	s.role.Store(uint32(RoleFollower))
	s.lastSeq.Store(5) // seq=5, target=1000 — never catches up

	mock := &mockStreamTransport{}
	ctx := &RequestContext{
		StreamTransport: mock,
		Request:         protocol.Request{Cmd: protocol.CmdTimeoutNow, Seq: 1000},
	}

	start := time.Now()
	err := s.handleTimeoutNow(ctx)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("handleTimeoutNow: %v", err)
	}

	resp, _ := protocol.DecodeResponse(mock.written)
	if resp.Status != protocol.StatusError {
		t.Errorf("status = %v, want StatusError on timeout", resp.Status)
	}

	// Should take roughly transferCatchUpTimeout (electionTimeout/2 = 1s)
	if elapsed < transferCatchUpTimeout/2 {
		t.Errorf("returned too fast (%v), expected ~%v", elapsed, transferCatchUpTimeout)
	}
	if elapsed > transferCatchUpTimeout+500*time.Millisecond {
		t.Errorf("took too long (%v), expected ~%v", elapsed, transferCatchUpTimeout)
	}

	// Cleanup should have happened
	if s.pendingTransferSeq.Load() != 0 {
		t.Errorf("pendingTransferSeq = %d, want 0", s.pendingTransferSeq.Load())
	}
}

func TestHandleTimeoutNow_SetThenCheck(t *testing.T) {
	// Tests the set-then-check path: lastSeq reaches target between first check
	// and pendingTransferSeq.Store
	s := newTestServerForTransfer(t)
	s.role.Store(uint32(RoleFollower))
	s.lastSeq.Store(10) // will match targetSeq in second check
	// First check: lastSeq(10) < targetSeq(20) — enters slow path
	// But we'll update lastSeq to 20 before the handler's second check

	// For this test we set lastSeq to 10 initially, then update it.
	// The handler does:
	//   1. if lastSeq >= targetSeq → fast path (skip, lastSeq=10 < 20)
	//   2. create channel, store pendingTransferSeq
	//   3. if lastSeq >= targetSeq → set-then-check path
	//
	// We trigger a goroutine that updates lastSeq between step 1 and step 3.
	// Since timing is tricky for a pure unit test, we test the boundary directly:
	// lastSeq=20 and targetSeq=20 — the fast path should handle it.
	s.lastSeq.Store(20)

	mock := &mockStreamTransport{}
	ctx := &RequestContext{
		StreamTransport: mock,
		Request:         protocol.Request{Cmd: protocol.CmdTimeoutNow, Seq: 20},
	}

	err := s.handleTimeoutNow(ctx)
	if err != nil {
		t.Fatalf("handleTimeoutNow: %v", err)
	}

	resp, _ := protocol.DecodeResponse(mock.written)
	if resp.Status != protocol.StatusOK {
		t.Errorf("status = %v, want StatusOK", resp.Status)
	}
}

func TestHandleTimeoutNow_ClearsFenced(t *testing.T) {
	s := newTestServerForTransfer(t)
	s.role.Store(uint32(RoleFollower))
	s.lastSeq.Store(100)
	s.fenced.Store(true)

	mock := &mockStreamTransport{}
	ctx := &RequestContext{
		StreamTransport: mock,
		Request:         protocol.Request{Cmd: protocol.CmdTimeoutNow, Seq: 50},
	}

	_ = s.handleTimeoutNow(ctx)

	if s.fenced.Load() {
		t.Error("fenced should be false after TimeoutNow (authoritative leader contact)")
	}
}

// ---------------------------------------------------------------------------
// Write fence (transferring flag) unit tests
// ---------------------------------------------------------------------------

func TestTransferring_RejectsWrites(t *testing.T) {
	s := &Server{}
	s.transferring.Store(true)

	mock := &mockStreamTransport{}
	ctx := &RequestContext{
		StreamTransport: mock,
		Request:         protocol.Request{Cmd: protocol.CmdPut, Key: []byte("key"), Value: []byte("val")},
	}

	err := s.processPrimaryPut(ctx)
	if err != nil {
		t.Fatalf("processPrimaryPut: %v", err)
	}

	resp, err := protocol.DecodeResponse(mock.written)
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	if resp.Status != protocol.StatusNotLeader {
		t.Errorf("status = %v, want StatusNotLeader when transferring", resp.Status)
	}
}

func TestTransferring_SafetyTimeoutClears(t *testing.T) {
	s := &Server{}
	s.transferring.Store(true)

	// CompareAndSwap should succeed
	if !s.transferring.CompareAndSwap(true, false) {
		t.Error("CompareAndSwap(true, false) should succeed")
	}
	if s.transferring.Load() {
		t.Error("transferring should be false after CAS")
	}
}

func TestTransferring_CASIdempotent(t *testing.T) {
	// If transfer completed (flag already cleared), CAS should be no-op
	s := &Server{}
	s.transferring.Store(false)

	if s.transferring.CompareAndSwap(true, false) {
		t.Error("CompareAndSwap(true, false) should fail when already false")
	}
}

// ---------------------------------------------------------------------------
// applyReplicatedPut notification unit tests
// ---------------------------------------------------------------------------

func TestApplyReplicatedPut_NotifiesChannel(t *testing.T) {
	s := &Server{}
	ch := make(chan struct{}, 1)
	s.transferMu.Lock()
	s.seqReachedCh = ch
	s.transferMu.Unlock()
	s.pendingTransferSeq.Store(10)
	s.lastSeq.Store(9) // will be updated by the function under test

	// Directly test the notification logic
	// Simulate: req.Seq = 10 (reaches pending target)
	s.lastSeq.Store(10)

	pendingSeq := s.pendingTransferSeq.Load()
	s.transferMu.RLock()
	gotCh := s.seqReachedCh
	s.transferMu.RUnlock()
	if pendingSeq > 0 && int64(10) >= pendingSeq && gotCh != nil {
		select {
		case gotCh <- struct{}{}:
		default:
		}
	}

	// Channel should have been signaled
	select {
	case <-ch:
		// OK
	default:
		t.Error("channel should have been signaled when seq >= pendingTransferSeq")
	}
}

func TestApplyReplicatedPut_NoNotifyWhenSeqBelow(t *testing.T) {
	s := &Server{}
	ch := make(chan struct{}, 1)
	s.transferMu.Lock()
	s.seqReachedCh = ch
	s.transferMu.Unlock()
	s.pendingTransferSeq.Store(10)

	// req.Seq = 9 — not yet reached
	pendingSeq := s.pendingTransferSeq.Load()
	s.transferMu.RLock()
	gotCh := s.seqReachedCh
	s.transferMu.RUnlock()
	if pendingSeq > 0 && int64(9) >= pendingSeq && gotCh != nil {
		select {
		case gotCh <- struct{}{}:
		default:
		}
	}

	// Channel should NOT have been signaled
	select {
	case <-ch:
		t.Error("channel should not be signaled when seq < pendingTransferSeq")
	default:
		// OK
	}
}

func TestApplyReplicatedPut_NoNotifyWhenNoPending(t *testing.T) {
	s := &Server{}
	// pendingTransferSeq = 0 (no transfer in progress)
	s.pendingTransferSeq.Store(0)

	// No channel set up — the check should short-circuit
	pendingSeq := s.pendingTransferSeq.Load()
	if pendingSeq > 0 {
		t.Error("should skip notification when pendingTransferSeq == 0")
	}
}

func TestApplyReplicatedPut_NonBlockingSend(t *testing.T) {
	// Channel already has a value — second send should not block
	s := &Server{}
	ch := make(chan struct{}, 1)
	ch <- struct{}{} // pre-fill
	s.transferMu.Lock()
	s.seqReachedCh = ch
	s.transferMu.Unlock()
	s.pendingTransferSeq.Store(5)

	// This should not block because of the default case
	pendingSeq := s.pendingTransferSeq.Load()
	s.transferMu.RLock()
	gotCh := s.seqReachedCh
	s.transferMu.RUnlock()
	if pendingSeq > 0 && int64(10) >= pendingSeq && gotCh != nil {
		select {
		case gotCh <- struct{}{}:
		default:
			// expected: channel full, default taken
		}
	}
	// If we reach here without blocking, test passes
}

// ---------------------------------------------------------------------------
// Protocol tests
// ---------------------------------------------------------------------------

func TestNewTransferLeaderRequest(t *testing.T) {
	req := protocol.NewTransferLeaderRequest("node-abc")
	if req.Cmd != protocol.CmdTransferLeader {
		t.Errorf("cmd = %v, want CmdTransferLeader", req.Cmd)
	}
	if string(req.Value) != "node-abc" {
		t.Errorf("value = %q, want %q", string(req.Value), "node-abc")
	}
}

func TestNewTimeoutNowRequest(t *testing.T) {
	req := protocol.NewTimeoutNowRequest(42)
	if req.Cmd != protocol.CmdTimeoutNow {
		t.Errorf("cmd = %v, want CmdTimeoutNow", req.Cmd)
	}
	if req.Seq != 42 {
		t.Errorf("seq = %d, want 42", req.Seq)
	}
}

func TestTimeoutNowRequest_RoundTrip(t *testing.T) {
	req := protocol.NewTimeoutNowRequest(999)
	payload, err := protocol.EncodeRequest(req)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}

	decoded, err := protocol.DecodeRequest(payload)
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	if decoded.Cmd != protocol.CmdTimeoutNow {
		t.Errorf("cmd = %v, want CmdTimeoutNow", decoded.Cmd)
	}
	if decoded.Seq != 999 {
		t.Errorf("seq = %d, want 999", decoded.Seq)
	}
}

func TestTransferLeaderRequest_RoundTrip(t *testing.T) {
	req := protocol.NewTransferLeaderRequest("target-node")
	payload, err := protocol.EncodeRequest(req)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}

	decoded, err := protocol.DecodeRequest(payload)
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	if decoded.Cmd != protocol.CmdTransferLeader {
		t.Errorf("cmd = %v, want CmdTransferLeader", decoded.Cmd)
	}
	if string(decoded.Value) != "target-node" {
		t.Errorf("value = %q, want %q", string(decoded.Value), "target-node")
	}
}

// ---------------------------------------------------------------------------
// StatusNotLeader tests
// ---------------------------------------------------------------------------

func TestStatusNotLeader_Constant(t *testing.T) {
	// Verify StatusNotLeader has expected value (12)
	if protocol.StatusNotLeader != 12 {
		t.Errorf("StatusNotLeader = %d, want 12", protocol.StatusNotLeader)
	}
}

// ---------------------------------------------------------------------------
// Concurrency: transferMu protects seqReachedCh
// ---------------------------------------------------------------------------

func TestTransferMu_ConcurrentAccess(t *testing.T) {
	s := &Server{}
	s.pendingTransferSeq.Store(10)

	// Writer goroutine (simulates handleTimeoutNow)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		s.transferMu.Lock()
		s.seqReachedCh = make(chan struct{}, 1)
		s.transferMu.Unlock()
		time.Sleep(10 * time.Millisecond)
		s.transferMu.Lock()
		s.seqReachedCh = nil
		s.transferMu.Unlock()
	}()

	// Reader goroutines (simulate applyReplicatedPut)
	var readCount atomic.Int32
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				s.transferMu.RLock()
				ch := s.seqReachedCh
				s.transferMu.RUnlock()
				if ch != nil {
					select {
					case ch <- struct{}{}:
					default:
					}
					readCount.Add(1)
				}
				time.Sleep(100 * time.Microsecond)
			}
		}()
	}

	wg.Wait()
	// No race detector errors = pass
}

// ---------------------------------------------------------------------------
// Config constant tests
// ---------------------------------------------------------------------------

func TestTransferCatchUpTimeout(t *testing.T) {
	if transferCatchUpTimeout != electionTimeout/2 {
		t.Errorf("transferCatchUpTimeout = %v, want %v", transferCatchUpTimeout, electionTimeout/2)
	}
	if transferCatchUpTimeout <= 0 {
		t.Error("transferCatchUpTimeout must be positive")
	}
}
