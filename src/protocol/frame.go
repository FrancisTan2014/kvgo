package protocol

import (
	"bufio"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"time"
)

// DefaultMaxFrameSize is a safety limit to avoid unbounded allocations on malformed input.
// Tune this based on expected max key/value sizes.
const DefaultMaxFrameSize = 16 << 20 // 16 MiB

var (
	ErrFrameTooLarge  = errors.New("protocol: frame too large")
	ErrInvalidFrame   = errors.New("protocol: invalid frame")
	ErrInvalidTimeout = errors.New("protocol: invalid timeout")
	ErrNoDeadline     = errors.New("protocol: reader/writer does not support deadlines")
)

type deadlineReader interface{ SetReadDeadline(time.Time) error }
type deadlineWriter interface{ SetWriteDeadline(time.Time) error }

// Framer reads and writes length-prefixed frames.
//
// Framing is required because TCP is a byte stream; it does not preserve message boundaries.
type Framer struct {
	r          *bufio.Reader
	w          *bufio.Writer
	maxPayload int
	readDL     deadlineReader
	writeDL    deadlineWriter
}

// NewConnFramer is a convenience for the common server/client case.
// It enables ReadWithTimeout/WriteWithTimeout.
func NewConnFramer(conn net.Conn) *Framer { return NewFramer(conn, conn) }

// NewFramer wraps r/w with buffering and enables optional per-call timeouts if r/w
// supports deadlines (typically net.Conn).
func NewFramer(r io.Reader, w io.Writer) *Framer {
	var readDL deadlineReader
	if v, ok := r.(deadlineReader); ok {
		readDL = v
	}
	var writeDL deadlineWriter
	if v, ok := w.(deadlineWriter); ok {
		writeDL = v
	}

	return &Framer{
		r:          bufio.NewReader(r),
		w:          bufio.NewWriter(w),
		maxPayload: DefaultMaxFrameSize,
		readDL:     readDL,
		writeDL:    writeDL,
	}
}

// SetMaxPayload sets the maximum permitted payload size.
// If you set this too large, a peer can force large allocations.
func (f *Framer) SetMaxPayload(n int) { f.maxPayload = n }

func (f *Framer) Read() ([]byte, error) { return readFrameMax(f.r, f.maxPayload) }

// ReadWithTimeout reads one frame but fails if the read does not complete within timeout.
// This requires the underlying reader to support deadlines (typically net.Conn).
//
// Note: this sets a deadline on the underlying connection and clears it afterwards.
// If you want more control, set deadlines on the net.Conn directly.
func (f *Framer) ReadWithTimeout(timeout time.Duration) ([]byte, error) {
	if timeout < 0 {
		return nil, ErrInvalidTimeout
	}
	if timeout == 0 {
		return f.Read()
	}
	return f.ReadWithDeadline(time.Now().Add(timeout))
}

// ReadWithDeadline reads one frame but fails if the read does not complete by the deadline.
// This requires the underlying reader to support deadlines (typically net.Conn).
//
// Note: this sets a deadline on the underlying connection and clears it afterwards.
// If you want more control, set deadlines on the net.Conn directly.
func (f *Framer) ReadWithDeadline(deadline time.Time) ([]byte, error) {
	if deadline.IsZero() {
		return f.Read()
	}
	if f.readDL == nil {
		return nil, ErrNoDeadline
	}
	if err := f.readDL.SetReadDeadline(deadline); err != nil {
		return nil, err
	}
	defer f.readDL.SetReadDeadline(time.Time{})
	return f.Read()
}

// Write writes one frame.
func (f *Framer) Write(payload []byte) error {
	if err := writeFrame(f.w, payload); err != nil {
		return err
	}
	return f.w.Flush()
}

// WriteWithTimeout writes one frame but fails if the write does not complete within timeout.
// This requires the underlying writer to support deadlines (typically net.Conn).
//
// Note: this sets a deadline on the underlying connection and clears it afterwards.
// If you want more control, set deadlines on the net.Conn directly.
func (f *Framer) WriteWithTimeout(payload []byte, timeout time.Duration) error {
	if timeout < 0 {
		return ErrInvalidTimeout
	}
	if timeout == 0 {
		return f.Write(payload)
	}
	return f.WriteWithDeadline(payload, time.Now().Add(timeout))
}

// WriteWithDeadline writes one frame but fails if the write does not complete by the deadline.
// This requires the underlying writer to support deadlines (typically net.Conn).
//
// Note: this sets a deadline on the underlying connection and clears it afterwards.
// If you want more control, set deadlines on the net.Conn directly.
func (f *Framer) WriteWithDeadline(payload []byte, deadline time.Time) error {
	if deadline.IsZero() {
		return f.Write(payload)
	}
	if f.writeDL == nil {
		return ErrNoDeadline
	}
	if err := f.writeDL.SetWriteDeadline(deadline); err != nil {
		return err
	}
	defer f.writeDL.SetWriteDeadline(time.Time{})
	return f.Write(payload)
}

// Frame format:
//
//	[4 bytes frameLen LE][frameLen bytes payload]
//
// This is intentionally simple: io.ReadFull is the "state machine".
func readFrameMax(r io.Reader, maxPayload int) ([]byte, error) {
	if maxPayload <= 0 {
		return nil, ErrInvalidFrame
	}

	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	ln := int(binary.LittleEndian.Uint32(hdr[:]))
	if ln < 0 || ln > maxPayload {
		return nil, ErrFrameTooLarge
	}
	if ln == 0 {
		return []byte{}, nil
	}

	payload := make([]byte, ln)
	if _, err := io.ReadFull(r, payload); err != nil {
		return nil, err
	}
	return payload, nil
}

func writeFrame(w io.Writer, payload []byte) error {
	if len(payload) > int(^uint32(0)) {
		return ErrFrameTooLarge
	}
	var hdr [4]byte
	binary.LittleEndian.PutUint32(hdr[:], uint32(len(payload)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err := w.Write(payload)
	return err
}
