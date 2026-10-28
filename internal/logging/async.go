// Package logging provides the process-wide logger setup. The default handler
// writes through an asynchronous, batching writer so a slow or blocked stdout
// can never stall request goroutines: /free is skipped by the access log, but
// every other route logs a line, and a global slog mutex waiting on a stalled
// pipe used to take the whole admin UI and the AI API down with it.
package logging

import (
	"bytes"
	"io"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// DefaultQueueSize is the number of log records buffered before new records
	// are dropped. 8192 lines is several seconds of traffic at hundreds of
	// requests per second, and the memory is bounded (roughly the line size).
	DefaultQueueSize = 8192
	// DefaultBatchSize is the number of bytes coalesced into one destination
	// Write. Batching turns one syscall per log line into one per ~64 KiB.
	DefaultBatchSize = 64 << 10
	// DefaultBatchLines caps a batch by line count as well, because an access
	// log line is ~120 bytes: 100 lines flush long before 64 KiB accumulates,
	// so a burst of requests still shows up in the log immediately.
	DefaultBatchLines = 100
	// DefaultFlushEvery bounds how long a queued line waits when traffic is
	// light. Access logs are not latency sensitive, so a short delay is fine.
	DefaultFlushEvery = 200 * time.Millisecond
	// closeTimeout keeps shutdown from hanging forever when the destination is
	// already blocked; the process is exiting anyway.
	closeTimeout = 2 * time.Second
)

// newline counts how many records a chunk carries.
var newline = []byte{'\n'}

// Options tunes an AsyncWriter. Zero values select the defaults above.
type Options struct {
	QueueSize  int
	BatchSize  int
	BatchLines int
	FlushEvery time.Duration
}

// AsyncWriter is an io.Writer that copies each record into a bounded queue and
// returns immediately. One background goroutine drains the queue into batches
// and writes them to the destination.
//
// The writer never blocks the caller and never returns an error: logging is
// best-effort by design, so a full queue drops the record and counts it, and
// the next successful flush emits a single warning with the dropped total.
type AsyncWriter struct {
	dst        io.Writer
	queue      chan []byte
	batchSize  int
	batchLines int
	flushEvery time.Duration

	done      chan struct{}
	closeOnce sync.Once
	wg        sync.WaitGroup
	dropped   atomic.Uint64
}

// NewAsyncWriter starts an asynchronous writer around dst.
func NewAsyncWriter(dst io.Writer, opts Options) *AsyncWriter {
	if opts.QueueSize <= 0 {
		opts.QueueSize = DefaultQueueSize
	}
	if opts.BatchSize <= 0 {
		opts.BatchSize = DefaultBatchSize
	}
	if opts.BatchLines <= 0 {
		opts.BatchLines = DefaultBatchLines
	}
	if opts.FlushEvery <= 0 {
		opts.FlushEvery = DefaultFlushEvery
	}
	writer := &AsyncWriter{
		dst:        dst,
		queue:      make(chan []byte, opts.QueueSize),
		batchSize:  opts.BatchSize,
		batchLines: opts.BatchLines,
		flushEvery: opts.FlushEvery,
		done:       make(chan struct{}),
	}
	writer.wg.Add(1)
	go writer.run()
	return writer
}

// Write enqueues p and reports success even when the queue is full. A dropped
// record is counted and surfaced on the next flush, so a blocked destination
// degrades logging instead of stalling the request path.
func (w *AsyncWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	chunk := make([]byte, len(p))
	copy(chunk, p)
	select {
	case w.queue <- chunk:
	default:
		w.dropped.Add(1)
	}
	return len(p), nil
}

// Close stops accepting batches, drains the queue and waits (bounded) for the
// final write. It is safe to call more than once.
func (w *AsyncWriter) Close() error {
	w.closeOnce.Do(func() { close(w.done) })
	finished := make(chan struct{})
	go func() {
		w.wg.Wait()
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(closeTimeout):
	}
	return nil
}

// run drains the queue until Close, coalescing records into one Write per
// batch or flush interval. A blocked destination fills the queue; producers
// keep running and the excess is dropped.
func (w *AsyncWriter) run() {
	defer w.wg.Done()
	ticker := time.NewTicker(w.flushEvery)
	defer ticker.Stop()

	batch := make([]byte, 0, w.batchSize)
	lines := 0
	flush := func() {
		if dropped := w.dropped.Swap(0); dropped > 0 {
			batch = append(batch, `level=WARN msg="async log writer dropped records" dropped=`...)
			batch = strconv.AppendUint(batch, dropped, 10)
			batch = append(batch, '\n')
		}
		if len(batch) == 0 {
			return
		}
		_, _ = w.dst.Write(batch)
		batch = batch[:0]
		lines = 0
	}
	// appendChunk buffers one record and flushes as soon as either cap is hit,
	// so 100 short access-log lines do not wait for 64 KiB to accumulate.
	appendChunk := func(chunk []byte) {
		batch = append(batch, chunk...)
		lines += bytes.Count(chunk, newline)
		if len(batch) >= w.batchSize || lines >= w.batchLines {
			flush()
		}
	}

	for {
		select {
		case chunk := <-w.queue:
			appendChunk(chunk)
		case <-ticker.C:
			flush()
		case <-w.done:
			// Drain what is already queued, then exit. The queue is never
			// closed, so a producer racing with Close cannot panic.
			for {
				select {
				case chunk := <-w.queue:
					appendChunk(chunk)
				default:
					flush()
					return
				}
			}
		}
	}
}
