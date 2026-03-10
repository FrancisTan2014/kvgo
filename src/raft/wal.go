package raft

import (
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
)

const frameHeaderSize = 4

var ErrCorruption = errors.New("corrupted at end")
var ErrTailCorruption = errors.New("corrupted tail")

// wal is a private write-ahead log implementation.
type wal struct {
	path     string
	filename string
	file     *os.File
	size     int64
}

func newWAL(path string, filename string) (*wal, error) {
	file, err := openFile(filepath.Join(path, filename))
	if file == nil {
		return nil, err
	}
	size, err := fileSize(file)
	if err != nil {
		_ = file.Close()
		return nil, err
	}

	return &wal{
		path:     path,
		filename: filename,
		file:     file,
		size:     size,
	}, nil
}

func openFile(path string) (*os.File, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}
	return os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_RDWR, 0644)
}

func fileSize(file *os.File) (int64, error) {
	info, err := file.Stat()
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

func (w *wal) close() error {
	return w.file.Close()
}

func (w *wal) clear() error {
	if err := w.close(); err != nil {
		return err
	}

	return os.Remove(filepath.Join(w.path, w.filename))
}

func (w *wal) sync() error {
	return w.file.Sync()
}

func (w *wal) write(batch []byte) error {
	framed := make([]byte, frameHeaderSize+len(batch))
	binary.LittleEndian.PutUint32(framed[0:], uint32(len(batch)))
	copy(framed[frameHeaderSize:], batch)
	n, err := w.file.Write(framed)
	w.size += int64(n)
	return err
}

func (w *wal) read() ([]byte, error) {
	hdr := make([]byte, frameHeaderSize)
	if _, err := io.ReadFull(w.file, hdr); err != nil {
		if err == io.ErrUnexpectedEOF {
			return nil, ErrTailCorruption
		}
		return nil, err
	}

	size := binary.LittleEndian.Uint32(hdr)
	current, err := w.file.Seek(0, io.SeekCurrent)
	if err != nil {
		return nil, err
	}
	remaining := w.size - current
	if int64(size) > remaining {
		return nil, ErrTailCorruption
	}

	buf := make([]byte, size)
	if _, err := io.ReadFull(w.file, buf); err != nil {
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return nil, ErrTailCorruption
		}
		return nil, err
	}

	return buf, nil
}

func (w *wal) truncate(offset int64) error {
	path := filepath.Join(w.path, w.filename)
	if err := w.file.Close(); err != nil {
		return err
	}
	if err := os.Truncate(path, offset); err != nil {
		return err
	}
	file, err := openFile(path)
	if err != nil {
		return err
	}
	w.file = file
	w.size = offset
	return nil
}
