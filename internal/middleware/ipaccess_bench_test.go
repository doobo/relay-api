package middleware

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"relay-api/internal/ipacl"
)

func BenchmarkIPAccess(b *testing.B) {
	for _, count := range []int{0, 10, 100} {
		b.Run(fmt.Sprintf("whitelist_%d", count), func(b *testing.B) {
			entries := make([]string, count)
			for i := range entries {
				entries[i] = fmt.Sprintf("10.%d.%d.0/24", i/256, i%256)
			}
			rules, err := ipacl.Compile(entries, nil)
			if err != nil {
				b.Fatal(err)
			}
			store := ipacl.NewStore(rules)
			handler := IPAccess(store)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))

			request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
			request.RemoteAddr = "10.0.0.77:1234"
			if count > 0 {
				// Match the last entry, the linear scan's worst case.
				request.RemoteAddr = fmt.Sprintf("10.%d.%d.77:1234", (count-1)/256, (count-1)%256)
			}
			writer := &discardResponseWriter{header: make(http.Header)}

			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				handler.ServeHTTP(writer, request)
			}
		})
	}
}

type discardResponseWriter struct {
	header http.Header
}

func (w *discardResponseWriter) Header() http.Header         { return w.header }
func (w *discardResponseWriter) Write(p []byte) (int, error) { return len(p), nil }
func (w *discardResponseWriter) WriteHeader(int)             {}
