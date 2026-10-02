// Package httperr renders the OpenAI-style error envelope the whole gateway
// (and the reused admin UI) depends on:
//
//	{ "error": { "message": "...", "type": "...", "code": "..." } }
package httperr

import (
	"encoding/json"
	"net/http"
)

// Error is the inner object of the envelope.
type Error struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code"`
}

// Envelope is the top-level JSON error body.
type Envelope struct {
	Error Error `json:"error"`
}

// New builds an envelope from its parts.
func New(message, kind, code string) Envelope {
	return Envelope{Error: Error{Message: message, Type: kind, Code: code}}
}

// WriteJSON writes body as JSON with the given status.
func WriteJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("content-type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// Write renders an error envelope with the given status.
func Write(w http.ResponseWriter, status int, message, kind, code string) {
	WriteJSON(w, status, New(message, kind, code))
}

// NotFound is the shared 404 for unknown paths and routes.
func NotFound(w http.ResponseWriter) {
	Write(w, http.StatusNotFound, "Not found", "not_found_error", "not_found")
}
