package server

import (
	"kvgo/protocol"
)

func (s *Server) handleGet(ctx *RequestContext) error {
	req, err := protocol.DecodeRequest(ctx.Payload)
	if err != nil {
		return err
	}

	key := string(req.Key)
	val, ok := s.db.Get(key)
	if err != nil {
		return err
	}

	var resp protocol.Response
	if !ok {
		resp = protocol.Response{Status: protocol.StatusNotFound}
	} else {
		// Avoid aliasing engine memory.
		copyVal := append([]byte(nil), val...)
		resp = protocol.Response{Status: protocol.StatusOK, Value: copyVal}
	}

	return s.writeResponse(ctx.Framer, resp)
}
