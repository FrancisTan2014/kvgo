package server

import (
	"kvgo/protocol"
	"kvgo/transport"
	"time"
)

type HandlerFunc func(*Server, *RequestContext) error

type RequestContext struct {
	StreamTransport  transport.StreamTransport
	RequestTransport transport.RequestTransport // Same object as StreamTransport (MultiplexedTransport)
	Request          protocol.Request           // Decoded request (replaces raw Payload)
	takenOver        bool                       // If true, connection ownership transferred to handler; caller should not close connection
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
	s.requestHandlers[protocol.CmdTopology] = (*Server).handleTopology
	s.requestHandlers[protocol.CmdPeerHandshake] = (*Server).handlePeerHandshake
}

func (s *Server) handleRequest(t transport.StreamTransport, timeout time.Duration) (takenOver bool) {
	for {
		var payload []byte
		var err error
		if timeout > 0 {
			payload, err = t.ReceiveWithTimeout(timeout)
		} else {
			payload, err = t.Receive()
		}
		if err != nil {
			return false
		}

		req, err := protocol.DecodeRequest(payload)
		if err != nil {
			s.log().Error("failed to decode request", "error", err)
			return false
		}

		handler := s.requestHandlers[req.Cmd]
		if handler == nil {
			s.log().Error("unsupported request detected", "cmd", req.Cmd)
			return false
		}

		ctx := &RequestContext{
			StreamTransport:  t,
			RequestTransport: transport.AsRequestTransport(t, s.opts.ReadTimeout),
			Request:          req,
		}

		if err := handler(s, ctx); err != nil {
			s.log().Error("failed to process the request", "cmd", req.Cmd, "error", err)
			return false
		}

		if ctx.takenOver {
			return true // Handler owns connection now, exit loop
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
