package util

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestFetchUpstreamHeaderTimeout(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	defer close(release)

	start := time.Now()
	_, err := FetchUpstream(context.Background(), server.URL, FetchOptions{
		Method:          http.MethodPost,
		HeaderTimeoutMs: 50,
	})
	if !errors.Is(err, ErrUpstreamTimeout) {
		t.Fatalf("error = %v, want ErrUpstreamTimeout", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("header timeout took %v", elapsed)
	}
}

// TestFetchUpstreamBodyIdleTimeout makes sure a client upload that stalls is
// cancelled instead of pinning the request (and both sockets) forever.
func TestFetchUpstreamBodyIdleTimeout(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	defer close(release)

	reader, writer := io.Pipe()
	defer writer.Close()

	start := time.Now()
	_, err := FetchUpstream(context.Background(), server.URL, FetchOptions{
		Method:            http.MethodPost,
		Body:              reader,
		BodyIdleTimeoutMs: 50,
	})
	if !errors.Is(err, ErrUpstreamTimeout) {
		t.Fatalf("error = %v, want ErrUpstreamTimeout", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("body idle timeout took %v", elapsed)
	}
}

// TestFetchUpstreamHeaderTimeoutStopsAtHeaders makes sure the header deadline
// does not become an overall stream deadline: once headers arrive the body may
// stream for longer than HeaderTimeoutMs.
func TestFetchUpstreamHeaderTimeoutStopsAtHeaders(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		time.Sleep(150 * time.Millisecond)
		_, _ = io.WriteString(w, "data: ok\n\n")
	}))
	defer server.Close()

	response, err := FetchUpstream(context.Background(), server.URL, FetchOptions{
		Method:          http.MethodGet,
		HeaderTimeoutMs: 50,
	})
	if err != nil {
		t.Fatalf("headers arrived but the request failed: %v", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("stream body was cut off: %v", err)
	}
	if string(body) != "data: ok\n\n" {
		t.Fatalf("body = %q", body)
	}
}
