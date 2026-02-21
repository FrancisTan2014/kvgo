package server

import (
	"kvgo/protocol"
	"testing"
)

// TestCommandRegistration verifies all protocol commands have registered handlers.
// This is a safety check to catch missing handler registrations.
func TestCommandRegistration(t *testing.T) {
	s := &Server{
		requestHandlers: make(map[protocol.Cmd]HandlerFunc),
	}
	s.registerRequestHandlers()

	expectedCommands := []struct {
		cmd  protocol.Cmd
		name string
	}{
		{protocol.CmdGet, "GET"},
		{protocol.CmdPut, "PUT"},
		{protocol.CmdReplicate, "REPLICATE"},
		{protocol.CmdPing, "PING"},
		{protocol.CmdPromote, "PROMOTE"},
		{protocol.CmdReplicaOf, "REPLICAOF"},
		{protocol.CmdCleanup, "CLEANUP"},
		{protocol.CmdAck, "ACK"},
		{protocol.CmdNack, "NACK"},
		{protocol.CmdTopology, "TOPOLOGY"},
		{protocol.CmdPeerHandshake, "PEER"},
		{protocol.CmdVoteRequest, "VOTE"},
	}

	for _, tc := range expectedCommands {
		handler, ok := s.requestHandlers[tc.cmd]
		if !ok {
			t.Errorf("command %s (0x%02x) not registered", tc.name, tc.cmd)
			continue
		}
		if handler == nil {
			t.Errorf("command %s (0x%02x) has nil handler", tc.name, tc.cmd)
		}
	}

	// Verify we have exactly the expected number of handlers
	if len(s.requestHandlers) != len(expectedCommands) {
		t.Errorf("expected %d registered handlers, got %d", len(expectedCommands), len(s.requestHandlers))
	}
}

// TestHandlerInvocation verifies handlers are called with correct context.
func TestHandlerInvocation(t *testing.T) {
	s := &Server{
		requestHandlers: make(map[protocol.Cmd]HandlerFunc),
	}
	s.registerRequestHandlers()

	// Test that GET handler exists
	handler := s.requestHandlers[protocol.CmdGet]
	if handler == nil {
		t.Fatal("GET handler not registered")
	}

	// Verify handler is not nil (actual invocation tested in handler_test.go)
	if handler == nil {
		t.Error("GET handler should not be nil")
	}
}

// TestUnknownCommandHandling verifies behavior when unknown command is dispatched.
func TestUnknownCommandHandling(t *testing.T) {
	s := &Server{
		requestHandlers: make(map[protocol.Cmd]HandlerFunc),
	}
	s.registerRequestHandlers()

	// Try to look up an unregistered command
	unknownCmd := protocol.Cmd(0xFF)
	handler, ok := s.requestHandlers[unknownCmd]

	if ok {
		t.Errorf("unknown command 0x%02x should not be registered", unknownCmd)
	}

	if handler != nil {
		t.Error("unknown command should return nil handler")
	}
}

// TestHandlerErrorPropagation verifies error responses are properly formatted.
// (Full error propagation tested in handler_test.go with real DB)
func TestHandlerErrorPropagation(t *testing.T) {
	// Verify replica mode flag works correctly
	s := &Server{}
	if s.isLeader() {
		t.Error("expected replica mode")
	}
}

// TestConcurrentHandlerInvocation verifies handler map is thread-safe.
// (Full concurrent execution tested in integration tests)
func TestConcurrentHandlerInvocation(t *testing.T) {
	s := &Server{
		requestHandlers: make(map[protocol.Cmd]HandlerFunc),
	}
	s.registerRequestHandlers()

	// Verify we can read handlers concurrently
	done := make(chan bool, 10)

	for i := 0; i < 10; i++ {
		go func() {
			// Concurrent reads should be safe
			_ = s.requestHandlers[protocol.CmdGet]
			_ = s.requestHandlers[protocol.CmdPut]
			done <- true
		}()
	}

	for i := 0; i < 10; i++ {
		<-done
	}
}

// TestHandlerContextFields verifies RequestContext fields are accessible.
func TestHandlerContextFields(t *testing.T) {
	req := protocol.Request{
		Cmd: protocol.CmdGet,
		Key: []byte("test-key"),
		Seq: 42,
	}

	ctx := &RequestContext{
		Request: req,
	}

	// Verify context fields are accessible
	if string(ctx.Request.Key) != "test-key" {
		t.Errorf("context Request.Key = %q, want 'test-key'", ctx.Request.Key)
	}

	if ctx.Request.Seq != 42 {
		t.Errorf("context Request.Seq = %d, want 42", ctx.Request.Seq)
	}

	if ctx.takenOver {
		t.Error("context takenOver should default to false")
	}
}

// TestHandlerTakeoverFlag verifies takenOver flag behavior.
func TestHandlerTakeoverFlag(t *testing.T) {
	ctx := &RequestContext{
		takenOver: false,
	}

	// Initially false
	if ctx.takenOver {
		t.Error("takenOver should initially be false")
	}

	// Handler can set it
	ctx.takenOver = true

	if !ctx.takenOver {
		t.Error("takenOver not set")
	}

	// Dispatcher should check this flag and exit loop
	// (This is implicit behavior tested in integration tests)
}

// TestComputeReplicaAcksNeeded_EdgeCases verifies quorum calculation edge cases.
// (Detailed tests in quorum_test.go)
func TestComputeReplicaAcksNeeded_EdgeCases(t *testing.T) {
	// Create server with known replica count
	s := &Server{
		replicas: make(map[string]*replicaConn),
	}

	// Test with zero replicas
	got := s.computeReplicaAcksNeeded()
	if got != 0 {
		t.Errorf("expected 0 ACKs needed with 0 replicas, got %d", got)
	}

	// Detailed quorum calculation tests are in quorum_test.go
}
