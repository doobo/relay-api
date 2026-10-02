package middleware

import (
	"context"
	"net/http"
	"strings"
	"time"

	"relay-api/internal/db"
	"relay-api/internal/httperr"
	"relay-api/internal/util"
)

const adminAuthKey contextKey = 1

// AdminAuth is the authenticated admin identity for a request.
type AdminAuth struct {
	// Via is "session" for login-based auth, "token" for the static ADMIN_TOKEN.
	Via string
	// User is nil for the static ADMIN_TOKEN.
	User *db.AdminUserRow
	// Token is the raw bearer token (used by logout to revoke the session).
	Token string
}

// AdminAuthFrom returns the auth context, or nil on a public route.
func AdminAuthFrom(ctx context.Context) *AdminAuth {
	auth, _ := ctx.Value(adminAuthKey).(*AdminAuth)
	return auth
}

// IsAdminAuth reports whether the caller may change things: a full admin
// account, or the static ADMIN_TOKEN (machine access, keeps full rights).
func IsAdminAuth(auth *AdminAuth) bool {
	if auth == nil {
		return false
	}
	return auth.Via == "token" || (auth.User != nil && auth.User.Role == "admin")
}

// AdminAuthMiddleware authenticates /admin/* requests against either the
// static ADMIN_TOKEN or a DB-backed login session.
type AdminAuthMiddleware struct {
	adminToken string
}

// NewAdminAuthMiddleware builds the middleware for the given static token
// (empty disables static-token auth).
func NewAdminAuthMiddleware(adminToken string) *AdminAuthMiddleware {
	return &AdminAuthMiddleware{adminToken: adminToken}
}

// Handler wraps the /admin/* subtree. Only POST /admin/auth/login and
// GET /admin/auth/challenge are public; everything else needs a bearer token.
func (m *AdminAuthMiddleware) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.ReplaceAll(r.URL.Path, "\\", "/")
		if path == "/admin/auth/login" && r.Method == http.MethodPost {
			next.ServeHTTP(w, r)
			return
		}
		if path == "/admin/auth/challenge" && r.Method == http.MethodGet {
			next.ServeHTTP(w, r)
			return
		}

		token := extractBearer(r.Header.Get("Authorization"))
		if token == "" {
			writeAdminAuthRequired(w)
			return
		}

		// Static bootstrap token keeps scripted/admin-token workflows working.
		if m.adminToken != "" && token == m.adminToken {
			next.ServeHTTP(w, withAdminAuth(r, &AdminAuth{Via: "token", Token: token}))
			return
		}

		db.PurgeExpiredAdminSessions()
		session, err := db.GetAdminSession(util.SHA256Hex(token))
		if err == nil && session != nil && session.ExpiresAt > time.Now().UnixMilli() {
			user, err := db.GetAdminUser(session.AdminUserID)
			if err == nil && user != nil && user.Enabled {
				next.ServeHTTP(w, withAdminAuth(r, &AdminAuth{Via: "session", User: user, Token: token}))
				return
			}
		}

		writeAdminAuthRequired(w)
	})
}

// RequireAdminRole lets reads (GET/HEAD) through and requires an admin for
// every mutating method. Mount it on the resource APIs; /admin/auth/* is
// deliberately exempt so a read-only account can still log out.
func RequireAdminRole(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead:
			next.ServeHTTP(w, r)
			return
		}
		if IsAdminAuth(AdminAuthFrom(r.Context())) {
			next.ServeHTTP(w, r)
			return
		}
		httperr.Write(w, http.StatusForbidden, "Your account has read-only access", "permission_error", "admin_role_required")
	})
}

func withAdminAuth(r *http.Request, auth *AdminAuth) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), adminAuthKey, auth))
}

// extractBearer parses "Bearer <token>" (scheme case-insensitive).
func extractBearer(header string) string {
	trimmed := strings.TrimSpace(header)
	const prefix = "bearer "
	if len(trimmed) < len(prefix) || !strings.EqualFold(trimmed[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(trimmed[len(prefix):])
}

func writeAdminAuthRequired(w http.ResponseWriter) {
	httperr.Write(w, http.StatusUnauthorized, "Admin token required", "authentication_error", "admin_auth_required")
}
