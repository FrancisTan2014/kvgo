package core

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
)

// wal is a private write-ahead log implementation.
type wal struct {
	file *os.File
}

func newWAL(path string) (*wal, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_RDWR, 0644)
	if file == nil {
		return nil, err
	}

	return &wal{file: file}, nil
}

func walRecordLen(keyLen int, valueLen int) int {
	return walRecordHeaderBytes + keyLen + valueLen
}

func (w *wal) writeAndSync(buf []byte) error {
	_, err := w.file.Write(buf)
	if err != nil {
		return err
	}

	return w.file.Sync()
}

func (w *wal) close() error {
	return w.file.Close()
}

func (w *wal) read(handler func([]byte, []byte) error) error {
	for {
		var header [walRecordHeaderBytes]byte
		_, err := io.ReadFull(w.file, header[:])
		if err == io.ErrUnexpectedEOF {
			fmt.Printf("Warning: WAL corrupted at end, truncating.\n")
			return io.EOF
		}
		if err != nil {
			return err
		}

		klen := binary.LittleEndian.Uint32(header[0:4])
		vlen := binary.LittleEndian.Uint32(header[4:8])

		buf := make([]byte, int(klen+vlen))
		_, err = io.ReadFull(w.file, buf)
		if err == io.ErrUnexpectedEOF {
			// Ignore trailing partial record instead of failing startup.
			fmt.Printf("Warning: WAL corrupted at end, truncating.\n")
			return io.EOF
		}
		if err != nil {
			return err
		}

		err = handler(buf[:klen], buf[klen:])
		if err != nil {
			return err
		}
	}
}

func encodeInto(dst []byte, key string, value []byte) int {
	klen := len(key)
	vlen := len(value)
	binary.LittleEndian.PutUint32(dst[0:4], uint32(klen))
	binary.LittleEndian.PutUint32(dst[4:8], uint32(vlen))
	copy(dst[walRecordHeaderBytes:walRecordHeaderBytes+klen], key)
	copy(dst[walRecordHeaderBytes+klen:walRecordHeaderBytes+klen+vlen], value)
	return walRecordLen(klen, vlen)
}
