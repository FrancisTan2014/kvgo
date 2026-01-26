package server

import (
	"errors"
	"os"
	"path/filepath"
)

var errLockBusy = errors.New("server: lock busy")

type dataDirLock struct {
	path string
	file *os.File
}

func acquireDataDirLock(dataDir string) (*dataDirLock, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, err
	}

	lockPath := filepath.Join(dataDir, "LOCK")
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}

	l := &dataDirLock{path: lockPath, file: f}
	if err := lockFile(f); err != nil {
		_ = f.Close()
		return nil, err
	}
	return l, nil
}

func (l *dataDirLock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	err := unlockFile(l.file)
	cerr := l.file.Close()
	l.file = nil
	if err != nil {
		if cerr != nil {
			return errors.Join(err, cerr)
		}
		return err
	}
	return cerr
}
