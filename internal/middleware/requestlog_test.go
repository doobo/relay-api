package middleware

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRequestLogSkipPrefixSkipsRecorderButKeepsID(t *testing.T) {
	var handlerWriter http.ResponseWriter
	var handlerID string
	handler := RequestLog(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlerWriter = w
		handlerID = RequestID(r.Context())
		w.WriteHeader(http.StatusNoContent)
	}), "/free")

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/free/stream", nil))

	if handlerWriter == nil || handlerID == "" {
		t.Fatalf("handler writer=%v id=%q", handlerWriter, handlerID)
	}
	if _, wrapped := handlerWriter.(*statusRecorder); wrapped {
		t.Fatal("skipped path should not be wrapped in a status recorder")
	}
	if got := recorder.Header().Get("X-Request-ID"); got != handlerID {
		t.Fatalf("response id = %q, handler id = %q", got, handlerID)
	}
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("status = %d", recorder.Code)
	}
}

func TestRequestLogSamplesSuccessButKeepsErrors(t *testing.T) {
	var buf bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	handler := RequestLogWithConfig(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/fail" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}), AccessLogConfig{SkipPrefixes: []string{"/free"}, Sample: 3})

	for range 9 {
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/ok", nil))
	}
	for range 2 {
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/fail", nil))
	}
	// Skipped paths never reach the log even when they would have been sampled.
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/free/stream", nil))

	// 9 successful requests at 1-in-3 plus 2 errors that are always logged.
	if got := strings.Count(buf.String(), "msg=request"); got != 5 {
		t.Fatalf("request log lines = %d, want 5; output:\n%s", got, buf.String())
	}
}
