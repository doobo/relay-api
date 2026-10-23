package routes

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestHeaderSnapshotRedactsSecrets(t *testing.T) {
	header := http.Header{}
	header.Set("Authorization", "Bearer sk-secret")
	header.Set("Cookie", "session=abc")
	header.Set("X-User-Id", "alice")
	header.Set("X-Opencode-Session", "oc-1")

	raw := snapshotHTTPHeaders(header)
	if raw == nil {
		t.Fatal("expected a snapshot")
	}
	var parsed map[string]string
	if err := json.Unmarshal([]byte(*raw), &parsed); err != nil {
		t.Fatalf("decode snapshot: %v", err)
	}
	if parsed["authorization"] != "[redacted]" || parsed["cookie"] != "[redacted]" {
		t.Fatalf("secrets not redacted: %+v", parsed)
	}
	if parsed["x-user-id"] != "alice" || parsed["x-opencode-session"] != "oc-1" {
		t.Fatalf("non-secret headers missing: %+v", parsed)
	}
	if snapshotHeaderMap(nil) != nil {
		t.Fatal("empty headers should not produce a snapshot")
	}
}
