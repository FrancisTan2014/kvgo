// Package engine provides atomic file replacement operations.
//
// LESSON LEARNED: DON'T OVER-ENGINEER AROUND PLATFORM LIMITATIONS
//
// Original implementation had 150+ lines of retry logic, backup/restore, rollback.
// Studied etcd (production system with years of usage) - they use simple os.Rename.
//
// The truth:
// - Windows has NO atomic multi-file replacement primitive
// - Retry loops and backups don't fix this fundamental gap
// - Adding complexity creates more failure modes without improving crash-safety
// - Between any two operations, crash = potential inconsistency (both approaches)
//
// What we learned from etcd:
// - Accept platform limitations, don't pretend to solve them
// - Use the simplest code that works
// - Document what you DON'T guarantee
// - Move on
//
// This simpler version has the same crash-safety as the complex version (none for
// multi-file ops) but is easier to understand, test, and maintain.
package engine

import (
	"fmt"
	"kvgo/platform"
	"os"
)

// replaceWALFile replaces livePath with newPath.
//
// Unix: Atomic via os.Rename (POSIX guarantees atomic replacement).
// Windows: os.Rename fails if target exists, must remove first.
//          NOT atomic - crash between Remove and Rename = data loss.
//          This is a platform limitation we accept (matches etcd, Redis, others).
func replaceWALFile(livePath, newPath string) error {
	if !platform.IsWindows {
		// Unix: os.Rename atomically replaces existing files
		return os.Rename(newPath, livePath)
	}

	// Windows: Remove then rename. Not atomic but simplest approach.
	_ = os.Remove(livePath)
	return os.Rename(newPath, livePath)
}

// replaceWALFiles replaces both index and value files.
//
// Unix: Each os.Rename is atomic individually, but two renames are NOT atomic together.
// Windows: Even worse - requires Remove before Rename, four non-atomic operations.
//
// Limitation accepted: Crash between file replacements = inconsistent state.
// WAL replay detects corruption via checksums and discards incomplete entries.
func replaceWALFiles(indexPath, valuePath, tempIndexPath, tempValuePath string) error {
	if !platform.IsWindows {
		// Unix: Each rename is atomic
		if err := os.Rename(tempIndexPath, indexPath); err != nil {
			return fmt.Errorf("replace index: %w", err)
		}
		if err := os.Rename(tempValuePath, valuePath); err != nil {
			return fmt.Errorf("replace value: %w", err)
		}
		return nil
	}

	// Windows: Remove + rename for each file
	_ = os.Remove(indexPath)
	if err := os.Rename(tempIndexPath, indexPath); err != nil {
		return fmt.Errorf("replace index: %w", err)
	}

	_ = os.Remove(valuePath)
	if err := os.Rename(tempValuePath, valuePath); err != nil {
		return fmt.Errorf("replace value: %w", err)
	}

	return nil
}
