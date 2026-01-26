package server

import (
	"kvgo/protocol"
	"net"
)

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

		req, err := protocol.DecodeRequest(payload)
		if err != nil {
			// Protocol error: reply with StatusError and close.
			_ = s.writeResponse(f, protocol.Response{Status: protocol.StatusError})
			return
		}

		resp := s.handle(req)
		if err := s.writeResponse(f, resp); err != nil {
			return
		}
	}
}

func (s *Server) handle(req protocol.Request) protocol.Response {
	key := string(req.Key)
	switch req.Op {
	case protocol.OpGet:
		val, ok := s.db.Get(key)
		if !ok {
			s.logf("GET %q -> not found", key)
			return protocol.Response{Status: protocol.StatusNotFound}
		}
		s.logf("GET %q -> %d bytes", key, len(val))
		// Avoid aliasing engine memory.
		copyVal := append([]byte(nil), val...)
		return protocol.Response{Status: protocol.StatusOK, Value: copyVal}
	case protocol.OpPut:
		if err := s.db.Put(key, req.Value); err != nil {
			s.logf("PUT %q -> error: %v", key, err)
			return protocol.Response{Status: protocol.StatusError}
		}
		s.logf("PUT %q <- %d bytes", key, len(req.Value))
		return protocol.Response{Status: protocol.StatusOK}
	default:
		s.logf("unknown op %d", req.Op)
		return protocol.Response{Status: protocol.StatusError}
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
