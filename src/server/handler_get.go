package server

import (
	"kvgo/protocol"
	"time"
)

func (s *Server) handleGet(ctx *RequestContext) error {
	req, err := protocol.DecodeRequest(ctx.Payload)
	if err != nil {
		return err
	}

	if s.isStaleness() {
		return s.responseStatusWithPrimaryAddress(ctx, protocol.StatusReplicaTooStale)
	}

	if req.WaitForSeq > 0 {
		return s.doStrongGet(ctx, &req)
	}

	return s.doGet(ctx, &req)
}

func (s *Server) isStaleness() bool {
	if !s.isReplica {
		return false
	}

	seqLag := s.primarySeq - s.lastSeq.Load()
	heartbeatAge := time.Since(s.lastHeartbeat)
	return seqLag > uint64(s.opts.ReplicaStaleLag) || heartbeatAge > s.opts.ReplicaStaleHeartbeat
}

func (s *Server) doGet(ctx *RequestContext, req *protocol.Request) error {
	key := string(req.Key)
	val, ok := s.db.Get(key)

	var resp protocol.Response
	if !ok {
		resp = protocol.Response{Status: protocol.StatusNotFound, Seq: s.lastSeq.Load()}
	} else {
		// Avoid aliasing engine memory.
		copyVal := append([]byte(nil), val...)
		resp = protocol.Response{Status: protocol.StatusOK, Value: copyVal, Seq: s.lastSeq.Load()}
	}

	return s.writeResponse(ctx.Framer, resp)
}

func (s *Server) doStrongGet(ctx *RequestContext, req *protocol.Request) error {
	maxDuration := 10 * time.Millisecond
	currentDuration := time.Millisecond

	startedAt := time.Now()
	for time.Since(startedAt) < s.opts.StrongReadTimeout {

		if s.lastSeq.Load() >= req.WaitForSeq {
			return s.doGet(ctx, req)
		}

		time.Sleep(currentDuration)
		currentDuration = min(currentDuration*2, maxDuration)
	}

	// Timeout: replica didn't catch up within StrongReadTimeout window
	s.log().Warn("strong read timeout: replica lagging, redirecting to primary",
		"requested_seq", req.WaitForSeq,
		"current_seq", s.lastSeq.Load(),
		"elapsed", time.Since(startedAt),
		"timeout", s.opts.StrongReadTimeout)

	return s.responseStatusWithPrimaryAddress(ctx, protocol.StatusReadOnly)
}

func (s *Server) responseStatusWithPrimaryAddress(ctx *RequestContext, status protocol.Status) error {
	return s.writeResponse(ctx.Framer, protocol.Response{
		Status: status,
		Value:  []byte(s.opts.ReplicaOf), // Primary address for client redirect
	})
}
