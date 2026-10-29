package middleware

import (
	"context"
	"net/http"
	"strings"

	"relay-api/internal/util"
)

// AISessionContextKey is the name the resolved client identity is exposed under
// to provider header templates.
const AISessionContextKey = "_ai_session"

const aiSessionKey contextKey = 4

// aiSessionHeaders are checked in order for a client-supplied identity. The
// dedicated session names win so a caller can override ambiguous generic names.
var aiSessionHeaders = []string{
	"x-opencode-session",
	"x-ai-session",
	"ai-session",
	"x-session-id",
	"session-id",
	"x-user-id",
	"user-id",
	"x-client-id",
	"client-id",
	"x-uuid",
	"uuid",
	"x-user",
	"user",
}

// AISessionFrom returns the resolved client identity, or "" when the
// middleware is not mounted.
func AISessionFrom(ctx context.Context) string {
	session, _ := ctx.Value(aiSessionKey).(string)
	return session
}

// AISession resolves a stable client identity and puts it on the request
// context for provider header templates. It looks at well-known identity
// headers, then the OpenAI-style `user` body field, and finally falls back to a
// hash of the client IP and User-Agent.
//
// It must run after BodyParser so the body is available.
func AISession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		session := resolveAISession(r)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), aiSessionKey, session)))
	})
}

func resolveAISession(r *http.Request) string {
	for _, name := range aiSessionHeaders {
		if value := strings.TrimSpace(r.Header.Get(name)); value != "" {
			return sanitizeSession(value)
		}
	}
	if summary, ok := RequestSummaryFrom(r.Context()); ok {
		if value := strings.TrimSpace(summary.User); value != "" {
			return sanitizeSession(value)
		}
		if value := strings.TrimSpace(summary.UserID); value != "" {
			return sanitizeSession(value)
		}
	}

	seed := util.ClientIP(r) + "|" + r.UserAgent()
	sum := util.SHA256Hex(seed)
	return sum[:32]
}

// sanitizeSession strips control characters and caps the length so the value is
// always safe to emit as a header.
func sanitizeSession(value string) string {
	cleaned := strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, value)
	if len(cleaned) > 200 {
		return cleaned[:200]
	}
	return cleaned
}
