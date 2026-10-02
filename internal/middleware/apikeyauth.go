package middleware

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"relay-api/internal/config"
	"relay-api/internal/db"
	"relay-api/internal/httperr"
	"relay-api/internal/util"
)

const apiAuthKey contextKey = 3

// APIAuth is the authenticated client API key for a request.
type APIAuth struct {
	Key db.APIKeyRow
}

// APIAuthFrom returns the API-key auth context, or nil on unauthenticated
// routes.
func APIAuthFrom(ctx context.Context) *APIAuth {
	auth, _ := ctx.Value(apiAuthKey).(*APIAuth)
	return auth
}

// APIKeyAuth validates the bearer key and enforces expiry, scope, model
// permissions, rate limit and quotas (port of middleware/auth.ts). It requires
// BodyParser upstream so `model` can be checked.
func APIKeyAuth(limiter *RateLimiter) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fullKey := extractBearer(r.Header.Get("Authorization"))
			if fullKey == "" {
				unauthorizedKey(w, "Missing or invalid API key")
				return
			}

			keyRow, err := db.GetAPIKeyByHash(util.SHA256Hex(fullKey))
			if err != nil {
				httperr.Write(w, http.StatusInternalServerError, "Internal server error", "internal_error", "internal_error")
				return
			}
			if keyRow == nil {
				unauthorizedKey(w, "Unknown API key")
				return
			}
			if !keyRow.Enabled {
				unauthorizedKey(w, "API key is disabled")
				return
			}
			if keyRow.ExpiresAt != nil && *keyRow.ExpiresAt < time.Now().UnixMilli() {
				unauthorizedKey(w, "API key is expired")
				return
			}

			// Scope: ai -> /v1/*, api -> /open/*, both -> all.
			path := r.URL.Path
			scope := keyRow.Scope
			if scope == "" {
				scope = "both"
			}
			if scope == "ai" && strings.HasPrefix(path, config.ForwardPrefix+"/") {
				httperr.Write(w, http.StatusForbidden, "This API key cannot access "+config.ForwardPrefix+"/* endpoints", "permission_error", "scope_denied")
				return
			}
			if scope == "api" && strings.HasPrefix(path, "/v1/") {
				httperr.Write(w, http.StatusForbidden, "This API key cannot access /v1/* endpoints", "permission_error", "scope_denied")
				return
			}

			// Model permission: NULL or empty array = no restriction.
			allowedModels := parseJSONStringArray(keyRow.AllowedModels)
			body := ParsedJSON(r.Context())
			model, _ := body["model"].(string)
			if model != "" && len(allowedModels) > 0 && !containsString(allowedModels, model) {
				httperr.Write(w, http.StatusForbidden, "Model '"+model+"' is not allowed for this API key", "permission_error", "model_not_allowed")
				return
			}

			if _, allowed := limiter.Check(fmt.Sprintf("key:%d", keyRow.ID), keyRow.RateLimit); !allowed {
				httperr.Write(w, http.StatusTooManyRequests, "Rate limit exceeded", "rate_limit_error", "rate_limit_exceeded")
				return
			}
			if message, exceeded := enforceQuotas(*keyRow); exceeded {
				httperr.Write(w, http.StatusTooManyRequests, message, "rate_limit_error", "quota_exceeded")
				return
			}

			db.MarkAPIKeyUsed(keyRow.ID)
			ctx := context.WithValue(r.Context(), apiAuthKey, &APIAuth{Key: *keyRow})
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// enforceQuotas checks the key's day/week/month request and token quotas. A
// limit of 0 is unlimited.
func enforceQuotas(key db.APIKeyRow) (string, bool) {
	if key.DailyRequestLimit <= 0 && key.DailyTokenLimit <= 0 &&
		key.WeeklyRequestLimit <= 0 && key.WeeklyTokenLimit <= 0 &&
		key.MonthlyRequestLimit <= 0 && key.MonthlyTokenLimit <= 0 {
		return "", false
	}
	usage, err := db.APIKeyQuotaUsage(key.ID)
	if err != nil {
		return "", false
	}
	type period struct {
		label            string
		requestLimit     int
		tokenLimit       int
		requests, tokens int
	}
	periods := []period{
		{"Daily", key.DailyRequestLimit, key.DailyTokenLimit, usage.DayRequests, usage.DayTokens},
		{"Weekly", key.WeeklyRequestLimit, key.WeeklyTokenLimit, usage.WeekRequests, usage.WeekTokens},
		{"Monthly", key.MonthlyRequestLimit, key.MonthlyTokenLimit, usage.MonthRequests, usage.MonthTokens},
	}
	for _, p := range periods {
		if p.requestLimit > 0 && p.requests >= p.requestLimit {
			return fmt.Sprintf("%s request quota exceeded (%d/%d)", p.label, p.requests, p.requestLimit), true
		}
		if p.tokenLimit > 0 && p.tokens >= p.tokenLimit {
			return fmt.Sprintf("%s token quota exceeded (%d/%d)", p.label, p.tokens, p.tokenLimit), true
		}
	}
	return "", false
}

func unauthorizedKey(w http.ResponseWriter, message string) {
	httperr.Write(w, http.StatusUnauthorized, message, "authentication_error", "unauthorized")
}

func parseJSONStringArray(raw *string) []string {
	if raw == nil || *raw == "" {
		return nil
	}
	var values []string
	if err := json.Unmarshal([]byte(*raw), &values); err != nil {
		return nil
	}
	return values
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
