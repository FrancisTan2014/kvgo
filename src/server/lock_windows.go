//go:build windows

package server

import (
	"os"

	"golang.org/x/sys/windows"
)

func lockFile(f *os.File) error {
	h := windows.Handle(f.Fd())
	var ol windows.Overlapped

	// Lock the first byte of the file, non-blocking.
	err := windows.LockFileEx(
		h,
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0,
		1,
		0,
		&ol,
	)
	if err == windows.ERROR_LOCK_VIOLATION {
		return errLockBusy
	}
	return err
}

func unlockFile(f *os.File) error {
	h := windows.Handle(f.Fd())
	var ol windows.Overlapped
	return windows.UnlockFileEx(h, 0, 1, 0, &ol)
}
