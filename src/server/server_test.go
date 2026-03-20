package server

import (
	"context"
	"errors"
	"kvgo/protocol"
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

	s, err := NewServer(Options{
		DataDir: t.TempDir(),
	})
	require.NoError(t, err)

	fsm := newFakeStateMachine()
	fr := newFakeRaftHost()
	fw := newFakeWait()

	s.sm, s.raftHost, s.w = fsm, fr, fw
	s.ctx, s.cancel = context.WithCancel(context.Background())

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
		errc <- s.raftPut(ctx)
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
		errc <- s.raftPut(ctx)
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
		errc <- s.raftPut(ctx)
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
		errc <- s.raftPut(ctx)
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
