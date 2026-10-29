package util

import (
	"net"
	"net/http"
	"strings"
)

// ClientIP returns the caller's address, preferring the headers a trusted
// reverse proxy sets and falling back to the socket address. Cloudflare's
// CF-Connecting-IP comes first because it is a single, unambiguous value even
// when the request also carries an X-Forwarded-For chain.
func ClientIP(r *http.Request) string {
	// net/http stores header keys in canonical form (Cf-Connecting-Ip), so
	// looking them up in that form avoids the allocation CanonicalMIMEHeaderKey
	// otherwise performs on every call. ClientIP is on the hot path of both
	// usage logging and the IP access-control middleware.
	for _, name := range [...]string{"Cf-Connecting-Ip", "True-Client-Ip"} {
		if value := strings.TrimSpace(r.Header.Get(name)); value != "" {
			return value
		}
	}
	if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
		if first := strings.TrimSpace(strings.Split(forwarded, ",")[0]); first != "" {
			return first
		}
	}
	if realIP := strings.TrimSpace(r.Header.Get("X-Real-Ip")); realIP != "" {
		return realIP
	}
	host, _, err := net.SplitHostPort(strings.TrimSpace(r.RemoteAddr))
	if err == nil {
		return host
	}
	return strings.TrimSpace(r.RemoteAddr)
}
