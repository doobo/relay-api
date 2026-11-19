package middleware

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
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

// AccessLogConfig tunes RequestLog. The zero value keeps the historical
// behaviour: every request except the configured prefixes is logged.
type AccessLogConfig struct {
	// SkipPrefixes are path prefixes that bypass the access log entirely.
	SkipPrefixes []string
	// Sample logs one in N successful (status < 400) requests. 1 (or less)
	// logs every request. Failed requests are always logged, so a sampled log
	// still contains every error and slow-path investigation starts from those.
	Sample int
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
	return RequestLogWithConfig(next, AccessLogConfig{SkipPrefixes: skipPrefixes})
}

// RequestLogWithConfig is RequestLog with sampling and skip prefixes supplied
// as one value. Sampling only applies to successful responses; 4xx/5xx are
// always kept.
func RequestLogWithConfig(next http.Handler, cfg AccessLogConfig) http.Handler {
	skipPrefixes := cfg.SkipPrefixes
	sample := cfg.Sample
	if sample < 1 {
		sample = 1
	}
	// Only successful requests go through the counter, so errors neither
	// consume nor perturb the sample sequence.
	var successSeq atomic.Uint64

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		logRequest := true
		for _, prefix := range skipPrefixes {
			if prefix != "" && strings.HasPrefix(r.URL.Path, prefix) {
				logRequest = false
				break
			}
		}

		id := r.Header.Get("X-Request-ID")
		if id == "" {
			if logRequest {
				id = util.NewRequestID()
			} else {
				id = util.NewFastRequestID()
			}
		}
		w.Header().Set("X-Request-ID", id)

		ctx := context.WithValue(r.Context(), requestIDKey, id)
		if !logRequest {
			// High-frequency skipped paths (the /free tunnel) get the original
			// writer and no status recorder: the access log is the only reason
			// for the wrapper, and crypto/rand ids were pure per-packet cost.
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}

		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		next.ServeHTTP(rec, r.WithContext(ctx))

		if rec.status < http.StatusBadRequest && sample > 1 &&
			successSeq.Add(1)%uint64(sample) != 0 {
			return
		}

		// LogAttrs avoids the []any allocation of the args form; the access log
		// is the most frequent log site, so this matters under load.
		slog.LogAttrs(ctx, slog.LevelInfo, "request",
			slog.String("request_id", id),
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Int("status", rec.status),
			slog.Int64("latency_ms", time.Since(start).Milliseconds()),
			slog.String("client_ip", util.ClientIP(r)),
			slog.String("protocol", requestProtocol(r)),
		)
	})
}

// requestProtocol names the wire protocol for the access log. This server
// speaks h2c directly; behind a TLS terminator an h2 request arrives with no
// r.TLS, so h2 vs h2c is decided by whether the connection itself is TLS.
func requestProtocol(r *http.Request) string {
	if r.ProtoMajor == 2 {
		if r.TLS != nil {
			return "h2"
		}
		return "h2c"
	}
	if r.ProtoMajor == 1 {
		return "http/1.1"
	}
	return r.Proto
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
