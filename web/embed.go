// Package web embeds the admin UI and serves it.
//
// The UI is copied verbatim from the reference gateway's web/ directory: it is
// plain ES modules with absolute asset paths (/style/*.css, /js/**/*.js,
// /favicon.svg) and calls the Admin API same-origin. Serving those exact paths
// is therefore all it takes to reuse the frontend unchanged.
//
// The Twitter/X rewrite of TweetNaCl (web/js/vendor/tweetnacl.js) is vendored
// from the bytebase the reference served from node_modules; the WebCrypto-free
// login fallback dynamically imports it.
package web

import (
	"net/http"
	"path"
	"strings"

	"embed"

	"relay-api/internal/httperr"
)

//go:embed index.html favicon.svg style js
var assets embed.FS

// contentTypes pins the MIME types the reference served, so the browser gets
// an ES-module-compatible JS type regardless of the host's mime.types.
var contentTypes = map[string]string{
	".html": "text/html; charset=utf-8",
	".css":  "text/css; charset=utf-8",
	".js":   "text/javascript; charset=utf-8",
	".svg":  "image/svg+xml",
}

// Handler serves the embedded UI. It is meant to be mounted on "/" (the lowest
// priority pattern), so it only sees paths no API route matched, and it answers
// unknown paths with the shared JSON 404.
func Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("allow", "GET, HEAD")
			httperr.Write(w, http.StatusMethodNotAllowed, "Method not allowed", "invalid_request_error", "method_not_allowed")
			return
		}

		name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if name == "" {
			name = "index.html"
		}

		data, err := assets.ReadFile(name)
		if err != nil {
			httperr.NotFound(w)
			return
		}

		w.Header().Set("content-type", contentTypeFor(name))
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodHead {
			return
		}
		_, _ = w.Write(data)
	})
}

func contentTypeFor(name string) string {
	if ctype, ok := contentTypes[path.Ext(name)]; ok {
		return ctype
	}
	return "application/octet-stream"
}
