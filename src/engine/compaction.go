package engine

import (
	"errors"
	"kvgo/platform"
	"os"
	"path/filepath"
	"time"
)

const compactingFileSuffix = ".compacting"

type compactWorker struct {
	lastCompacted time.Time
	policy        compactionPolicy
}

type compactionPolicy struct {
	CheckEvery   time.Duration
	MinInterval  time.Duration
	MaxInterval  time.Duration
	AmpThreshold float64
}

var defaultCompactionPolicy = compactionPolicy{
	// How often to check whether compaction is needed.
	CheckEvery: time.Minute,

	// Never compact more frequently than this.
	// Compaction blocks the commit loop, so this bounds how often Put() can stall.
	MinInterval: 10 * time.Minute,

	// Force a rewrite if we have not compacted for too long.
	// This keeps the WAL from growing forever even if amplification stays low.
	MaxInterval: 24 * time.Hour,

	// Trigger when on-disk bytes are this many times larger than in-RAM logical bytes.
	AmpThreshold: 4.0,
}

var ErrInvalidCompactionPolicy = errors.New(
	"invalid compaction policy: require CheckEvery>=1m, MinInterval>=1m, MaxInterval>=1m, AmpThreshold>1, and CheckEvery<=MinInterval<=MaxInterval",
)

func newCompactionWorker(policy compactionPolicy) (*compactWorker, error) {
	if !isReasonable(policy) {
		return nil, ErrInvalidCompactionPolicy
	}
	return &compactWorker{policy: policy}, nil
}

func isReasonable(p compactionPolicy) bool {
	return p.CheckEvery >= time.Minute &&
		p.MinInterval >= time.Minute &&
		p.MaxInterval >= time.Minute &&
		p.AmpThreshold > 1 &&
		p.CheckEvery <= p.MinInterval &&
		p.MinInterval <= p.MaxInterval
}

func (s *shard) startCompactionWorker() {
	if s.compactionWorker == nil || s.committer == nil {
		return
	}

	ticker := time.NewTicker(s.compactionWorker.policy.CheckEvery)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if s.shouldCompact() {
				if err := s.compact(); err == nil {
					s.compactionWorker.lastCompacted = time.Now()
				} else if err == ErrClosed {
					return
				} else {
					// TODO: record/log the error; do NOT update lastCompacted.
				}
			}
		case <-s.committer.stopCh:
			return
		}
	}
}

func (s *shard) shouldCompact() bool {
	if s.compactionWorker == nil {
		return false
	}
	policy := s.compactionWorker.policy
	now := time.Now()
	last := s.compactionWorker.lastCompacted

	// Rate limit: never compact too frequently.
	if !last.IsZero() && now.Sub(last) < policy.MinInterval {
		return false
	}

	actualIndexBytes := s.indexBytesOnDisk.Load()
	if actualIndexBytes <= 0 {
		return false
	}

	// Optional safety net: force compaction if it's been too long.
	maxDue := !last.IsZero() && now.Sub(last) >= policy.MaxInterval

	// Expected index size = keys + overhead (16 bytes header per key)
	keyBytes := s.keyBytesInRAM.Load()
	numKeys := s.numKeys.Load()
	if keyBytes <= 0 || numKeys <= 0 {
		return maxDue
	}

	expectedIndexBytes := keyBytes + numKeys*walIndexHeaderBytes
	amp := float64(actualIndexBytes) / float64(expectedIndexBytes)
	ampDue := amp >= policy.AmpThreshold

	return maxDue || ampDue
}

func (s *shard) compact() error {
	s.mu.RLock()
	if s.committer == nil {
		s.mu.RUnlock()
		return ErrClosed
	}
	committer := s.committer
	s.mu.RUnlock()

	req := &compactRequest{doCompact: s.doCompact, err: make(chan error)}
	select {
	case committer.compactCh <- req:
		// ok
	case <-committer.stopCh:
		return ErrClosed
	}

	return <-req.err
}

// doCompact rewrites the index file to remove deleted entries.
// Called by group committer after draining writes. Value file unchanged.
func (s *shard) doCompact() error {
	indices := s.snapshot()
	if len(indices) == 0 {
		return nil
	}

	indexFilePath := filepath.Join(s.wal.path, s.wal.indexFilename)
	tempPath := indexFilePath + compactingFileSuffix

	if err := writeSnapshot(tempPath, indices); err != nil {
		return err
	}

	if err := s.wal.indexFile.Close(); err != nil {
		return err
	}

	if platform.IsWindows {
		time.Sleep(50 * time.Millisecond)
	}

	if err := replaceWALFile(indexFilePath, tempPath); err != nil {
		if platform.IsWindows {
			s.handleWindowsCompactionFailure(tempPath, err)
		}
		if reopenErr := s.reopenIndexFile(); reopenErr != nil {
			return errors.Join(err, reopenErr)
		}
		return err
	}

	return s.reopenIndexFile()
}

// writeSnapshot writes data to path atomically. Removes partial file on error.
func writeSnapshot(path string, data []byte) (err error) {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := f.Close(); cerr != nil {
			if err == nil {
				err = cerr
			} else {
				err = errors.Join(err, cerr)
			}
		}
		if err != nil {
			_ = os.Remove(path)
		}
	}()

	if _, err = f.Write(data); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	return nil
}

// reopenIndexFile reopens the index file and updates metrics.
func (s *shard) reopenIndexFile() error {
	indexPath := filepath.Join(s.wal.path, s.wal.indexFilename)
	f, err := openFile(indexPath)
	if err != nil {
		return err
	}
	s.wal.indexFile = f

	if info, err := f.Stat(); err == nil {
		s.indexBytesOnDisk.Store(info.Size())
	} else {
		// Avoid keeping a stale value if stat fails.
		s.indexBytesOnDisk.Store(0)
	}
	return nil
}

// handleWindowsCompactionFailure logs file inventory and triggers shutdown.
func (s *shard) handleWindowsCompactionFailure(tempPath string, err error) {
	s.logger.Error("CRITICAL: Windows compaction file replacement failed - index may be inconsistent",
		"error", err,
		"shard_path", s.wal.path,
		"action", "server_terminating")

	entries, readErr := os.ReadDir(s.wal.path)
	if readErr == nil {
		s.logger.Error("Files present in shard directory for manual recovery",
			"shard_path", s.wal.path,
			"index_file", s.wal.indexFilename,
			"temp_file", filepath.Base(tempPath))

		for _, entry := range entries {
			info, _ := entry.Info()
			size := int64(0)
			if info != nil {
				size = info.Size()
			}
			s.logger.Error("  file_inventory",
				"name", entry.Name(),
				"size", size,
				"is_dir", entry.IsDir())
		}
	}

	s.logger.Error("Operator action required: manually inspect shard directory and restore consistent state")
	s.logger.Error("Server terminating due to unrecoverable Windows compaction failure")

	close(s.fatalErrCh)
}

// snapshot serializes all index entries to a compact byte slice.
// Called by group committer with exclusive access to s.data.
func (s *shard) snapshot() []byte {
	total := 0
	for k, _ := range s.data {
		total += walIndexLen(len(k))
	}

	// Allocate the final size up front, then encode sequentially.
	buf := make([]byte, total)
	offset := 0
	for k, v := range s.data {
		offset += encodeInto(buf[offset:], k, v.size, v.offset)
	}
	return buf[:offset]
}
