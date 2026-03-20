package server

import (
	"context"
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

	d := marshalPut(uint64(1), "test", "foo")
	suite.fr.applyc <- toApply{data: [][]byte{d}}

	assertStatePresents(t, suite.fsm, "test", "foo")
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

	r1 := marshalPut(uint64(1), "k1", "v1")
	r2 := marshalPut(uint64(2), "k2", "v2")
	suite.fr.applyc <- toApply{data: [][]byte{r1, r2}}

	assertStatePresents(t, suite.fsm, "k1", "v1")
	assertStatePresents(t, suite.fsm, "k2", "v2")
}

func TestApplyLoopSurvivesMalformedEntry_036p(t *testing.T) {
	suite := newTestServer(t)
	s := suite.server
	defer s.cancel()

	go s.run()

	suite.fr.applyc <- toApply{data: [][]byte{[]byte("malformed")}}

	req := marshalPut(uint64(2), "test", "foo")
	suite.fr.applyc <- toApply{data: [][]byte{req}}

	assertStatePresents(t, suite.fsm, "test", "foo")
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
