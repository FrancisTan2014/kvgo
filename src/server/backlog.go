package server

import (
	"context"
	"time"
)

type backlogEntry struct {
	size    int
	seq     uint64
	payload []byte
}

func (s *Server) appendBacklog(entry backlogEntry) {
	s.backlogMu.Lock()
	s.backlog.PushBack(entry)
	s.backlogMu.Unlock()
	s.backlogSize.Add(int64(entry.size))
}

func (s *Server) existsInBacklog(seq uint64) bool {
	s.backlogMu.RLock()
	defer s.backlogMu.RUnlock()

	if s.backlog.Len() == 0 {
		return false
	}

	front := s.backlog.Front().Value.(backlogEntry)
	if seq < front.seq {
		return false
	}

	offset := int(seq - front.seq)
	if offset >= s.backlog.Len() {
		return false
	}

	return true
}

func (s *Server) forwardBacklog(seq uint64, write func(e backlogEntry) error) error {
	// Collect entries under lock, write outside to avoid blocking trimmer
	var entries []backlogEntry

	s.backlogMu.RLock()
	if s.backlog.Len() == 0 {
		s.backlogMu.RUnlock()
		return nil
	}

	// Find starting position
	front := s.backlog.Front().Value.(backlogEntry)
	if seq < front.seq {
		seq = front.seq // Start from beginning if requested seq was trimmed
	}

	offset := int(seq - front.seq)
	if offset >= s.backlog.Len() {
		s.backlogMu.RUnlock()
		return nil // Nothing to forward
	}

	// Walk to starting position
	node := s.backlog.Front()
	for i := 0; i < offset && node != nil; i++ {
		node = node.Next()
	}

	// Collect entries
	for node != nil {
		entries = append(entries, node.Value.(backlogEntry))
		node = node.Next()
	}
	s.backlogMu.RUnlock()

	// Write outside lock
	for _, entry := range entries {
		if err := write(entry); err != nil {
			return err
		}
	}

	return nil
}

func (s *Server) startBacklogTrimmer() {
	// Cancel any existing loop
	if s.backlogCancel != nil {
		s.backlogCancel()
	}

	// Derived from server context; cancelled on shutdown or role change
	s.backlogCtx, s.backlogCancel = context.WithCancel(s.ctx)
	ctx := s.backlogCtx

	trimDuration := s.opts.BacklogTrimDuration
	if trimDuration < time.Millisecond {
		s.log().Warn("backlog trim duration is too short, using default", "configured", trimDuration, "default", DefaultBacklogTrimDuration)
		trimDuration = DefaultBacklogTrimDuration
	}

	if s.opts.BacklogSizeLimit < MinBacklogSize {
		s.log().Warn("backlog size limit is too small, using default", "configured", s.opts.BacklogSizeLimit, "default", DefaultBacklogSizeLimit)
		s.opts.BacklogSizeLimit = DefaultBacklogSizeLimit
	}

	trimmerTicker := time.NewTicker(trimDuration)

	s.connWg.Go(func() {
		// Goroutine owns backlog lifecycle; clear on exit regardless of reason
		defer trimmerTicker.Stop()
		defer func() {
			s.backlog.Init()
			s.backlogSize.Store(0)
		}()

		s.backlogTrimmerLoop(ctx, trimmerTicker)
	})
}

func (s *Server) backlogTrimmerLoop(ctx context.Context, trimmerTicker *time.Ticker) {
	trimThreshold := int64(TrimRatioThreshold) * int64(s.opts.BacklogSizeLimit)
	for {
		select {
		case <-ctx.Done():
			return

		case <-trimmerTicker.C:
			if s.backlogSize.Load() > trimThreshold {
				s.backlogMu.Lock()
				for s.backlogSize.Load() > int64(s.opts.BacklogSizeLimit) {
					n := s.backlog.Front()
					s.backlogSize.Add(-int64(n.Value.(backlogEntry).size))
					s.backlog.Remove(n)
				}
				s.backlogMu.Unlock()
			}
		}
	}
}
