package middleware

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"

	"relay-api/internal/httperr"
)

const parsedBodyKey contextKey = 2

// parsedBody holds a buffered request body and its JSON parse.
type parsedBody struct {
	raw  []byte
	json map[string]any
}

// BodyParser enforces the request size limit and buffers the body, parsing JSON
// for later authorization checks (model permissions). It must not be mounted on
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
				if len(data) > 0 {
					var parsed map[string]any
					if json.Unmarshal(data, &parsed) == nil {
						body.json = parsed
					}
				}
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
		return body.json
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
