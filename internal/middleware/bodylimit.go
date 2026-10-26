package middleware

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"sync"

	"relay-api/internal/httperr"
)

const parsedBodyKey contextKey = 2

// parsedBody holds a buffered request body and decodes it to JSON only when a
// handler actually needs the object. Pure /open passthrough requests never ask
// for it, so they skip the parse entirely.
type parsedBody struct {
	raw  []byte
	once sync.Once
	json map[string]any

	summaryOnce sync.Once
	summary     RequestSummary
	summaryOK   bool
}

// parsed decodes the body to a JSON object on first use. Invalid JSON (or a
// non-object body) yields nil, matching the previous eager behaviour.
func (b *parsedBody) parsed() map[string]any {
	b.once.Do(func() {
		if len(b.raw) == 0 {
			return
		}
		var parsed map[string]any
		if json.Unmarshal(b.raw, &parsed) == nil {
			b.json = parsed
		}
	})
	return b.json
}

// BodyParser enforces the request size limit and buffers the body. JSON is
// decoded lazily by ParsedJSON, so callers that only need the raw bytes (the
// /open passthrough path) never pay for a parse. It must not be mounted on
// streaming routes such as /free.
func BodyParser(maxBytes int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Body != nil {
				if declared, err := strconv.ParseInt(r.Header.Get("Content-Length"), 10, 64); err == nil && declared > maxBytes {
					writeTooLarge(w, maxBytes)
					return
				}
				data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBytes))
				if err != nil {
					writeTooLarge(w, maxBytes)
					return
				}
				body := &parsedBody{raw: data}
				r.Body = io.NopCloser(bytes.NewReader(data))
				r = r.WithContext(context.WithValue(r.Context(), parsedBodyKey, body))
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RequestBody returns the buffered raw request body, or nil.
func RequestBody(ctx context.Context) []byte {
	if body, ok := ctx.Value(parsedBodyKey).(*parsedBody); ok {
		return body.raw
	}
	return nil
}

// ParsedJSON returns the parsed JSON object of the request body, or nil.
func ParsedJSON(ctx context.Context) map[string]any {
	if body, ok := ctx.Value(parsedBodyKey).(*parsedBody); ok {
		return body.parsed()
	}
	return nil
}

func writeTooLarge(w http.ResponseWriter, limit int64) {
	httperr.Write(
		w,
		http.StatusBadRequest,
		"Request body too large (limit "+strconv.FormatInt(limit, 10)+" bytes)",
		"invalid_request_error",
		"request_too_large",
	)
}
