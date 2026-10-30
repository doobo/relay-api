package middleware

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
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
				// Anthropic-native clients send the key in x-api-key.
				fullKey = strings.TrimSpace(r.Header.Get("x-api-key"))
			}
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

			// Model permission: NULL or empty array = no restriction. Only
			// read/decode the body when the key actually restricts models.
			allowedModels := keyRow.AllowedModelList()
			if len(allowedModels) > 0 {
				model := ""
				if summary, ok := RequestSummaryFrom(r.Context()); ok {
					model = summary.Model
				}
				if model != "" && !containsString(allowedModels, model) {
					httperr.Write(w, http.StatusForbidden, "Model '"+model+"' is not allowed for this API key", "permission_error", "model_not_allowed")
					return
				}
			}

			var rateKeyBuffer [24]byte
			rateKey := append(rateKeyBuffer[:0], "key:"...)
			rateKey = strconv.AppendInt(rateKey, keyRow.ID, 10)
			if _, allowed := limiter.Check(string(rateKey), keyRow.RateLimit); !allowed {
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
	if !key.HasQuotaLimits() {
		return "", false
	}
	usage, err := db.APIKeyQuotaUsageCached(key.ID)
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

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
