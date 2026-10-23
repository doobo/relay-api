package util

import (
	"io"
	"sync"
)

// maxBufferedPipeBytes bounds how far a writer may run ahead of the reader.
const maxBufferedPipeBytes = 256 * 1024

// BufferedPipe is a bounded in-memory byte pipe. Unlike io.Pipe, which blocks
// every Write until a matching Read, it lets a producer run ahead by up to
// maxBufferedPipeBytes. The streaming providers use it when transforming an
// upstream SSE stream: without it every emitted event costs a goroutine
// hand-off between the transformer and the response writer.
type BufferedPipe struct {
	mu     sync.Mutex
	cond   *sync.Cond
	buffer []byte
	closed bool
	err    error
}

// NewBufferedPipe returns an empty pipe. The same value is both the reader and
// the writer, so the producer and consumer must not Close it independently
// without tolerating the other side's Write/Read failing.
func NewBufferedPipe() *BufferedPipe {
	pipe := &BufferedPipe{}
	pipe.cond = sync.NewCond(&pipe.mu)
	return pipe
}

// Read copies buffered bytes, blocking while the pipe is open and empty.
func (p *BufferedPipe) Read(target []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for len(p.buffer) == 0 && !p.closed {
		p.cond.Wait()
	}
	if len(p.buffer) == 0 {
		if p.err != nil {
			return 0, p.err
		}
		return 0, io.EOF
	}
	n := copy(target, p.buffer)
	p.buffer = p.buffer[n:]
	if len(p.buffer) == 0 {
		p.buffer = nil
	}
	p.cond.Broadcast()
	return n, nil
}

// Write appends bytes, blocking only when the buffer is full.
func (p *BufferedPipe) Write(source []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for len(p.buffer) >= maxBufferedPipeBytes && !p.closed {
		p.cond.Wait()
	}
	if p.closed {
		if p.err != nil {
			return 0, p.err
		}
		return 0, io.ErrClosedPipe
	}
	p.buffer = append(p.buffer, source...)
	p.cond.Broadcast()
	return len(source), nil
}

// Close closes the pipe; pending and future reads drain the buffer first.
func (p *BufferedPipe) Close() error {
	return p.CloseWithError(nil)
}

// CloseWithError closes the pipe, making Read return err after the buffered
// bytes are consumed. A nil err makes Read return io.EOF.
func (p *BufferedPipe) CloseWithError(err error) error {
	p.mu.Lock()
	if !p.closed {
		p.closed = true
		p.err = err
		p.cond.Broadcast()
	}
	p.mu.Unlock()
	return nil
}
