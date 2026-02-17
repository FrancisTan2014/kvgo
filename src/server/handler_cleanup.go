package server

import (
	"kvgo/protocol"
	"time"
)

// handleCleanup performs cleanup of orphaned value files.
// This is an operator-triggered maintenance command (not automatic).
//
// During cleanup, the shard is locked and writes are REJECTED.
// This is a synchronous, blocking operation per shard.
func (s *Server) handleCleanup(ctx *RequestContext) error {
	if s.cleanupInProgress.Load() {
		s.log().Warn("cleanup already in progress, rejecting new CLEANUP request",
			"remote_addr", ctx.StreamTransport.RemoteAddr(),
			"action", "rejected",
		)
		return s.writeResponse(ctx.StreamTransport, protocol.Response{Status: protocol.StatusCleaning})
	}

	s.log().Info("starting value file cleanup",
		"remote_addr", ctx.StreamTransport.RemoteAddr(),
		"action", "cleanup_initiated",
		"data_dir", s.opts.DataDir,
	)

	// Return immediately and run cleanup asynchronously to free connection resources.
	// Cleanup can take minutes for large databases, don't block the handler.
	go s.StartCleanup()
	return s.responseStatusOk(ctx)
}

func (s *Server) StartCleanup() {
	s.cleanupInProgress.Store(true)
	defer s.cleanupInProgress.Store(false)

	startTime := time.Now()

	err := s.db.Clean()

	elapsed := time.Since(startTime)

	if err == nil {
		s.log().Info("value file cleanup completed successfully",
			"elapsed", elapsed,
			"action", "cleanup_completed",
		)
	} else {
		s.log().Error("value file cleanup failed",
			"error", err,
			"elapsed", elapsed,
			"action", "cleanup_failed",
		)
	}
}
