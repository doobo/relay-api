package middleware

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"relay-api/internal/util"
)

type contextKey int

const requestIDKey contextKey = iota

// RequestID returns the request id assigned by RequestLog, or "".
func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

// RequestLog assigns (or propagates) X-Request-ID and logs every request on
// completion, mirroring middleware/request-log.ts.
//
// The header is set before the handler runs, because a Go ResponseWriter
// commits headers on the first write; the reference could set it afterwards
// only because Hono buffers the response.
//
// Paths matching any skipPrefix avoid the access-log line (the X-Request-ID is
// still assigned). /free is skipped by default because the tunnel can carry
// thousands of tiny XHTTP packet requests per client and logging each one is
// pure overhead.
func RequestLog(next http.Handler, skipPrefixes ...string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		if id == "" {
			id = util.NewRequestID()
		}
		w.Header().Set("X-Request-ID", id)

		logRequest := true
		for _, prefix := range skipPrefixes {
			if prefix != "" && strings.HasPrefix(r.URL.Path, prefix) {
				logRequest = false
				break
			}
		}

		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		next.ServeHTTP(rec, r.WithContext(context.WithValue(r.Context(), requestIDKey, id)))

		if logRequest {
			slog.Info("request",
				"request_id", id,
				"method", r.Method,
				"path", r.URL.Path,
				"status", rec.status,
				"latency_ms", time.Since(start).Milliseconds(),
			)
		}
	})
}

// statusRecorder captures the response status for the access log while leaving
// the underlying writer (including http.Flusher, needed for SSE) available.
type statusRecorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if !s.wrote {
		s.status = code
		s.wrote = true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if !s.wrote {
		s.status = http.StatusOK
		s.wrote = true
	}
	return s.ResponseWriter.Write(b)
}

// Flush forwards to the underlying writer so streamed responses keep flushing
// through the recorder.
func (s *statusRecorder) Flush() {
	if flusher, ok := s.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

// ReadFrom forwards to the underlying writer so io.Copy can take its zero-copy
// fast path (splice on Linux) instead of bouncing through a user-space buffer.
// Without this, wrapping the writer in the recorder silently disables it.
func (s *statusRecorder) ReadFrom(src io.Reader) (int64, error) {
	if !s.wrote {
		s.status = http.StatusOK
		s.wrote = true
	}
	if readerFrom, ok := s.ResponseWriter.(io.ReaderFrom); ok {
		return readerFrom.ReadFrom(src)
	}
	return io.Copy(s.ResponseWriter, src)
}
