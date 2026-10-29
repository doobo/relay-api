package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"relay-api/internal/ipacl"
)

func TestIPAccessDisabledAllowsEverything(t *testing.T) {
	store := ipacl.NewStore(nil)
	recorder := callIPAccess(t, store, "8.8.8.8:1234", nil)
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", recorder.Code)
	}
}

func TestIPAccessWhitelist(t *testing.T) {
	store := ipacl.NewStore(mustRules(t, []string{"192.0.2.0/24"}, nil))

	recorder := callIPAccess(t, store, "192.0.2.5:1234", nil)
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("whitelisted status = %d, want 204", recorder.Code)
	}

	recorder = callIPAccess(t, store, "198.51.100.5:1234", nil)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("non-whitelisted status = %d, want 403", recorder.Code)
	}
	if !containsCode(recorder, "ip_forbidden") {
		t.Fatalf("body = %s, want ip_forbidden", recorder.Body.String())
	}
}

func TestIPAccessBlacklist(t *testing.T) {
	store := ipacl.NewStore(mustRules(t, nil, []string{"198.51.100.0/24"}))

	recorder := callIPAccess(t, store, "198.51.100.5:1234", nil)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("blacklisted status = %d, want 403", recorder.Code)
	}

	recorder = callIPAccess(t, store, "192.0.2.5:1234", nil)
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("allowed status = %d, want 204", recorder.Code)
	}
}

func TestIPAccessLoopbackBypass(t *testing.T) {
	// A loopback caller is exempt even when its own address is blacklisted.
	store := ipacl.NewStore(mustRules(t, []string{"192.0.2.0/24"}, []string{"127.0.0.1", "::1"}))

	recorder := callIPAccess(t, store, "127.0.0.1:4000", nil)
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("IPv4 loopback status = %d, want 204", recorder.Code)
	}
	recorder = callIPAccess(t, store, "[::1]:4000", nil)
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("IPv6 loopback status = %d, want 204", recorder.Code)
	}
}

func TestIPAccessSpoofedLoopbackHeaderDoesNotBypass(t *testing.T) {
	// The TCP peer is remote, so claiming a loopback forwarded address must
	// not skip the blacklist.
	store := ipacl.NewStore(mustRules(t, nil, []string{"127.0.0.1"}))
	recorder := callIPAccess(t, store, "8.8.8.8:1234", map[string]string{
		"CF-Connecting-IP": "127.0.0.1",
	})
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", recorder.Code)
	}
}

func TestIPAccessLocalProxyForwardsRealClient(t *testing.T) {
	// A local reverse proxy is loopback at the socket level, but the forwarded
	// client address is what the lists must match.
	store := ipacl.NewStore(mustRules(t, []string{"192.0.2.0/24"}, nil))
	recorder := callIPAccess(t, store, "127.0.0.1:1234", map[string]string{
		"X-Forwarded-For": "198.51.100.7, 127.0.0.1",
	})
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("proxied client status = %d, want 403", recorder.Code)
	}
}

func TestIPAccessUnparseableClientFailsClosed(t *testing.T) {
	store := ipacl.NewStore(mustRules(t, []string{"192.0.2.0/24"}, nil))
	recorder := callIPAccess(t, store, "8.8.8.8:1234", map[string]string{
		"X-Forwarded-For": "not-an-ip",
	})
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", recorder.Code)
	}
}

func callIPAccess(t *testing.T, store *ipacl.Store, remoteAddr string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	handler := IPAccess(store)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	request.RemoteAddr = remoteAddr
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func mustRules(t *testing.T, whitelist, blacklist []string) *ipacl.Rules {
	t.Helper()
	rules, err := ipacl.Compile(whitelist, blacklist)
	if err != nil {
		t.Fatalf("compile rules: %v", err)
	}
	return rules
}

func containsCode(recorder *httptest.ResponseRecorder, code string) bool {
	return strings.Contains(recorder.Body.String(), code)
}
