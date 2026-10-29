package util

import (
	"net/http/httptest"
	"testing"
)

func TestClientIPPrecedence(t *testing.T) {
	request := httptest.NewRequest("GET", "/", nil)
	request.RemoteAddr = "10.0.0.1:1234"
	request.Header.Set("X-Real-IP", "198.51.100.2")
	request.Header.Set("X-Forwarded-For", "198.51.100.3, 10.0.0.9")
	request.Header.Set("CF-Connecting-IP", "203.0.113.7")

	if got := ClientIP(request); got != "203.0.113.7" {
		t.Fatalf("ClientIP = %q, want CF-Connecting-IP", got)
	}

	request.Header.Del("CF-Connecting-IP")
	if got := ClientIP(request); got != "198.51.100.3" {
		t.Fatalf("ClientIP = %q, want first X-Forwarded-For entry", got)
	}

	request.Header.Del("X-Forwarded-For")
	if got := ClientIP(request); got != "198.51.100.2" {
		t.Fatalf("ClientIP = %q, want X-Real-IP", got)
	}

	request.Header.Del("X-Real-IP")
	if got := ClientIP(request); got != "10.0.0.1" {
		t.Fatalf("ClientIP = %q, want RemoteAddr host", got)
	}
}
