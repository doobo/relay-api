package middleware

import (
	"net/http"
	"net/http/httptest"
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
