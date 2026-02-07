package engine

import (
	"fmt"
	"kvgo/platform"
	"os"
	"time"
)

const (
	backupFileSuffix = ".bak"
)

// replaceWALFile replaces livePath with newPath.
// Unix: Atomic via os.Rename.
// Windows: Best-effort with backup/restore. Not crash-safe.
func replaceWALFile(livePath, newPath string) error {
	if !platform.IsWindows {
		// Unix: os.Rename atomically replaces existing files
		return os.Rename(newPath, livePath)
	}

	// Windows chaos: os.Rename fails if target exists, requires backup dance
	return replaceWALFileWindows(livePath, newPath)
}

// replaceWALFiles replaces both index and value files.
// Unix: Atomic replacements via os.Rename.
// Windows: Best-effort consistency with rollback attempts. Not transactional.
func replaceWALFiles(indexPath, valuePath, tempIndexPath, tempValuePath string) error {
	if !platform.IsWindows {
		// Unix: Each os.Rename is atomic
		if err := os.Rename(tempIndexPath, indexPath); err != nil {
			return fmt.Errorf("replace index: %w", err)
		}
		if err := os.Rename(tempValuePath, valuePath); err != nil {
			return fmt.Errorf("replace value: %w", err)
		}
		return nil
	}

	// Windows chaos: complicated backup/restore with file handle delays
	return replaceWALFilesWindows(indexPath, valuePath, tempIndexPath, tempValuePath)
}

// --- Windows-specific implementations below ---
// These provide best-effort consistency but are NOT crash-safe or transactional.
// File handle delays, backup failures, or crashes leave inconsistent state.

func replaceWALFileWindows(livePath, newPath string) error {
	backupPath := livePath + backupFileSuffix

	if err := os.Remove(backupPath); err != nil && !os.IsNotExist(err) {
		return err
	}

	if err := retryRenameWindows(livePath, backupPath); err != nil && !os.IsNotExist(err) {
		return err
	}

	if err := retryRenameWindows(newPath, livePath); err != nil {
		_ = os.Rename(backupPath, livePath)
		return err
	}

	_ = os.Remove(backupPath)
	return nil
}

func replaceWALFilesWindows(indexPath, valuePath, tempIndexPath, tempValuePath string) error {
	indexBackup := indexPath + backupFileSuffix
	valueBackup := valuePath + backupFileSuffix

	if err := backupBothFilesWindows(indexPath, valuePath, indexBackup, valueBackup); err != nil {
		return fmt.Errorf("backup phase: %w", err)
	}

	if err := retryRenameWindows(tempIndexPath, indexPath); err != nil {
		_ = retryRenameWindows(indexBackup, indexPath)
		_ = retryRenameWindows(valueBackup, valuePath)
		return fmt.Errorf("replace index file: %w", err)
	}

	if err := retryRenameWindows(tempValuePath, valuePath); err != nil {
		_ = retryRenameWindows(indexBackup, indexPath)
		_ = retryRenameWindows(valueBackup, valuePath)
		return fmt.Errorf("replace value file: %w", err)
	}

	_ = os.Remove(indexBackup)
	_ = os.Remove(valueBackup)

	return nil
}

func backupBothFilesWindows(indexPath, valuePath, indexBackup, valueBackup string) error {
	_ = os.Remove(indexBackup)
	_ = os.Remove(valueBackup)

	if err := retryRenameWindows(indexPath, indexBackup); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("backup index: %w", err)
	}

	if err := retryRenameWindows(valuePath, valueBackup); err != nil && !os.IsNotExist(err) {
		_ = retryRenameWindows(indexBackup, indexPath)
		return fmt.Errorf("backup value: %w", err)
	}

	return nil
}

func retryRenameWindows(oldpath, newpath string) error {
	var err error
	maxRetries := 10

	for i := 0; i < maxRetries; i++ {
		err = os.Rename(oldpath, newpath)
		if err == nil {
			return nil
		}

		if i < maxRetries-1 {
			time.Sleep(time.Duration(20*(i+1)) * time.Millisecond)
			continue
		}
		break
	}
	return err
}

// Test helpers - expose internal implementations for unit testing
var (
	retryRename     = retryRenameImpl
	backupBothFiles = backupBothFilesImpl
)

func retryRenameImpl(oldpath, newpath string) error {
	if !platform.IsWindows {
		return os.Rename(oldpath, newpath)
	}
	return retryRenameWindows(oldpath, newpath)
}

func backupBothFilesImpl(indexPath, valuePath, indexBackup, valueBackup string) error {
	if !platform.IsWindows {
		if err := os.Rename(indexPath, indexBackup); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("backup index: %w", err)
		}
		if err := os.Rename(valuePath, valueBackup); err != nil && !os.IsNotExist(err) {
			_ = os.Rename(indexBackup, indexPath)
			return fmt.Errorf("backup value: %w", err)
		}
		return nil
	}
	return backupBothFilesWindows(indexPath, valuePath, indexBackup, valueBackup)
}
