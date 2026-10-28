package logging

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"
)

// lockedBuffer is a goroutine-safe destination for the async writer.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestAsyncWriterDrainsOnClose(t *testing.T) {
	dst := &lockedBuffer{}
	writer := NewAsyncWriter(dst, Options{QueueSize: 16, BatchSize: 4096, FlushEvery: time.Hour})

	lines := []string{"one\n", "two\n", "three\n"}
	for _, line := range lines {
		if _, err := writer.Write([]byte(line)); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if got, want := dst.String(), strings.Join(lines, ""); got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

func TestAsyncWriterFlushesOnInterval(t *testing.T) {
	dst := &lockedBuffer{}
	writer := NewAsyncWriter(dst, Options{QueueSize: 8, BatchSize: 4096, FlushEvery: 10 * time.Millisecond})
	defer func() { _ = writer.Close() }()

	if _, err := writer.Write([]byte("tick\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	deadline := time.Now().Add(time.Second)
	for !strings.Contains(dst.String(), "tick") {
		if time.Now().After(deadline) {
			t.Fatal("queued line was not flushed")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// blockingWriter holds every write until release is closed, simulating a log
// pipe whose consumer stopped reading.
type blockingWriter struct {
	release chan struct{}
	mu      sync.Mutex
	buf     bytes.Buffer
}

func (w *blockingWriter) Write(p []byte) (int, error) {
	<-w.release
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *blockingWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

func TestAsyncWriterNeverBlocksOnStalledDestination(t *testing.T) {
	dst := &blockingWriter{release: make(chan struct{})}
	writer := NewAsyncWriter(dst, Options{QueueSize: 1, BatchSize: 1, FlushEvery: time.Millisecond})

	start := time.Now()
	for index := 0; index < 2000; index++ {
		if _, err := writer.Write([]byte("line\n")); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("2000 writes took %v; Write must not wait on the destination", elapsed)
	}

	close(dst.release)
	if err := writer.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if !strings.Contains(dst.String(), "dropped=") {
		t.Fatalf("dropped records were not reported: %q", dst.String())
	}
}
