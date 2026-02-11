package server

import (
	"kvgo/protocol"
	"kvgo/utils"
)

// ---------------------------------------------------------------------------
// REPLICATE Command Handler (Primary receives from replica)
// ---------------------------------------------------------------------------

// handleReplicate handles the REPLICATE command from a replica wanting to sync.
// This handler takes over the connection - serveReplica blocks until disconnection.
func (s *Server) handleReplicate(ctx *RequestContext) error {
	if s.isReplica {
		s.log().Warn("REPLICATE rejected: node is replica")
		return s.responseStatusError(ctx)
	}

	req := &ctx.Request

	replid := string(req.Value)
	rc := newReplicaConn(ctx.Conn, req.Seq, replid)
	s.mu.Lock()
	s.replicas[ctx.Conn] = rc
	s.mu.Unlock()

	// Mark connection as taken over BEFORE blocking call.
	// serveReplica will handle all communication until replica disconnects.
	ctx.takenOver = true

	// serveReplica blocks until replica disconnects.
	// Connection cleanup happens inside serveReplica's defer.
	s.serveReplica(rc)

	return nil
}

// ---------------------------------------------------------------------------
// REPLICAOF Command Handler (Client initiates replication change)
// ---------------------------------------------------------------------------

// handleReplicaOf handles the REPLICAOF command to dynamically change replication target.
func (s *Server) handleReplicaOf(ctx *RequestContext) error {
	req := &ctx.Request

	if err := s.relocate(string(req.Value)); err != nil {
		s.log().Error("REPLICAOF failed", "error", err)
		return s.responseStatusError(ctx)
	}
	return s.writeResponse(ctx.Framer, protocol.Response{Status: protocol.StatusOK})
}

func (s *Server) relocate(primaryAddr string) error {
	s.log().Info("switching primary", "address", primaryAddr)

	// Update config immediately
	s.mu.Lock()
	s.isReplica = true
	s.opts.ReplicaOf = primaryAddr
	s.mu.Unlock()

	// Do cleanup async to avoid blocking client response
	go func() {
		s.mu.Lock()
		// Cancel old replication loop
		if s.replCancel != nil {
			s.replCancel()
		}

		for c, r := range s.replicas {
			close(r.sendCh)
			_ = r.conn.Close()
			delete(s.replicas, c)
		}

		if s.primary != nil {
			_ = s.primary.Close()
			s.primary = nil
		}

		if s.backlogCancel != nil {
			s.backlogCancel()
		}
		s.mu.Unlock()

		s.db.Clear()
		s.startReplicationLoop()
	}()

	return nil
}

// ---------------------------------------------------------------------------
// PROMOTE Command Handler (Replica promotes itself to primary)
// ---------------------------------------------------------------------------

func (s *Server) handlePromote(ctx *RequestContext) error {
	if !s.isReplica {
		s.log().Warn("PROMOTE rejected: already primary")
		return s.responseStatusError(ctx)
	}

	if err := s.promote(); err != nil {
		s.log().Error("PROMOTE failed", "error", err)
		return s.responseStatusError(ctx)
	}

	s.log().Info("promoted to primary")
	return s.writeResponse(ctx.Framer, protocol.Response{Status: protocol.StatusOK})
}

func (s *Server) promote() error {
	s.isReplica = false

	// Generate new replid — this node is starting a new timeline
	s.replid = utils.GenerateUniqueID()
	s.seq.Store(0)

	if s.replCancel != nil {
		s.replCancel() // signal loop to exit
	}
	if s.primary != nil {
		_ = s.primary.Close()
	}

	// Start backlog trimmer for new primary role
	s.startBacklogTrimmer()

	if err := s.storeState(); err != nil {
		s.log().Error("failed to store new replid after promotion", "error", err)
	}

	return nil
}
