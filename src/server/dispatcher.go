package server

import (
	"context"
	"kvgo/protocol"
	"kvgo/transport"
	"time"
)

type HandlerFunc func(*Server, *RequestContext) error

type RequestContext struct {
	StreamTransport transport.StreamTransport
	Request         protocol.Request // Decoded request (replaces raw Payload)
}

func (s *Server) registerRequestHandlers() {
	s.requestHandlers[protocol.CmdGet] = (*Server).handleGet
	s.requestHandlers[protocol.CmdPut] = (*Server).handlePut
}

func (s *Server) handleRequest(t transport.StreamTransport, timeout time.Duration) {
	for {
		readCtx := context.Background()
		if timeout > 0 {
			var cancel context.CancelFunc
			readCtx, cancel = context.WithTimeout(readCtx, timeout)
			defer cancel() // OK: each cancel is idempotent; all run on function exit
		}
		payload, err := t.Receive(readCtx)
		if err != nil {
			return
		}

		req, err := protocol.DecodeRequest(payload)
		if err != nil {
			s.log().Warn("failed to decode request", "error", err)
			if writeErr := s.writeResponse(t, protocol.Response{Status: protocol.StatusError}); writeErr != nil {
				return
			}
			continue
		}

		handler := s.requestHandlers[req.Cmd]
		if handler == nil {
			s.log().Warn("unsupported request detected", "cmd", req.Cmd)
			if writeErr := s.writeResponse(t, protocol.Response{Status: protocol.StatusError}); writeErr != nil {
				return
			}
			continue
		}

		ctx := &RequestContext{
			StreamTransport: t,
			Request:         req,
		}

		if err := handler(s, ctx); err != nil {
			s.log().Warn("request failed", "cmd", req.Cmd, "error", err)
			// Send error response instead of killing connection.
			// Only close on transport write failure (connection dead).
			if writeErr := s.writeResponse(t, protocol.Response{Status: protocol.StatusError}); writeErr != nil {
				return
			}
		}
	}
}

func (s *Server) writeResponse(t transport.StreamTransport, resp protocol.Response) error {
	payload, err := protocol.EncodeResponse(resp)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), s.opts.WriteTimeout)
	defer cancel()
	return t.Send(ctx, payload)
}
