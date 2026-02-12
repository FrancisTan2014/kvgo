package server

import (
	"kvgo/protocol"
	"kvgo/transport"
)

type HandlerFunc func(*Server, *RequestContext) error

type RequestContext struct {
	Transport transport.StreamTransport
	Request   protocol.Request // Decoded request (replaces raw Payload)
	takenOver bool
}

func (s *Server) registerRequestHandlers() {
	s.requestHandlers[protocol.CmdGet] = (*Server).handleGet
	s.requestHandlers[protocol.CmdPut] = (*Server).handlePut
	s.requestHandlers[protocol.CmdReplicate] = (*Server).handleReplicate
	s.requestHandlers[protocol.CmdPing] = (*Server).handlePing
	s.requestHandlers[protocol.CmdPong] = (*Server).handlePong
	s.requestHandlers[protocol.CmdPromote] = (*Server).handlePromote
	s.requestHandlers[protocol.CmdReplicaOf] = (*Server).handleReplicaOf
	s.requestHandlers[protocol.CmdCleanup] = (*Server).handleCleanup
	s.requestHandlers[protocol.CmdAck] = (*Server).handleAck
	s.requestHandlers[protocol.CmdNack] = (*Server).handleNack
}

func (s *Server) handleRequest(t transport.StreamTransport) {
	for {
		var payload []byte
		var err error
		if s.opts.ReadTimeout > 0 {
			payload, err = t.ReceiveWithTimeout(s.opts.ReadTimeout)
		} else {
			payload, err = t.Receive()
		}
		if err != nil {
			return
		}

		req, err := protocol.DecodeRequest(payload)
		if err != nil {
			s.log().Error("failed to decode request", "error", err)
			return
		}

		handler := s.requestHandlers[req.Cmd]
		if handler == nil {
			s.log().Error("unsupported request detected", "cmd", req.Cmd)
			return
		}

		ctx := &RequestContext{
			Transport: t,
			Request:   req,
		}

		if err := handler(s, ctx); err != nil {
			s.log().Error("failed to process the request", "cmd", req.Cmd, "error", err)
			return
		}

		if ctx.takenOver {
			return // Handler owns connection now, exit loop
		}
	}
}

func (s *Server) writeResponse(t transport.StreamTransport, resp protocol.Response) error {
	payload, err := protocol.EncodeResponse(resp)
	if err != nil {
		return err
	}
	if s.opts.WriteTimeout > 0 {
		return t.SendWithTimeout(payload, s.opts.WriteTimeout)
	}
	return t.Send(payload)
}
