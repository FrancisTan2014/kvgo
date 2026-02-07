package engine

import (
	"fmt"
	"kvgo/platform"
	"os"
	"path/filepath"
)

const (
	cleanupIndexBufferSize = 1024 * 1024 // 1MB
	cleanupValueBufferSize = 1024 * 1024 // 1MB
	cleanupFlushThreshold  = 512 * 1024  // 512KB

	tempFileSuffix = ".tmp"
)

// clean initiates a cleanup operation for this shard.
// Cleanup removes orphaned value data by rewriting value and index files.
// This is an operator-triggered maintenance operation that locks the shard.
func (s *shard) clean() error {
	s.mu.RLock()
	if s.committer == nil {
		s.mu.RUnlock()
		return ErrClosed
	}
	committer := s.committer
	s.mu.RUnlock()

	// Send cleanup request through compaction channel for atomicity.
	// The group committer will drain all pending writes before executing.
	req := &compactRequest{doCompact: s.doCleanup, err: make(chan error)}
	select {
	case committer.compactCh <- req:
		// ok
	case <-committer.stopCh:
		return ErrClosed
	}

	return <-req.err
}

// doCleanup rewrites both index and value files to remove orphaned data.
// Called by group committer after draining writes.
func (s *shard) doCleanup() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	tempIndexFile, tempValueFile, tempIndexPath, tempValuePath, err := s.createCleanupTempFiles()
	if err != nil {
		return err
	}
	defer tempIndexFile.Close()
	defer tempValueFile.Close()

	newOffsets, newValueOffset, err := s.writeCleanupData(tempIndexFile, tempValueFile)
	if err != nil {
		return err
	}

	if err := s.swapCleanupFiles(tempIndexFile, tempValueFile, tempIndexPath, tempValuePath); err != nil {
		return err
	}

	return s.updateShardAfterCleanup(newOffsets, newValueOffset)
}

// createCleanupTempFiles creates temporary index and value files for cleanup.
func (s *shard) createCleanupTempFiles() (*os.File, *os.File, string, string, error) {
	tempIndexPath := filepath.Join(s.wal.path, s.wal.indexFilename+tempFileSuffix)
	tempValuePath := filepath.Join(s.wal.path, s.wal.valueFilename+tempFileSuffix)

	tempIndexFile, err := openFile(tempIndexPath)
	if err != nil {
		return nil, nil, "", "", fmt.Errorf("create temp index file: %w", err)
	}

	tempValueFile, err := openFile(tempValuePath)
	if err != nil {
		tempIndexFile.Close()
		return nil, nil, "", "", fmt.Errorf("create temp value file: %w", err)
	}

	return tempIndexFile, tempValueFile, tempIndexPath, tempValuePath, nil
}

// writeCleanupData writes live data to temp files with new sequential offsets.
func (s *shard) writeCleanupData(tempIndexFile, tempValueFile *os.File) (map[string]int64, int64, error) {
	newValueOffset := int64(0)
	indexBuf := make([]byte, 0, cleanupIndexBufferSize)
	valueBuf := make([]byte, 0, cleanupValueBufferSize)
	newOffsets := make(map[string]int64, len(s.data))

	for key, ve := range s.data {
		value, err := s.getValue(ve)
		if err != nil {
			return nil, 0, fmt.Errorf("get value for key %q: %w", key, err)
		}

		if len(value) > 0 {
			valueBuf = append(valueBuf, value...)
			if len(valueBuf) >= cleanupFlushThreshold {
				if _, err := tempValueFile.Write(valueBuf); err != nil {
					return nil, 0, fmt.Errorf("write value data: %w", err)
				}
				valueBuf = valueBuf[:0]
			}
		}

		newOffsets[key] = newValueOffset

		entrySize := walIndexLen(len(key))
		if cap(indexBuf)-len(indexBuf) < entrySize {
			if _, err := tempIndexFile.Write(indexBuf); err != nil {
				return nil, 0, fmt.Errorf("write index data: %w", err)
			}
			indexBuf = indexBuf[:0]
		}

		oldLen := len(indexBuf)
		indexBuf = indexBuf[:oldLen+entrySize]
		encodeInto(indexBuf[oldLen:], key, len(value), newValueOffset)

		newValueOffset += int64(len(value))
	}

	if len(valueBuf) > 0 {
		if _, err := tempValueFile.Write(valueBuf); err != nil {
			return nil, 0, fmt.Errorf("write remaining value data: %w", err)
		}
	}
	if len(indexBuf) > 0 {
		if _, err := tempIndexFile.Write(indexBuf); err != nil {
			return nil, 0, fmt.Errorf("write remaining index data: %w", err)
		}
	}

	return newOffsets, newValueOffset, nil
}

// swapCleanupFiles replaces production files with cleaned files.
// Windows failure triggers server shutdown with file inventory for manual recovery.
func (s *shard) swapCleanupFiles(tempIndexFile, tempValueFile *os.File, tempIndexPath, tempValuePath string) error {
	if err := tempValueFile.Sync(); err != nil {
		return fmt.Errorf("sync temp value file: %w", err)
	}
	if err := tempIndexFile.Sync(); err != nil {
		return fmt.Errorf("sync temp index file: %w", err)
	}

	tempIndexFile.Close()
	tempValueFile.Close()

	if err := s.wal.close(); err != nil {
		return fmt.Errorf("close current WAL: %w", err)
	}

	indexPath := filepath.Join(s.wal.path, s.wal.indexFilename)
	valuePath := filepath.Join(s.wal.path, s.wal.valueFilename)

	if err := replaceWALFiles(indexPath, valuePath, tempIndexPath, tempValuePath); err != nil {
		if platform.IsWindows {
			s.handleWindowsReplacementFailure(tempIndexPath, tempValuePath, err)
		}
		return err
	}

	return nil
}

// handleWindowsReplacementFailure logs file inventory and triggers shutdown.
func (s *shard) handleWindowsReplacementFailure(tempIndexPath, tempValuePath string, err error) {
	s.logger.Error("CRITICAL: Windows cleanup file replacement failed - data consistency not guaranteed",
		"error", err,
		"shard_path", s.wal.path,
		"action", "server_terminating")

	entries, readErr := os.ReadDir(s.wal.path)
	if readErr == nil {
		s.logger.Error("Files present in shard directory for manual recovery",
			"shard_path", s.wal.path,
			"index_file", s.wal.indexFilename,
			"value_file", s.wal.valueFilename,
			"temp_index", filepath.Base(tempIndexPath),
			"temp_value", filepath.Base(tempValuePath))

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
	s.logger.Error("Server terminating due to unrecoverable Windows file replacement failure")

	close(s.fatalErrCh)
}

// updateShardAfterCleanup reopens the WAL and updates in-memory offsets.
func (s *shard) updateShardAfterCleanup(newOffsets map[string]int64, newValueOffset int64) error {
	// Reopen WAL with new files
	w, err := newWAL(s.wal.path, s.wal.indexFilename, s.wal.valueFilename)
	if err != nil {
		return fmt.Errorf("reopen WAL: %w", err)
	}
	s.wal = w

	// Update in-memory offsets
	for key, newOffset := range newOffsets {
		if ve, exists := s.data[key]; exists {
			ve.offset = newOffset
			// Keep payload in memory if it was already loaded
		}
	}

	// Update metrics
	s.valueOffset.Store(newValueOffset)

	return nil
}
