package routes

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"relay-api/internal/db"
)

// discardResponseWriter is the minimal ResponseWriter+Flusher a copy benchmark
// needs: it counts bytes without accumulating them, so the numbers describe the
// tunnel loop rather than a growing recorder buffer.
type discardResponseWriter struct {
	header http.Header
}

func (w *discardResponseWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}

func (w *discardResponseWriter) Write(p []byte) (int, error) { return len(p), nil }
func (w *discardResponseWriter) WriteHeader(int)             {}
func (w *discardResponseWriter) Flush()                      {}

// BenchmarkCopyAndFlush measures the copy loop with the two buffer regimes:
// a body smaller than the 8 KiB chunk (no promotion) and a bulk body that
// promotes once to the configured large chunk.
func BenchmarkCopyAndFlush(b *testing.B) {
	for _, testCase := range []struct {
		name string
		size int
	}{
		{"small_4k", 4 << 10},
		{"bulk_256k", 256 << 10},
	} {
		b.Run(testCase.name, func(b *testing.B) {
			payload := bytes.Repeat([]byte("x"), testCase.size)
			writer := &discardResponseWriter{}
			b.SetBytes(int64(testCase.size))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := copyAndFlush(writer, bytes.NewReader(payload)); err != nil {
					b.Fatalf("copyAndFlush: %v", err)
				}
			}
		})
	}
}

// BenchmarkTunnelHandle measures the full handler over a loopback upstream,
// including config resolution, header filtering and usage enqueueing. The
// upstream round trip is part of the number; compare before/after changes on
// the same machine rather than treating it as a pure CPU benchmark.
func BenchmarkTunnelHandle(b *testing.B) {
	harness := newTunnelHarness(b)

	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "application/octet-stream")
		_, _ = w.Write([]byte("ok"))
	}))
	defer mock.Close()

	mustConfig(b, db.NewAPIConfig{
		Name: "bench", Method: "NONE", URL: mock.URL + "/",
		StreamTimeoutMs: 15000, Route: "free", Enabled: true,
	})

	request := httptest.NewRequest(http.MethodGet, "/free/bench", nil)
	recorder := httptest.NewRecorder()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		recorder.Body.Reset()
		harness.handler.ServeHTTP(recorder, request)
	}
}
