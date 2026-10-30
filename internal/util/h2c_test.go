package util

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// TestFetchUpstreamH2COptIn pins the per-request switch: plaintext upstreams
// stay on HTTP/1.1 unless the caller opts in, and the opt-in reaches an h2c
// server as HTTP/2. The test server uses net/http's native h2c support, the
// same protocol setup main.go serves with.
func TestFetchUpstreamH2COptIn(t *testing.T) {
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(r.Proto))
	}))
	server.Config.Protocols = protocols
	server.Start()
	defer server.Close()

	target, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		h2c  bool
		want string
	}{
		{h2c: false, want: "HTTP/1.1"},
		{h2c: true, want: "HTTP/2.0"},
	}
	for _, tc := range cases {
		response, err := FetchUpstream(context.Background(), "", FetchOptions{
			Method: http.MethodGet,
			URL:    target,
			H2C:    tc.h2c,
		})
		if err != nil {
			t.Fatalf("h2c=%v: %v", tc.h2c, err)
		}
		body, err := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if err != nil {
			t.Fatalf("h2c=%v read: %v", tc.h2c, err)
		}
		if string(body) != tc.want {
			t.Fatalf("h2c=%v: protocol = %q, want %q", tc.h2c, body, tc.want)
		}
	}
}
