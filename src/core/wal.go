// The Write-Ahead Log engine

package core

import (
	"encoding/binary"
	"io"
	"os"
)

type WAL struct {
	file *os.File
}

func NewWAL(path string) (*WAL, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_RDWR, 0644)
	if file == nil {
		return nil, err
	}

	return &WAL{
		file,
	}, nil
}

func (w *WAL) Write(key []byte, value []byte) error {
	buf := encode(key, value)
	_, err := w.file.Write(buf)
	return err
}

func (w *WAL) Sync() error {
	return w.file.Sync()
}

func (w *WAL) Close() error {
	return w.file.Close()
}

func (w *WAL) Read(handler func([]byte, []byte) error) error {
	var err error
	var klen, vlen uint32
	header := make([]byte, 8)

	for {
		_, err = w.file.Read(header)
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}

		klen = binary.LittleEndian.Uint32(header[0:4])
		vlen = binary.LittleEndian.Uint32(header[4:8])

		buf := make([]byte, klen+vlen)
		_, err = w.file.Read(buf)
		if err != nil {
			// Data is corrupted, should be carefully deal with later
			return err
		}

		err = handler(buf[0:klen], buf[klen:])
		if err != nil {
			break
		}
	}

	return err
}

func encode(key []byte, value []byte) []byte {
	if key == nil {
		panic("Key cannot be nil")
	}

	if value == nil {
		value = make([]byte, 0)
	}

	buf := make([]byte, 8+len(key)+len(value))
	binary.LittleEndian.PutUint32(buf[0:4], uint32(len(key)))
	binary.LittleEndian.PutUint32(buf[4:8], uint32(len(value)))
	copy(buf[8:], key)
	copy(buf[8+len(key):], value)
	return buf
}
