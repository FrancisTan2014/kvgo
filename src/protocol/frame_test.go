package protocol

import (
	"bytes"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

func TestFrame_RoundTrip(t *testing.T) {
	var buf bytes.Buffer
	payload := []byte("hello")

	f := NewFramer(&buf, &buf)
	if err := f.Write(payload); err != nil {
		t.Fatalf("Write: %v", err)
	}

	got, err := f.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("payload mismatch: got %q want %q", got, payload)
	}
}

func TestFrame_EmptyPayload(t *testing.T) {
	var buf bytes.Buffer

	f := NewFramer(&buf, &buf)
	if err := f.Write(nil); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := f.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty payload, got %d bytes", len(got))
	}
}

func TestReadFrameMax_TooLarge(t *testing.T) {
	var buf bytes.Buffer
	_ = writeFrame(&buf, []byte("0123456789"))

	_, err := readFrameMax(&buf, 3)
	if !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("expected ErrFrameTooLarge, got %v", err)
	}
}

func TestReadFrame_Truncated(t *testing.T) {
	// Header says 5, but payload is only 2 bytes.
	data := []byte{
		5, 0, 0, 0,
		'a', 'b',
	}
	_, err := readFrameMax(bytes.NewReader(data), DefaultMaxFrameSize)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("expected io.ErrUnexpectedEOF, got %v", err)
	}
}

func TestFramer_ReadWithTimeout(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()

	f := NewConnFramer(c1)
	_, err := f.ReadWithTimeout(20 * time.Millisecond)
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	var ne net.Error
	if !errors.As(err, &ne) || !ne.Timeout() {
		t.Fatalf("expected net timeout error, got %T: %v", err, err)
	}
}
