package server

import (
	"kvgo/protocol"
	"net"
)

type HandlerFunc func(*Server, *RequestContext) error

type RequestContext struct {
	Conn      net.Conn
	Framer    *protocol.Framer
	Payload   []byte
	takenOver bool
}

func (s *Server) registerRequestHandlers() {
	s.requestHandlers[protocol.OpGet] = (*Server).handleGet
	s.requestHandlers[protocol.OpPut] = (*Server).handlePut
	s.requestHandlers[protocol.OpReplicate] = (*Server).handleReplicate
	s.requestHandlers[protocol.OpPing] = (*Server).handlePing
	s.requestHandlers[protocol.OpPromote] = (*Server).handlePromote
	s.requestHandlers[protocol.OpReplicaOf] = (*Server).handleReplicaOf
}

func (s *Server) handleRequest(conn net.Conn) {
	f := protocol.NewConnFramer(conn)
	f.SetMaxPayload(s.opts.MaxFrameSize)

	for {
		var payload []byte
		var err error
		if s.opts.ReadTimeout > 0 {
			payload, err = f.ReadWithTimeout(s.opts.ReadTimeout)
		} else {
			payload, err = f.Read()
		}
		if err != nil {
			return
		}

		op := payload[0]
		handler := s.requestHandlers[protocol.Op(op)]
		if handler == nil {
			s.log().Error("unsupported request detected", "op", op)
			return
		}

		ctx := &RequestContext{
			Conn:    conn,
			Framer:  f,
			Payload: payload,
		}

		if err := handler(s, ctx); err != nil {
			s.log().Error("failed to process the request", "op", op, "error", err)
			return
		}

		if ctx.takenOver {
			return // Handler owns connection now, exit loop
		}
	}
}

func (s *Server) writeResponse(f *protocol.Framer, resp protocol.Response) error {
	payload, err := protocol.EncodeResponse(resp)
	if err != nil {
		return err
	}
	if s.opts.WriteTimeout > 0 {
		return f.WriteWithTimeout(payload, s.opts.WriteTimeout)
	}
	return f.Write(payload)
}
