package server

import (
	"context"
	"fmt"
	"io"
	"kvgo/protocol"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// 037j — The Jepsen (Bridge): HTTP API Tests
// ---------------------------------------------------------------------------

// newTestServerWithHTTP creates a unit-test server with fakes and starts an
// HTTP listener on an ephemeral port. Returns the base URL and a cleanup func.
func newTestServerWithHTTP(t *testing.T) (*testServeSuite, string) {
	t.Helper()
	suite := newTestServer(t)
	s := suite.server

	// newTestServer skips applyDefaults; set MaxFrameSize for HTTP body limit.
	if s.opts.MaxFrameSize <= 0 {
		s.opts.MaxFrameSize = defaultMaxFrameSize
	}

	// Start HTTP on ephemeral port.
	mux := s.httpMux()
	srv := &http.Server{Handler: mux}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	go srv.Serve(ln)
	t.Cleanup(func() {
		_ = srv.Close()
	})

	base := fmt.Sprintf("http://%s", ln.Addr().String())
	return suite, base
}

func TestHTTPGetReturnsValueAfterPut_037j(t *testing.T) {
	suite, base := newTestServerWithHTTP(t)
	defer suite.server.cancel()

	// Seed state machine directly — this tests the HTTP GET path, not Raft.
	suite.fsm.data["hello"] = []byte("world")

	resp, err := http.Get(base + "/kv/hello")
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusOK, resp.StatusCode)
	body, _ := io.ReadAll(resp.Body)
	require.Equal(t, "world", string(body))
}

func TestHTTPGetReturns404ForMissingKey_037j(t *testing.T) {
	suite, base := newTestServerWithHTTP(t)
	defer suite.server.cancel()

	resp, err := http.Get(base + "/kv/nonexistent")
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestHTTPPutReturns200AfterRaftCommit_037j(t *testing.T) {
	suite, base := newTestServerWithHTTP(t)
	s := suite.server
	defer s.cancel()

	// Start the apply loop so committed entries get processed.
	go s.run()

	// PUT via HTTP — will propose to Raft and block.
	errc := make(chan *http.Response, 1)
	go func() {
		resp, err := http.NewRequest("PUT", base+"/kv/mykey", strings.NewReader("myvalue"))
		if err != nil {
			return
		}
		r, _ := http.DefaultClient.Do(resp)
		errc <- r
	}()

	// Wait for the proposal to arrive, then commit it.
	proposed := <-suite.fr.proposec
	suite.fr.applyc <- toApply{data: [][]byte{proposed}}

	select {
	case resp := <-errc:
		require.NotNil(t, resp)
		defer resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode)
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for HTTP PUT response")
	}

	// Verify the value landed in the state machine.
	assertStatePresents(t, suite.fsm, "mykey", "myvalue")
}

func TestProposePutSharedPathBinaryAndHTTP_037j(t *testing.T) {
	suite := newTestServer(t)
	s := suite.server
	defer s.cancel()

	go s.run()

	// PUT via the binary protocol handler.
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
	case <-time.After(time.Second):
		t.Fatal("timeout")
	}
	assertStatePresents(t, suite.fsm, "k1", "v1")

	// PUT via proposePut directly (same path HTTP handler uses).
	errc2 := make(chan error, 1)
	go func() {
		errc2 <- s.proposePut("k2", []byte("v2"))
	}()

	proposed2 := <-suite.fr.proposec
	suite.fr.applyc <- toApply{data: [][]byte{proposed2}}

	select {
	case err := <-errc2:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("timeout")
	}
	assertStatePresents(t, suite.fsm, "k2", "v2")
}

// ---------------------------------------------------------------------------
// Integration: HTTP against a real 3-node cluster
// ---------------------------------------------------------------------------

func TestHTTPPutGetIntegration_037j(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	s1, s2, s3 := newTestCluster(t)
	leader := waitForLeader(t, 5*time.Second, s1, s2, s3)

	// Start HTTP on the leader with an ephemeral port.
	mux := leader.httpMux()
	srv := &http.Server{Handler: mux}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	go srv.Serve(ln)
	t.Cleanup(func() { _ = srv.Close() })

	base := fmt.Sprintf("http://%s", ln.Addr().String())

	// PUT via HTTP
	req, err := http.NewRequest("PUT", base+"/kv/integration", strings.NewReader("test-value"))
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req = req.WithContext(ctx)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	// GET via HTTP
	resp2, err := http.Get(base + "/kv/integration")
	require.NoError(t, err)
	defer resp2.Body.Close()

	require.Equal(t, http.StatusOK, resp2.StatusCode)
	body, _ := io.ReadAll(resp2.Body)
	require.Equal(t, "test-value", string(body))
}
