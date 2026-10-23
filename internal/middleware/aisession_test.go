package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAISessionIdentity(t *testing.T) {
	var captured string
	handler := BodyParser(1 << 20)(AISession(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		captured = AISessionFrom(r.Context())
	})))

	run := func(body string, headers map[string]string, remote string) string {
		t.Helper()
		request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		request.RemoteAddr = remote
		for key, value := range headers {
			request.Header.Set(key, value)
		}
		handler.ServeHTTP(httptest.NewRecorder(), request)
		return captured
	}

	t.Run("identity header", func(t *testing.T) {
		session := run(`{"model":"m"}`, map[string]string{"X-User-Id": "alice"}, "1.2.3.4:1000")
		if session != "alice" {
			t.Fatalf("session = %q, want alice", session)
		}
	})

	t.Run("body user field", func(t *testing.T) {
		session := run(`{"model":"m","user":"bob"}`, nil, "1.2.3.4:1000")
		if session != "bob" {
			t.Fatalf("session = %q, want bob", session)
		}
	})

	t.Run("opencode session header", func(t *testing.T) {
		session := run(`{"model":"m","user":"bob"}`, map[string]string{"X-Opencode-Session": "oc-1"}, "1.2.3.4:1000")
		if session != "oc-1" {
			t.Fatalf("session = %q, want oc-1", session)
		}
	})

	t.Run("explicit session header wins", func(t *testing.T) {
		session := run(`{"model":"m","user":"bob"}`, map[string]string{"X-AI-Session": "sess-1", "X-User-Id": "alice"}, "1.2.3.4:1000")
		if session != "sess-1" {
			t.Fatalf("session = %q, want sess-1", session)
		}
	})

	t.Run("ip fallback is stable and distinct", func(t *testing.T) {
		first := run(`{"model":"m"}`, nil, "1.2.3.4:1000")
		second := run(`{"model":"m"}`, nil, "1.2.3.4:2000")
		other := run(`{"model":"m"}`, nil, "5.6.7.8:1000")
		if len(first) != 32 || first != second {
			t.Fatalf("session not stable: %q %q", first, second)
		}
		if first == other {
			t.Fatalf("different IPs produced the same session %q", first)
		}
	})
}
