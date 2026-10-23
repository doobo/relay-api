package util

import (
	"errors"
	"io"
	"testing"
)

// TestBufferedPipeBuffersAndDrains locks the contract the streaming providers
// rely on: a write completes before anyone reads, buffered bytes drain in
// order, and Close turns the next empty read into io.EOF.
func TestBufferedPipeBuffersAndDrains(t *testing.T) {
	pipe := NewBufferedPipe()

	if _, err := pipe.Write([]byte("first")); err != nil {
		t.Fatalf("write before read: %v", err)
	}
	if _, err := pipe.Write([]byte("-second")); err != nil {
		t.Fatalf("second write: %v", err)
	}

	buffer := make([]byte, 64)
	n, err := pipe.Read(buffer)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(buffer[:n]) != "first-second" {
		t.Fatalf("read = %q", buffer[:n])
	}

	if err := pipe.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := pipe.Read(buffer); !errors.Is(err, io.EOF) {
		t.Fatalf("read after close = %v, want EOF", err)
	}
	if _, err := pipe.Write([]byte("late")); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("write after close = %v, want ErrClosedPipe", err)
	}
}
