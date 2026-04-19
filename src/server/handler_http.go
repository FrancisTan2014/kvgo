package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// httpMux returns an HTTP handler that exposes a key-value API for external
// testing tools (e.g. Jepsen). Reads go through the local state machine;
// writes go through Raft propose-wait-apply — the same path as CmdPut.
func (s *Server) httpMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /kv/{key}", s.httpGet)
	mux.HandleFunc("PUT /kv/{key}", s.httpPut)
	return mux
}

func (s *Server) httpGet(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	if err := s.proposeRead(); err != nil {
		s.log().Warn("read failed", "err", err, "key", key)
		if errors.Is(err, context.DeadlineExceeded) {
			http.Error(w, "read timeout", http.StatusGatewayTimeout)
			return
		}
		http.Error(w, fmt.Sprintf("read failed: %v", err), http.StatusServiceUnavailable)
		return
	}
	val, ok := s.sm.Get(key)
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(val)
}

func (s *Server) httpPut(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")

	// Limit body size to max frame size to prevent unbounded reads.
	body := http.MaxBytesReader(w, r.Body, int64(s.opts.MaxFrameSize))
	value, err := io.ReadAll(body)
	if err != nil {
		http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
		return
	}

	if err := s.proposePut(key, value); err != nil {
		http.Error(w, fmt.Sprintf("write failed: %v", err), http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
}
