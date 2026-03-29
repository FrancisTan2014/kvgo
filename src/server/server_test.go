package server

import (
	"context"
	"errors"
	"kvgo/protocol"
	rafttransport "kvgo/transport/raft"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type testServeSuite struct {
	server *Server
	fr     *fakeRaftHost
	fsm    *fakeStateMachine
	fw     *fakeWait
}

func newTestServer(t *testing.T) *testServeSuite {
	t.Helper()

	fsm := newFakeStateMachine()
	fr := newFakeRaftHost()
	fw := newFakeWait()

	ctx, cancel := context.WithCancel(context.Background())

	s := &Server{
		opts: Options{
			ID:           1,
			WriteTimeout: 5 * time.Second,
		},
		sm:       fsm,
		raftHost: fr,
		w:        fw,
		ctx:      ctx,
		cancel:   cancel,
	}

	return &testServeSuite{
		server: s,
		fr:     fr,
		fsm:    fsm,
		fw:     fw,
	}
}

func marshalPut(id uint64, key string, value string) []byte {
	req, _ := protocol.EncodeRequest(protocol.Request{
		Cmd:   protocol.CmdPut,
		Key:   []byte(key),
		Value: []byte(value),
	})
	return marshalEnvelope(id, req)
}

func assertStatePresents(t *testing.T, sm *fakeStateMachine, key string, value string) {
	t.Helper()

	actual, exists := sm.Get(key)
	require.True(t, exists, "key %q not found", key)
	require.Equal(t, []byte(value), actual, "value mismatch for key %q", key)
}

func TestApplyLoopExecutesPut_036p(t *testing.T) {
	suite := newTestServer(t)
	s := suite.server
	defer s.cancel()

	go s.run()

	id := uint64(1)
	ch := suite.fw.Register(id)
	d := marshalPut(id, "test", "foo")
	suite.fr.applyc <- toApply{data: [][]byte{d}}

	select {
	case <-ch:
		assertStatePresents(t, suite.fsm, "test", "foo")
	case <-time.After(100 * time.Millisecond):
		t.Fatal("timeout waiting for apply")
	}
}

func TestApplyLoopTriggersWaiter_036p(t *testing.T) {
	suite := newTestServer(t)
	s := suite.server
	defer s.cancel()

	go s.run()

	id := uint64(1)
	ch := suite.fw.Register(id)

	req := marshalPut(id, "k", "v")
	suite.fr.applyc <- toApply{data: [][]byte{req}}

	select {
	case v := <-ch:
		require.Nil(t, v)
	case <-time.After(100 * time.Millisecond):
		t.Fatal("timeout waiting for trigger")
	}
}

func TestApplyLoopBatchMultipleEntries_036p(t *testing.T) {
	suite := newTestServer(t)
	s := suite.server
	defer s.cancel()

	go s.run()

	id2 := uint64(2)
	ch := suite.fw.Register(id2)
	r1 := marshalPut(uint64(1), "k1", "v1")
	r2 := marshalPut(id2, "k2", "v2")
	suite.fr.applyc <- toApply{data: [][]byte{r1, r2}}

	select {
	case <-ch:
		assertStatePresents(t, suite.fsm, "k1", "v1")
		assertStatePresents(t, suite.fsm, "k2", "v2")
	case <-time.After(100 * time.Millisecond):
		t.Fatal("timeout waiting for apply")
	}
}

func TestApplyLoopSurvivesMalformedEntry_036p(t *testing.T) {
	suite := newTestServer(t)
	s := suite.server
	defer s.cancel()

	go s.run()

	suite.fr.applyc <- toApply{data: [][]byte{[]byte("malformed")}}

	id := uint64(2)
	ch := suite.fw.Register(id)
	req := marshalPut(id, "test", "foo")
	suite.fr.applyc <- toApply{data: [][]byte{req}}

	select {
	case <-ch:
		assertStatePresents(t, suite.fsm, "test", "foo")
	case <-time.After(100 * time.Millisecond):
		t.Fatal("timeout waiting for apply")
	}
}

func TestApplyLoopTriggersWaiterWithErrorOnBadPayload_036p(t *testing.T) {
	suite := newTestServer(t)
	s := suite.server
	defer s.cancel()

	go s.run()

	id := uint64(1)
	ch := suite.fw.Register(id)

	badPayload := marshalEnvelope(id, []byte("not-a-valid-protocol-request"))
	suite.fr.applyc <- toApply{data: [][]byte{badPayload}}

	select {
	case v := <-ch:
		require.Error(t, v.(error))
	case <-time.After(100 * time.Millisecond):
		t.Fatal("timeout waiting for error trigger")
	}
}

func TestApplyLoopStopsOnContextCancel_036p(t *testing.T) {
	suite := newTestServer(t)
	done := make(chan struct{})
	go func() {
		suite.server.run()
		close(done)
	}()
	suite.server.cancel()
	<-done

	select {
	case suite.fr.applyc <- toApply{data: [][]byte{[]byte("foo")}}:
		t.Fatal("expected send to block after context cancel")
	case <-time.After(50 * time.Millisecond):
		// apply loop stopped — no reader on the channel
	}
}

// ---------------------------------------------------------------------------
// 036q — The Propose Path
// ---------------------------------------------------------------------------

func newRequestContext(cmd protocol.Cmd, key, value string) *RequestContext {
	return &RequestContext{
		StreamTransport: &fakeStreamTransport{},
		Request: protocol.Request{
			Cmd:   cmd,
			Key:   []byte(key),
			Value: []byte(value),
		},
	}
}

func TestRaftPutRoundTrip_036q(t *testing.T) {
	suite := newTestServer(t)
	s := suite.server
	defer s.cancel()

	go s.run()

	ctx := newRequestContext(protocol.CmdPut, "k1", "v1")

	errc := make(chan error, 1)
	go func() {
		errc <- s.handlePut(ctx)
	}()

	proposed := <-suite.fr.proposec
	suite.fr.applyc <- toApply{data: [][]byte{proposed}}

	select {
	case err := <-errc:
		require.NoError(t, err)
	case <-time.After(100 * time.Millisecond):
		t.Fatal("timeout waiting for raftPut")
	}

	assertStatePresents(t, suite.fsm, "k1", "v1")
}

func TestRaftPutProposeError_036q(t *testing.T) {
	suite := newTestServer(t)
	s := suite.server
	defer s.cancel()

	go s.run()

	suite.fr.proposeErr = errors.New("not leader")

	ctx := newRequestContext(protocol.CmdPut, "k1", "v1")

	errc := make(chan error, 1)
	go func() {
		errc <- s.handlePut(ctx)
	}()

	select {
	case err := <-errc:
		require.ErrorContains(t, err, "not leader")
	case <-time.After(100 * time.Millisecond):
		t.Fatal("timeout waiting for raftPut")
	}
}

func TestRaftPutTimeout_036q(t *testing.T) {
	suite := newTestServer(t)
	s := suite.server

	go s.run()

	ctx := newRequestContext(protocol.CmdPut, "k1", "v1")

	errc := make(chan error, 1)
	go func() {
		errc <- s.handlePut(ctx)
	}()

	// consume the proposal but never commit it
	<-suite.fr.proposec

	// cancel server context to simulate timeout
	s.cancel()

	select {
	case err := <-errc:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(100 * time.Millisecond):
		t.Fatal("timeout waiting for raftPut")
	}
}

func TestRaftPutApplyError_036q(t *testing.T) {
	suite := newTestServer(t)
	s := suite.server
	defer s.cancel()

	suite.fsm.putErr = errors.New("disk full")

	go s.run()

	ctx := newRequestContext(protocol.CmdPut, "k1", "v1")

	errc := make(chan error, 1)
	go func() {
		errc <- s.handlePut(ctx)
	}()

	proposed := <-suite.fr.proposec
	suite.fr.applyc <- toApply{data: [][]byte{proposed}}

	select {
	case err := <-errc:
		require.ErrorContains(t, err, "disk full")
	case <-time.After(100 * time.Millisecond):
		t.Fatal("timeout waiting for raftPut")
	}
}

// ---------------------------------------------------------------------------
// 036u — The Wiring
// ---------------------------------------------------------------------------

func newListenerPair(t *testing.T) (net.Listener, net.Listener) {
	t.Helper()

	addr := "127.0.0.1:0"
	ln, err := net.Listen("tcp", addr)
	require.NoError(t, err)

	rln, err := net.Listen("tcp", addr)
	require.NoError(t, err)

	return ln, rln
}

func newTestCluster(t *testing.T) (*Server, *Server, *Server) {
	t.Helper()

	s1ln, s1rln := newListenerPair(t)
	s2ln, s2rln := newListenerPair(t)
	s3ln, s3rln := newListenerPair(t)

	s1, err := NewServer(Options{
		ID:           1,
		DataDir:      t.TempDir(),
		RaftListener: s1rln,
		Peers: []*rafttransport.PeerInfo{
			{ID: 2, Addr: s2rln.Addr().String()},
			{ID: 3, Addr: s3rln.Addr().String()},
		},
	})
	require.NoError(t, err)
	s1.ln = s1ln
	require.NoError(t, s1.Start())

	s2, err := NewServer(Options{
		ID:           2,
		DataDir:      t.TempDir(),
		RaftListener: s2rln,
		Peers: []*rafttransport.PeerInfo{
			{ID: 1, Addr: s1rln.Addr().String()},
			{ID: 3, Addr: s3rln.Addr().String()},
		},
	})
	require.NoError(t, err)
	s2.ln = s2ln
	require.NoError(t, s2.Start())

	s3, err := NewServer(Options{
		ID:           3,
		DataDir:      t.TempDir(),
		RaftListener: s3rln,
		Peers: []*rafttransport.PeerInfo{
			{ID: 1, Addr: s1rln.Addr().String()},
			{ID: 2, Addr: s2rln.Addr().String()},
		},
	})
	require.NoError(t, err)
	s3.ln = s3ln
	require.NoError(t, s3.Start())

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = s1.Shutdown(ctx)
		_ = s2.Shutdown(ctx)
		_ = s3.Shutdown(ctx)
	})

	return s1, s2, s3
}

func clusterLeader(servers ...*Server) *Server {
	for _, s := range servers {
		if s.raftHost.LeaderID() == s.opts.ID {
			return s
		}
	}
	return nil
}

func waitForLeader(t *testing.T, timeout time.Duration, servers ...*Server) *Server {
	t.Helper()
	deadline := time.After(timeout)
	for {
		if leader := clusterLeader(servers...); leader != nil {
			return leader
		}
		select {
		case <-deadline:
			t.Fatal("timeout waiting for leader election")
			return nil
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func TestZeroIDReturnsError_036u(t *testing.T) {
	_, err := NewServer(Options{
		DataDir: t.TempDir(),
		ID:      0,
	})
	require.ErrorContains(t, err, "ID cannot be zero")
}

func TestLeaderElectedWithoutManualCampaign_036u(t *testing.T) {
	s1, s2, s3 := newTestCluster(t)
	leader := waitForLeader(t, 5*time.Second, s1, s2, s3)
	require.NotNil(t, leader)
}

func TestPutAppearsInServerDB_036u(t *testing.T) {
	s1, s2, s3 := newTestCluster(t)
	leader := waitForLeader(t, 5*time.Second, s1, s2, s3)

	ctx := newRequestContext(protocol.CmdPut, "hello", "world")
	err := leader.handlePut(ctx)
	require.NoError(t, err)

	val, ok := leader.sm.Get("hello")
	require.True(t, ok, "key not found in leader state machine")
	require.Equal(t, []byte("world"), val)
}

func TestClusterShutdownCompletes_036u(t *testing.T) {
	// Proves the full lifecycle: start → elect → shutdown with no
	// deadlock, goroutine leak, or unclosed resource.
	s1, s2, s3 := newTestCluster(t)
	_ = waitForLeader(t, 5*time.Second, s1, s2, s3)

	// Shutdown is called by t.Cleanup in newTestCluster. If it hangs,
	// the test times out — which is the failure signal.
	// Explicitly shut down here to verify return values.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	require.NoError(t, s1.Shutdown(ctx))
	require.NoError(t, s2.Shutdown(ctx))
	require.NoError(t, s3.Shutdown(ctx))
}

func TestPutThenShutdownDoesNotDeadlock_036u(t *testing.T) {
	// The shutdown-drain fix: handleBatch selects on stopc when applyc
	// has no reader. This test verifies the full path: propose, commit,
	// then shut down immediately — the server must not hang.
	s1, s2, s3 := newTestCluster(t)
	leader := waitForLeader(t, 5*time.Second, s1, s2, s3)

	ctx := newRequestContext(protocol.CmdPut, "k", "v")
	require.NoError(t, leader.handlePut(ctx))

	shutCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	require.NoError(t, s1.Shutdown(shutCtx))
	require.NoError(t, s2.Shutdown(shutCtx))
	require.NoError(t, s3.Shutdown(shutCtx))
}

func TestPutAppearsInFollowerSM_036u(t *testing.T) {
	s1, s2, s3 := newTestCluster(t)
	leader := waitForLeader(t, 5*time.Second, s1, s2, s3)

	ctx := newRequestContext(protocol.CmdPut, "hello", "world")
	require.NoError(t, leader.handlePut(ctx))

	// Collect followers
	followers := make([]*Server, 0, 2)
	for _, s := range []*Server{s1, s2, s3} {
		if s != leader {
			followers = append(followers, s)
		}
	}

	// Poll until both followers have applied the entry
	deadline := time.After(5 * time.Second)
	for _, f := range followers {
		for {
			val, ok := f.sm.Get("hello")
			if ok {
				require.Equal(t, []byte("world"), val)
				break
			}
			select {
			case <-deadline:
				t.Fatalf("follower %d did not apply entry", f.opts.ID)
			case <-time.After(50 * time.Millisecond):
			}
		}
	}
}

// ---------------------------------------------------------------------------
// 037c — MsgProp Forwarding Integration
// ---------------------------------------------------------------------------

func TestPutOnFollowerCommitsViaForwarding_037c(t *testing.T) {
	s1, s2, s3 := newTestCluster(t)
	leader := waitForLeader(t, 5*time.Second, s1, s2, s3)

	// Pick a follower.
	var follower *Server
	for _, s := range []*Server{s1, s2, s3} {
		if s != leader {
			follower = s
			break
		}
	}
	require.NotNil(t, follower)

	// Send PUT to the follower. Raft forwards MsgProp to leader internally.
	ctx := newRequestContext(protocol.CmdPut, "fwd-key", "fwd-val")
	err := follower.handlePut(ctx)
	require.NoError(t, err)

	// The entry should appear in the follower's own state machine.
	val, ok := follower.sm.Get("fwd-key")
	require.True(t, ok, "key not found in follower SM after forwarded PUT")
	require.Equal(t, []byte("fwd-val"), val)
}
