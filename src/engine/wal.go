package engine

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Index entry: [klen : 4bytes][vlen : 4bytes][vOffset : 8bytes][key bytes]
const walIndexHeaderBytes = 16

var ErrCorruption = errors.New("corrupted at end")
var ErrTruncate = errors.New("failed to truncate tail corruption")
var ErrInvalidRead = errors.New("invalid offset or size")

// wal is a private write-ahead log implementation.
type wal struct {
	path          string
	indexFilename string
	valueFilename string
	indexFile     *os.File
	valueFile     *os.File
}

type indexEntry struct {
	Key         string
	ValueSize   int
	ValueOffset int64
}

func newWAL(path string, indexFilename string, valueFilename string) (*wal, error) {
	indexFile, err := openFile(filepath.Join(path, indexFilename))
	if indexFile == nil {
		return nil, err
	}

	valueFile, err := openFile(filepath.Join(path, valueFilename))
	if valueFile == nil {
		return nil, err
	}

	return &wal{
		path:          path,
		indexFilename: indexFilename,
		valueFilename: valueFilename,
		indexFile:     indexFile,
		valueFile:     valueFile,
	}, nil
}

func openFile(path string) (*os.File, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}
	return os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_RDWR, 0644)
}

func (w *wal) close() error {
	err1 := w.indexFile.Close()
	err2 := w.valueFile.Close()
	return errors.Join(err1, err2)
}

func (w *wal) clear() error {
	if err := w.close(); err != nil {
		return err
	}

	e1 := os.Remove(filepath.Join(w.path, w.indexFilename))
	e2 := os.Remove(filepath.Join(w.path, w.valueFilename))
	return errors.Join(e1, e2)
}

func walIndexLen(klen int) int {
	return walIndexHeaderBytes + klen
}

func encodeInto(dst []byte, key string, vsize int, voffset int64) int {
	klen := len(key)
	total := walIndexLen(klen)

	binary.LittleEndian.PutUint32(dst[0:4], uint32(klen))
	binary.LittleEndian.PutUint32(dst[4:8], uint32(vsize))
	binary.LittleEndian.PutUint64(dst[8:16], uint64(voffset))
	copy(dst[walIndexHeaderBytes:total], []byte(key))

	return total
}

func (w *wal) readIndex(handler func(indexEntry) error) (err error) {
	var currupted bool
	var position int64
	defer func() {
		if currupted {
			if truncErr := w.indexFile.Truncate(position); truncErr != nil {
				e := fmt.Errorf("truncation failed after corruption: %w", truncErr)
				err = errors.Join(ErrCorruption, e)
			}
		}
	}()

	for {
		var header [walIndexHeaderBytes]byte
		_, err := io.ReadFull(w.indexFile, header[:])
		if err == io.ErrUnexpectedEOF {
			currupted = true
			return ErrCorruption
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}

		klen := binary.LittleEndian.Uint32(header[0:4])
		vlen := binary.LittleEndian.Uint32(header[4:8])
		offset := binary.LittleEndian.Uint64(header[8:16])

		buf := make([]byte, int(klen))
		_, err = io.ReadFull(w.indexFile, buf)
		if err == io.ErrUnexpectedEOF || err == io.EOF {
			currupted = true
			return ErrCorruption
		}
		if err != nil {
			return err
		}

		position += int64(walIndexHeaderBytes + klen)

		entry := indexEntry{
			Key:         string(buf),
			ValueSize:   int(vlen),
			ValueOffset: int64(offset),
		}
		err = handler(entry)
		if err != nil {
			return err
		}
	}
}

func (w *wal) readValue(offset int64, size int) ([]byte, error) {
	if offset < 0 || size <= 0 {
		return nil, ErrInvalidRead
	}

	if offset > 0 {
		// Seek to the offset position
		_, err := w.valueFile.Seek(offset, io.SeekStart)
		if err != nil {
			return nil, err
		}
	}

	buf := make([]byte, size)
	_, err := io.ReadFull(w.valueFile, buf)
	if err == io.ErrUnexpectedEOF {
		if truncErr := w.valueFile.Truncate(offset); truncErr != nil {
			return nil, errors.Join(ErrCorruption, ErrTruncate, truncErr)
		}
		return nil, ErrCorruption
	}

	if err != nil {
		return nil, err
	}

	return buf, nil
}

func (w *wal) writeIndex(data []byte) error {
	_, err := w.indexFile.Write(data)
	if err != nil {
		return err
	}

	return w.indexFile.Sync()
}

func (w *wal) writeValue(data []byte) error {
	_, err := w.valueFile.Write(data)
	if err != nil {
		return err
	}

	return w.valueFile.Sync()
}
