package engine

import (
	"errors"
	"os"
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

func (db *DB) startCompactionWorker() {
	if db.compactionWorker == nil || db.committer == nil {
		return
	}

	ticker := time.NewTicker(db.compactionWorker.policy.CheckEvery)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if db.shouldCompact() {
				if err := db.compact(); err == nil {
					db.compactionWorker.lastCompacted = time.Now()
				} else if err == ErrClosed {
					return
				} else {
					// TODO: record/log the error; do NOT update lastCompacted.
				}
			}
		case <-db.committer.stopCh:
			return
		}
	}
}

func (db *DB) shouldCompact() bool {
	if db.compactionWorker == nil {
		return false
	}
	policy := db.compactionWorker.policy
	now := time.Now()
	last := db.compactionWorker.lastCompacted

	// Rate limit: never compact too frequently.
	if !last.IsZero() && now.Sub(last) < policy.MinInterval {
		return false
	}

	bytesOnDisk := db.bytesOnDisk.Load()
	if bytesOnDisk <= 0 {
		return false
	}

	// Optional safety net: force compaction if it's been too long.
	maxDue := !last.IsZero() && now.Sub(last) >= policy.MaxInterval

	bytesInRAM := db.bytesInRAM.Load()
	if bytesInRAM <= 0 {
		return maxDue
	}

	amp := float64(bytesOnDisk) / float64(bytesInRAM)
	ampDue := amp >= policy.AmpThreshold

	return maxDue || ampDue
}

func (db *DB) compact() error {
	db.stateMu.RLock()
	if db.closed || db.committer == nil {
		db.stateMu.RUnlock()
		return ErrClosed
	}
	committer := db.committer
	db.stateMu.RUnlock()

	req := &compactRequest{doCompact: db.doCompact, err: make(chan error)}
	select {
	case committer.compactCh <- req:
		// ok
	case <-committer.stopCh:
		return ErrClosed
	}

	return <-req.err
}

func (db *DB) doCompact() error {
	wholeMap := db.snapshot()
	tempPath := db.wal.path + ".compacting"

	// Step 1: Write the new data to a temp file.
	if err := writeSnapshot(tempPath, wholeMap); err != nil {
		return err
	}

	// Step 2: Swap the WAL files.
	// Windows does not allow renaming over an open file, so we close the live WAL
	// and use a backup+rename strategy.
	if err := db.wal.close(); err != nil {
		return err
	}

	if err := replaceWALFile(db.wal.path, tempPath); err != nil {
		// Best-effort recovery: try to reopen the old WAL to keep the DB usable.
		if reopenErr := db.openWAL(); reopenErr != nil {
			return errors.Join(err, reopenErr)
		}
		return err
	}

	// Step 3: Open the new WAL and refresh on-disk stats.
	return db.openWAL()
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

	// Move Current -> Backup.
	// If this fails, 'livePath' is still valid, so it's safe to abort.
	if err := os.Rename(livePath, backupPath); err != nil && !os.IsNotExist(err) {
		return err
	}

	// Move New -> Current. If this fails, attempt to restore Backup -> Current.
	if err := os.Rename(newPath, livePath); err != nil {
		_ = os.Rename(backupPath, livePath)
		return err
	}

	// Cleanup backup (best effort).
	_ = os.Remove(backupPath)
	return nil
}

// openWAL reopens the WAL file for normal operation and refreshes bytesOnDisk.
func (db *DB) openWAL() error {
	wal, err := newWAL(db.wal.path)
	if err != nil {
		return err
	}
	db.wal = wal

	if info, err := wal.file.Stat(); err == nil {
		db.bytesOnDisk.Store(info.Size())
	} else {
		// Avoid keeping a stale value if stat fails.
		db.bytesOnDisk.Store(0)
	}
	return nil
}

// snapshot is called from the group committer goroutine after a drain barrier,
// so no map writers can run concurrently and no shard locks are needed.
func (db *DB) snapshot() []byte {
	total := 0
	for _, s := range db.shards {
		for k, v := range s.data {
			total += walRecordLen(len(k), len(v))
		}
	}

	// Allocate the final size up front, then encode sequentially.
	buf := make([]byte, total)
	offset := 0
	for _, s := range db.shards {
		for k, v := range s.data {
			offset += encodeInto(buf[offset:], k, v)
		}
	}
	return buf[:offset]
}
