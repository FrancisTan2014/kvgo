package engine

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

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

func (s *shard) doCompact() error {
	indices := s.snapshot()

	// Skip compaction if there's no data to compact
	if len(indices) == 0 {
		return nil
	}

	indexFilePath := filepath.Join(s.wal.path, s.wal.indexFilename)
	tempPath := indexFilePath + ".compacting"

	// Step 1: Write compacted index to temp file.
	if err := writeSnapshot(tempPath, indices); err != nil {
		return err
	}

	// Step 2: Close index file only (value file stays open).
	if err := s.wal.indexFile.Close(); err != nil {
		return err
	}

	// Windows needs a moment to fully release the file handle
	if runtime.GOOS == "windows" {
		time.Sleep(50 * time.Millisecond)
	}

	// Step 3: Swap the index file using backup+rename.
	if err := replaceWALFile(indexFilePath, tempPath); err != nil {
		// Best-effort recovery: reopen the old index file.
		if reopenErr := s.reopenIndexFile(); reopenErr != nil {
			return errors.Join(err, reopenErr)
		}
		return err
	}

	// Step 4: Reopen the new index file and refresh on-disk stats.
	return s.reopenIndexFile()
}

// writeSnapshot creates/truncates path, writes data, and fsyncs it.
// On failure, it best-effort removes the partially written file.
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

// replaceWALFile replaces livePath with newPath using a backup+rename sequence.
// On Windows this avoids renaming over an existing file.
func replaceWALFile(livePath, newPath string) error {
	backupPath := livePath + ".bak"

	// Remove old backup if it exists.
	if err := os.Remove(backupPath); err != nil && !os.IsNotExist(err) {
		return err
	}

	// Move Current -> Backup with retry on Windows (file handle release delay).
	if err := retryRename(livePath, backupPath); err != nil && !os.IsNotExist(err) {
		return err
	}

	// Move New -> Current. If this fails, attempt to restore Backup -> Current.
	if err := retryRename(newPath, livePath); err != nil {
		_ = os.Rename(backupPath, livePath)
		return err
	}

	// Cleanup backup (best effort).
	_ = os.Remove(backupPath)
	return nil
}

// retryRename retries os.Rename on Windows to handle file handle release delays.
func retryRename(oldpath, newpath string) error {
	var err error
	maxRetries := 10

	for i := 0; i < maxRetries; i++ {
		err = os.Rename(oldpath, newpath)
		if err == nil {
			return nil
		}

		// Only retry on Windows for file access errors
		if runtime.GOOS == "windows" && i < maxRetries-1 {
			time.Sleep(time.Duration(20*(i+1)) * time.Millisecond)
			continue
		}
		break
	}
	return err
}

// reopenIndexFile reopens just the index file after compaction and refreshes metrics.
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

// snapshot is called from the group committer goroutine after a drain barrier,
// so no map writers can run concurrently and no shard locks are needed.
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
