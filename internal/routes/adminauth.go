// Package routes holds the HTTP handlers for the gateway and admin APIs.
package routes

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"relay-api/internal/config"
	"relay-api/internal/db"
	"relay-api/internal/httperr"
	"relay-api/internal/middleware"
	"relay-api/internal/util"
)

// sessionPrefix marks login session tokens (session auth token).
const sessionPrefix = "sat_"

// maxLoginBodyBytes caps the login body; encrypted payloads are a few KB.
const maxLoginBodyBytes = 1 << 20

// AdminAuthRoutes serves /admin/auth/*.
type AdminAuthRoutes struct {
	cfg        config.Config
	limiter    *middleware.RateLimiter
	challenges *util.ChallengeStore
}

// NewAdminAuthRoutes wires the routes to their shared dependencies.
func NewAdminAuthRoutes(cfg config.Config, limiter *middleware.RateLimiter, challenges *util.ChallengeStore) *AdminAuthRoutes {
	return &AdminAuthRoutes{cfg: cfg, limiter: limiter, challenges: challenges}
}

// Register mounts the routes on mux.
func (h *AdminAuthRoutes) Register(mux *http.ServeMux) {
	mux.HandleFunc("/admin/auth/challenge", h.handleChallenge)
	mux.HandleFunc("/admin/auth/login", h.handleLogin)
	mux.HandleFunc("/admin/auth/logout", h.handleLogout)
	mux.HandleFunc("/admin/auth/me", h.handleMe)
	mux.HandleFunc("/admin/auth/password", h.handlePassword)
	mux.HandleFunc("/admin/auth/reset", h.handleReset)
	mux.HandleFunc("/admin/auth/users", h.handleUsers)
	mux.HandleFunc("/admin/auth/users/{id}", h.handleUser)
}

// handleChallenge serves the public keys and a one-time nonce.
func (h *AdminAuthRoutes) handleChallenge(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, "GET")
		return
	}
	challenge, err := h.challenges.Issue()
	if err != nil {
		slog.Error("issue login challenge", "error", err)
		httperr.Write(w, http.StatusInternalServerError, "Internal server error", "internal_error", "internal_error")
		return
	}
	httperr.WriteJSON(w, http.StatusOK, challenge)
}

// Login payloads. Exactly one shape is expected; the encrypted/box forms are
// produced by the browser, the plaintext form is kept for scripted clients.
type encryptedPayload struct {
	Key  string `json:"key"`
	IV   string `json:"iv"`
	Data string `json:"data"`
}

type boxPayload struct {
	PublicKey string `json:"publicKey"`
	Nonce     string `json:"nonce"`
	Data      string `json:"data"`
}

type loginBody struct {
	Encrypted *encryptedPayload `json:"encrypted"`
	Box       *boxPayload       `json:"box"`
	Username  string            `json:"username"`
	Password  string            `json:"password"`
}

type loginResponse struct {
	Token       string `json:"token"`
	TokenType   string `json:"tokenType"`
	ExpiresInMs int64  `json:"expiresInMs"`
	User        struct {
		ID       int64  `json:"id"`
		Username string `json:"username"`
		Role     string `json:"role"`
	} `json:"user"`
}

func (h *AdminAuthRoutes) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, "POST")
		return
	}

	ip := clientIP(r)
	throttleKey := "login:" + ip
	// The bucket counts attempts per IP but is cleared on success, so only
	// failures accumulate.
	if _, allowed := h.limiter.Check(throttleKey, h.cfg.AdminLoginRateLimit); !allowed {
		httperr.Write(w, http.StatusTooManyRequests, "Rate limit exceeded", "rate_limit_error", "rate_limit_exceeded")
		return
	}

	body, ok := decodeLoginBody(w, r)
	if !ok {
		return
	}

	creds, ok := h.resolveCredentials(w, body, ip)
	if !ok {
		return
	}

	user, err := db.GetAdminUserByName(creds.Username)
	if err != nil {
		slog.Error("lookup admin user", "error", err)
		httperr.Write(w, http.StatusInternalServerError, "Internal server error", "internal_error", "internal_error")
		return
	}

	// Uniform failure for unknown user / wrong password / disabled account.
	if user == nil || !user.Enabled || !util.VerifyPassword(creds.Password, user.PasswordHash) {
		_ = db.RecordAudit("admin_login_failed", "admin_user:"+creds.Username, ip)
		httperr.Write(w, http.StatusUnauthorized, "Invalid username or password", "authentication_error", "unauthorized")
		return
	}

	h.limiter.Reset(throttleKey)

	rawToken := sessionPrefix + util.RandomToken(40)
	if err := db.CreateAdminSession(util.SHA256Hex(rawToken), user.ID, h.cfg.AdminSessionTTLMs); err != nil {
		slog.Error("create admin session", "error", err)
		httperr.Write(w, http.StatusInternalServerError, "Internal server error", "internal_error", "internal_error")
		return
	}
	_ = db.UpdateAdminLastLogin(user.ID)
	_ = db.RecordAudit("admin_login", "admin_user:"+user.Username, ip)

	var resp loginResponse
	resp.Token = rawToken
	resp.TokenType = "bearer"
	resp.ExpiresInMs = h.cfg.AdminSessionTTLMs
	resp.User.ID = user.ID
	resp.User.Username = user.Username
	resp.User.Role = user.Role
	httperr.WriteJSON(w, http.StatusOK, resp)
}

// resolveCredentials decrypts the payload according to its shape. It writes the
// error response itself and reports ok=false on failure.
func (h *AdminAuthRoutes) resolveCredentials(w http.ResponseWriter, body *loginBody, ip string) (util.Credentials, bool) {
	switch {
	case body.Encrypted != nil:
		if body.Encrypted.Key == "" || body.Encrypted.IV == "" || body.Encrypted.Data == "" {
			invalidRequest(w, "encrypted payload requires key, iv and data")
			return util.Credentials{}, false
		}
		creds, err := h.challenges.DecryptWebCrypto(body.Encrypted.Key, body.Encrypted.IV, body.Encrypted.Data)
		if err != nil {
			_ = db.RecordAudit("admin_login_failed", "encrypted_payload", ip)
			decryptionFailed(w)
			return util.Credentials{}, false
		}
		return creds, true

	case body.Box != nil:
		if body.Box.PublicKey == "" || body.Box.Nonce == "" || body.Box.Data == "" {
			invalidRequest(w, "box payload requires publicKey, nonce and data")
			return util.Credentials{}, false
		}
		creds, err := h.challenges.DecryptBox(body.Box.PublicKey, body.Box.Nonce, body.Box.Data)
		if err != nil {
			_ = db.RecordAudit("admin_login_failed", "box_payload", ip)
			decryptionFailed(w)
			return util.Credentials{}, false
		}
		return creds, true

	default:
		if body.Username == "" || body.Password == "" {
			invalidRequest(w, "username and password are required")
			return util.Credentials{}, false
		}
		return util.Credentials{Username: body.Username, Password: body.Password}, true
	}
}

func (h *AdminAuthRoutes) handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, "POST")
		return
	}
	auth := middleware.AdminAuthFrom(r.Context())
	if auth != nil && auth.Via == "session" && auth.Token != "" {
		_, _ = db.DeleteAdminSession(util.SHA256Hex(auth.Token))
		_ = db.RecordAudit("admin_logout", "admin_user:"+adminUsername(auth), clientIP(r))
	}
	httperr.WriteJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (h *AdminAuthRoutes) handleMe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, "GET")
		return
	}
	auth := middleware.AdminAuthFrom(r.Context())
	if auth == nil {
		httperr.Write(w, http.StatusUnauthorized, "Admin token required", "authentication_error", "admin_auth_required")
		return
	}

	response := struct {
		Via  string `json:"via"`
		User *struct {
			ID          int64  `json:"id"`
			Username    string `json:"username"`
			Role        string `json:"role"`
			LastLoginAt *int64 `json:"last_login_at"`
		} `json:"user"`
	}{Via: auth.Via}

	if auth.User != nil {
		response.User = &struct {
			ID          int64  `json:"id"`
			Username    string `json:"username"`
			Role        string `json:"role"`
			LastLoginAt *int64 `json:"last_login_at"`
		}{
			ID:          auth.User.ID,
			Username:    auth.User.Username,
			Role:        auth.User.Role,
			LastLoginAt: auth.User.LastLoginAt,
		}
	}
	httperr.WriteJSON(w, http.StatusOK, response)
}

// decodeLoginBody reads and validates the JSON shape; it writes the error and
// reports ok=false on failure.
func decodeLoginBody(w http.ResponseWriter, r *http.Request) (*loginBody, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, maxLoginBodyBytes)
	var body loginBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httperr.Write(w, http.StatusBadRequest, "Invalid JSON body", "invalid_request_error", "invalid_json")
		return nil, false
	}
	return &body, true
}

func adminUsername(auth *middleware.AdminAuth) string {
	if auth.User != nil {
		return auth.User.Username
	}
	return "token"
}

// clientIP resolves the caller address, matching the reference's precedence.
func clientIP(r *http.Request) string {
	if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
		if first := strings.TrimSpace(strings.Split(forwarded, ",")[0]); first != "" {
			return first
		}
	}
	if realIP := r.Header.Get("X-Real-Ip"); realIP != "" {
		return realIP
	}
	return "unknown"
}

func invalidRequest(w http.ResponseWriter, message string) {
	httperr.Write(w, http.StatusBadRequest, "Invalid request: "+message, "invalid_request_error", "invalid_request")
}

func decryptionFailed(w http.ResponseWriter) {
	httperr.Write(
		w,
		http.StatusBadRequest,
		"Encrypted login payload is invalid or expired; reload the page and try again",
		"invalid_request_error",
		"login_decryption_failed",
	)
}

func methodNotAllowed(w http.ResponseWriter, allow string) {
	w.Header().Set("Allow", allow)
	httperr.Write(w, http.StatusMethodNotAllowed, "Method not allowed", "invalid_request_error", "method_not_allowed")
}

// decodeJSON reads the JSON body with a size cap; it writes the error response
// and reports false on failure.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxLoginBodyBytes)
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		httperr.Write(w, http.StatusBadRequest, "Invalid JSON body", "invalid_request_error", "invalid_json")
		return false
	}
	return true
}

func internalError(w http.ResponseWriter, context string, err error) {
	slog.Error(context, "error", err)
	httperr.Write(w, http.StatusInternalServerError, "Internal server error", "internal_error", "internal_error")
}

func forbiddenRole(w http.ResponseWriter, message string) {
	httperr.Write(w, http.StatusForbidden, message, "permission_error", "admin_role_required")
}

func conflict(w http.ResponseWriter, message, code string) {
	httperr.Write(w, http.StatusBadRequest, message, "invalid_request_error", code)
}

// minPasswordLength matches the reference schemas (newPassword min 8).
const minPasswordLength = 8

// ---------------------------------------------------------------------------
// PUT /admin/auth/password - change the caller's own password (session only).

func (h *AdminAuthRoutes) handlePassword(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		methodNotAllowed(w, "PUT")
		return
	}
	auth := middleware.AdminAuthFrom(r.Context())
	if auth == nil || auth.Via != "session" || auth.User == nil {
		httperr.Write(
			w,
			http.StatusBadRequest,
			"Password change requires an authenticated admin login (not the static ADMIN_TOKEN)",
			"invalid_request_error",
			"password_change_requires_login",
		)
		return
	}

	var body struct {
		CurrentPassword string `json:"currentPassword"`
		NewPassword     string `json:"newPassword"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.CurrentPassword == "" || len(body.NewPassword) < minPasswordLength {
		invalidRequest(w, "currentPassword is required and newPassword must be at least 8 characters")
		return
	}
	if !util.VerifyPassword(body.CurrentPassword, auth.User.PasswordHash) {
		_ = db.RecordAudit("admin_password_change_failed", "admin_user:"+auth.User.Username, clientIP(r))
		httperr.Write(w, http.StatusUnauthorized, "Current password is incorrect", "authentication_error", "unauthorized")
		return
	}
	if body.CurrentPassword == body.NewPassword {
		invalidRequest(w, "New password must differ from the current password")
		return
	}

	hash, err := util.HashPassword(body.NewPassword)
	if err != nil {
		internalError(w, "hash new password", err)
		return
	}
	if err := db.UpdateAdminPassword(auth.User.ID, hash); err != nil {
		internalError(w, "update admin password", err)
		return
	}
	// Invalidate all sessions (including this one) after a password change.
	_ = db.DeleteAdminSessionsForUser(auth.User.ID)
	_ = db.RecordAudit("admin_password_changed", "admin_user:"+auth.User.Username, clientIP(r))
	httperr.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "Password updated; please log in again"})
}

// ---------------------------------------------------------------------------
// POST /admin/auth/reset - reset an account to the default (static token only).

func (h *AdminAuthRoutes) handleReset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, "POST")
		return
	}
	auth := middleware.AdminAuthFrom(r.Context())
	if auth == nil || auth.Via != "token" {
		httperr.Write(
			w,
			http.StatusBadRequest,
			"Password reset requires the static ADMIN_TOKEN (not a login session)",
			"invalid_request_error",
			"reset_requires_admin_token",
		)
		return
	}

	var body struct {
		Username    string  `json:"username"`
		NewPassword *string `json:"newPassword"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.Username == "" {
		invalidRequest(w, "username is required")
		return
	}
	user, err := db.GetAdminUserByName(body.Username)
	if err != nil {
		internalError(w, "lookup admin user", err)
		return
	}
	if user == nil {
		conflict(w, "Admin user '"+body.Username+"' does not exist", "invalid_request")
		return
	}

	newPassword := h.cfg.AdminDefaultPassword
	if body.NewPassword != nil && *body.NewPassword != "" {
		if len(*body.NewPassword) < minPasswordLength {
			invalidRequest(w, "newPassword must be at least 8 characters")
			return
		}
		newPassword = *body.NewPassword
	}
	hash, err := util.HashPassword(newPassword)
	if err != nil {
		internalError(w, "hash reset password", err)
		return
	}
	if err := db.UpdateAdminPassword(user.ID, hash); err != nil {
		internalError(w, "reset admin password", err)
		return
	}
	_ = db.DeleteAdminSessionsForUser(user.ID)
	_ = db.RecordAudit("admin_password_reset", "admin_user:"+user.Username, clientIP(r))
	httperr.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "username": user.Username, "password": newPassword})
}

// ---------------------------------------------------------------------------
// GET/POST /admin/auth/users - list and create accounts.

type adminUserDTO struct {
	ID          int64  `json:"id"`
	Username    string `json:"username"`
	Role        string `json:"role"`
	Enabled     int    `json:"enabled"`
	CreatedAt   int64  `json:"created_at"`
	UpdatedAt   int64  `json:"updated_at"`
	LastLoginAt *int64 `json:"last_login_at"`
}

func toAdminUserDTO(user db.AdminUserRow) adminUserDTO {
	enabled := 0
	if user.Enabled {
		enabled = 1
	}
	return adminUserDTO{
		ID:          user.ID,
		Username:    user.Username,
		Role:        user.Role,
		Enabled:     enabled,
		CreatedAt:   user.CreatedAt,
		UpdatedAt:   user.UpdatedAt,
		LastLoginAt: user.LastLoginAt,
	}
}

func (h *AdminAuthRoutes) handleUsers(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		h.listUsers(w, r)
	case http.MethodPost:
		h.createUser(w, r)
	default:
		methodNotAllowed(w, "GET, POST")
	}
}

func (h *AdminAuthRoutes) listUsers(w http.ResponseWriter, r *http.Request) {
	// The account list is a management surface, not something a read-only
	// account needs to see.
	if !middleware.IsAdminAuth(middleware.AdminAuthFrom(r.Context())) {
		forbiddenRole(w, "Listing users requires an admin account")
		return
	}
	users, err := db.ListAdminUsers()
	if err != nil {
		internalError(w, "list admin users", err)
		return
	}
	data := make([]adminUserDTO, 0, len(users))
	for _, user := range users {
		data = append(data, toAdminUserDTO(user))
	}
	httperr.WriteJSON(w, http.StatusOK, map[string]any{"data": data})
}

func (h *AdminAuthRoutes) createUser(w http.ResponseWriter, r *http.Request) {
	if !middleware.IsAdminAuth(middleware.AdminAuthFrom(r.Context())) {
		forbiddenRole(w, "Creating users requires an admin account (or the static ADMIN_TOKEN)")
		return
	}

	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Role     string `json:"role"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.Username == "" || len(body.Username) > 64 {
		invalidRequest(w, "username must be 1-64 characters")
		return
	}
	if len(body.Password) < minPasswordLength {
		invalidRequest(w, "password must be at least 8 characters")
		return
	}
	role := body.Role
	if role == "" {
		role = "admin"
	}
	if role != "admin" && role != "user" {
		invalidRequest(w, "role must be 'admin' or 'user'")
		return
	}

	existing, err := db.GetAdminUserByName(body.Username)
	if err != nil {
		internalError(w, "lookup admin user", err)
		return
	}
	if existing != nil {
		conflict(w, "Admin user '"+body.Username+"' already exists", "invalid_request")
		return
	}

	hash, err := util.HashPassword(body.Password)
	if err != nil {
		internalError(w, "hash user password", err)
		return
	}
	created, err := db.CreateAdminUser(body.Username, hash, role)
	if err != nil {
		internalError(w, "create admin user", err)
		return
	}
	_ = db.RecordAudit("admin_user_created", "admin_user:"+created.Username+" ("+created.Role+")", clientIP(r))
	httperr.WriteJSON(w, http.StatusCreated, map[string]any{
		"id":       created.ID,
		"username": created.Username,
		"role":     created.Role,
	})
}

// ---------------------------------------------------------------------------
// PUT/DELETE /admin/auth/users/{id} - update or delete an account.

func (h *AdminAuthRoutes) handleUser(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		invalidRequest(w, "Invalid id")
		return
	}
	target, err := db.GetAdminUser(id)
	if err != nil {
		internalError(w, "lookup admin user", err)
		return
	}
	if target == nil {
		httperr.Write(w, http.StatusNotFound, "Admin user not found", "not_found_error", "not_found")
		return
	}

	switch r.Method {
	case http.MethodPut:
		h.updateUser(w, r, target)
	case http.MethodDelete:
		h.deleteUser(w, r, target)
	default:
		methodNotAllowed(w, "PUT, DELETE")
	}
}

func (h *AdminAuthRoutes) updateUser(w http.ResponseWriter, r *http.Request, target *db.AdminUserRow) {
	auth := middleware.AdminAuthFrom(r.Context())
	if !middleware.IsAdminAuth(auth) {
		forbiddenRole(w, "Changing users requires an admin account")
		return
	}

	var body struct {
		NewPassword *string `json:"newPassword"`
		Role        *string `json:"role"`
		Enabled     *bool   `json:"enabled"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.NewPassword == nil && body.Enabled == nil && body.Role == nil {
		invalidRequest(w, "Nothing to update (provide newPassword, role and/or enabled)")
		return
	}
	if body.Role != nil && *body.Role != "admin" && *body.Role != "user" {
		invalidRequest(w, "role must be 'admin' or 'user'")
		return
	}
	if body.NewPassword != nil && len(*body.NewPassword) < minPasswordLength {
		invalidRequest(w, "newPassword must be at least 8 characters")
		return
	}

	isSelf := auth.User != nil && auth.User.ID == target.ID

	// Demoting: no one may demote themselves, and the last remaining admin
	// cannot be demoted. Promoting is always allowed.
	if body.Role != nil && *body.Role != target.Role {
		if isSelf {
			conflict(w, "You cannot change your own role", "cannot_change_own_role")
			return
		}
		if target.Role == "admin" {
			admins, err := db.CountEnabledAdmins()
			if err != nil {
				internalError(w, "count admins", err)
				return
			}
			if admins <= 1 {
				conflict(w, "Cannot remove the last admin user", "last_admin")
				return
			}
		}
	}

	// Disabling: never lock out the last enabled admin or yourself.
	if body.Enabled != nil && !*body.Enabled {
		if isSelf {
			conflict(w, "You cannot disable your own account", "cannot_disable_self")
			return
		}
		if target.Enabled && target.Role == "admin" {
			admins, err := db.CountEnabledAdmins()
			if err != nil {
				internalError(w, "count admins", err)
				return
			}
			if admins <= 1 {
				conflict(w, "Cannot disable the last enabled admin user", "last_admin")
				return
			}
		}
	}

	if body.NewPassword != nil {
		hash, err := util.HashPassword(*body.NewPassword)
		if err != nil {
			internalError(w, "hash user password", err)
			return
		}
		if err := db.UpdateAdminPassword(target.ID, hash); err != nil {
			internalError(w, "update user password", err)
			return
		}
		// Password changed by an administrator: revoke the target's sessions.
		_ = db.DeleteAdminSessionsForUser(target.ID)
	}
	if body.Role != nil && *body.Role != target.Role {
		// Sessions survive a role change on purpose: the role is re-read from the
		// database on every request.
		if err := db.UpdateAdminRole(target.ID, *body.Role); err != nil {
			internalError(w, "update user role", err)
			return
		}
	}
	if body.Enabled != nil {
		if err := db.UpdateAdminEnabled(target.ID, *body.Enabled); err != nil {
			internalError(w, "update user enabled", err)
			return
		}
		if !*body.Enabled {
			_ = db.DeleteAdminSessionsForUser(target.ID)
		}
	}

	_ = db.RecordAudit("admin_user_updated", "admin_user:"+target.Username+" ("+updateSummary(body.NewPassword != nil, body.Role, target.Role, body.Enabled)+")", clientIP(r))

	updated, err := db.GetAdminUser(target.ID)
	if err != nil || updated == nil {
		internalError(w, "reload admin user", err)
		return
	}
	httperr.WriteJSON(w, http.StatusOK, toAdminUserDTO(*updated))
}

func (h *AdminAuthRoutes) deleteUser(w http.ResponseWriter, r *http.Request, target *db.AdminUserRow) {
	auth := middleware.AdminAuthFrom(r.Context())
	if !middleware.IsAdminAuth(auth) {
		forbiddenRole(w, "Deleting users requires an admin account")
		return
	}
	if auth.User != nil && auth.User.ID == target.ID {
		conflict(w, "You cannot delete your own account", "cannot_delete_self")
		return
	}
	if target.Enabled && target.Role == "admin" {
		admins, err := db.CountEnabledAdmins()
		if err != nil {
			internalError(w, "count admins", err)
			return
		}
		if admins <= 1 {
			conflict(w, "Cannot delete the last enabled admin user", "last_admin")
			return
		}
	}
	if _, err := db.DeleteAdminUser(target.ID); err != nil {
		internalError(w, "delete admin user", err)
		return
	}
	_ = db.RecordAudit("admin_user_deleted", "admin_user:"+target.Username, clientIP(r))
	httperr.WriteJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// updateSummary describes what an account update changed, for the audit log.
func updateSummary(passwordChanged bool, role *string, currentRole string, enabled *bool) string {
	parts := make([]string, 0, 3)
	if passwordChanged {
		parts = append(parts, "password")
	}
	if role != nil && *role != currentRole {
		parts = append(parts, "role="+*role)
	}
	if enabled != nil {
		parts = append(parts, "enabled="+strconv.FormatBool(*enabled))
	}
	return strings.Join(parts, ", ")
}
