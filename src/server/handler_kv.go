package server

import (
	"context"
	"kvgo/protocol"
)

// ---------------------------------------------------------------------------
// GET Request Handler
// ---------------------------------------------------------------------------

func (s *Server) handleGet(ctx *RequestContext) error {
	key := string(ctx.Request.Key)
	val, ok := s.sm.Get(key)

	if !ok {
		return s.writeResponse(ctx.StreamTransport, protocol.Response{Status: protocol.StatusNotFound})
	}
	copyVal := append([]byte(nil), val...)
	return s.writeResponse(ctx.StreamTransport, protocol.Response{Status: protocol.StatusOK, Value: copyVal})
}

// ---------------------------------------------------------------------------
// PUT Request Handler
// ---------------------------------------------------------------------------

func (s *Server) handlePut(ctx *RequestContext) error {
	if err := s.proposePut(string(ctx.Request.Key), ctx.Request.Value); err != nil {
		return err
	}
	return s.responseWithStatus(ctx, protocol.StatusOK)
}

// proposePut proposes a PUT through Raft and blocks until the entry is
// committed and applied, or the write timeout expires. Shared by the binary
// protocol handler and the HTTP handler.
func (s *Server) proposePut(key string, value []byte) error {
	id := s.nextRequestID()
	ch := s.w.Register(id)

	ctx, cancel := context.WithTimeout(s.ctx, s.opts.WriteTimeout)
	defer cancel()

	req := protocol.Request{Cmd: protocol.CmdPut, Key: []byte(key), Value: value}
	encoded, err := protocol.EncodeRequest(req)
	if err != nil {
		s.w.Trigger(id, err)
	} else {
		data := marshalEnvelope(id, encoded)
		if err := s.raftHost.Propose(ctx, data); err != nil {
			s.w.Trigger(id, err)
		}
	}

	select {
	case result := <-ch:
		if result != nil {
			return result.(error)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ---------------------------------------------------------------------------
// Helper Functions
// ---------------------------------------------------------------------------

func (s *Server) responseStatusError(ctx *RequestContext) error {
	return s.responseWithStatus(ctx, protocol.StatusError)
}

func (s *Server) responseStatusOk(ctx *RequestContext) error {
	return s.responseWithStatus(ctx, protocol.StatusOK)
}

func (s *Server) responseWithStatus(ctx *RequestContext, status protocol.Status) error {
	return s.writeResponse(ctx.StreamTransport, protocol.Response{Status: status})
}
