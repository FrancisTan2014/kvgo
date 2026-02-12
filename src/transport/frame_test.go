package transport

import (
	"bytes"
	"io"
	"net"
	"testing"
	"time"
)

func TestFramer_WriteRead(t *testing.T) {
	var buf bytes.Buffer
	f := NewFramer(&buf, &buf)

	payload := []byte("hello")
	if err := f.Write(payload); err != nil {
		t.Fatalf("Write: %v", err)
	}

	out, err := f.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !bytes.Equal(out, payload) {
		t.Errorf("got %q, want %q", out, payload)
	}
}

func TestFramer_MaxPayload(t *testing.T) {
	var buf bytes.Buffer
	f := NewFramer(&buf, &buf)
	f.SetMaxPayload(5)

	smallPayload := []byte("hi")
	if err := f.Write(smallPayload); err != nil {
		t.Fatalf("Write small payload: %v", err)
	}
	if _, err := f.Read(); err != nil {
		t.Fatalf("Read small payload: %v", err)
	}

	// Write large payload
	largePayload := make([]byte, 100)
	if err := f.Write(largePayload); err != nil {
		t.Fatalf("Write large payload: %v", err)
	}

	// Try to read large payload (should fail because maxPayload=5)
	if _, err := f.Read(); err != ErrFrameTooLarge {
		t.Errorf("expected ErrFrameTooLarge, got %v", err)
	}
}

func TestFramer_EOF(t *testing.T) {
	var buf bytes.Buffer
	f := NewFramer(&buf, &buf)

	// Try to read from empty buffer
	if _, err := f.Read(); err != io.EOF {
		t.Errorf("expected io.EOF, got %v", err)
	}
}

func TestFramer_ReadWithTimeout(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()

	f := NewConnFramer(c1)

	// Start write in goroutine (will be slow)
	go func() {
		time.Sleep(100 * time.Millisecond)
		_ = NewConnFramer(c2).Write([]byte("data"))
	}()

	// Read with long timeout (should succeed)
	data, err := f.ReadWithTimeout(200 * time.Millisecond)
	if err != nil {
		t.Fatalf("ReadWithTimeout: %v", err)
	}
	if string(data) != "data" {
		t.Errorf("got %q, want %q", data, "data")
	}
}

func TestFramer_WriteWithTimeout(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()

	f1 := NewConnFramer(c1)
	f2 := NewConnFramer(c2)

	// Read and write concurrently (net.Pipe is synchronous)
	done := make(chan error, 1)
	go func() {
		err := f1.WriteWithTimeout([]byte("test"), 100*time.Millisecond)
		done <- err
	}()

	// Read from other end
	out, err := f2.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(out) != "test" {
		t.Errorf("got %q, want %q", out, "test")
	}

	// Check write completed successfully
	if err := <-done; err != nil {
		t.Fatalf("WriteWithTimeout: %v", err)
	}
}
