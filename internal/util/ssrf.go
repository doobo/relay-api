package util

import (
	"fmt"
	"net/netip"
	"net/url"
	"os"
	"strings"
)

// privatePrefixes are the IPv4/IPv6 ranges the gateway refuses by default
// (port of utils/ssrf.ts).
var privatePrefixes = []netip.Prefix{
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("::1/128"),
	netip.MustParsePrefix("fc00::/7"),
	netip.MustParsePrefix("fe80::/10"),
}

var blockedHostnames = map[string]bool{
	"localhost":                true,
	"metadata.google.internal": true,
}

// PrivateUpstreamsAllowed reports whether ALLOW_PRIVATE_UPSTREAMS relaxes the
// SSRF checks. Accepts the usual truthy spellings, case-insensitively.
func PrivateUpstreamsAllowed() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("ALLOW_PRIVATE_UPSTREAMS"))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// IsPrivateHost reports whether a hostname resolves (syntactically) to a
// private/internal target.
func IsPrivateHost(host string) bool {
	h := strings.ToLower(strings.Trim(host, "[]"))
	if h == "" {
		return false
	}
	if blockedHostnames[h] {
		return true
	}
	if strings.HasSuffix(h, ".localhost") || strings.HasSuffix(h, ".local") {
		return true
	}
	if addr, err := netip.ParseAddr(h); err == nil {
		for _, prefix := range privatePrefixes {
			if prefix.Contains(addr) {
				return true
			}
		}
		return false
	}
	return false
}

// ValidateUpstreamURL parses and validates an upstream URL for SSRF safety:
// only http/https to non-private hosts is allowed unless
// ALLOW_PRIVATE_UPSTREAMS is on.
func ValidateUpstreamURL(raw string) (*url.URL, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid URL: %s", raw)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("blocked URL scheme: %s", parsed.Scheme)
	}
	host := parsed.Hostname()
	if host == "" {
		return nil, fmt.Errorf("URL has no hostname: %s", raw)
	}
	if !PrivateUpstreamsAllowed() && IsPrivateHost(host) {
		return nil, fmt.Errorf(
			"blocked private/internal host: %s - set ALLOW_PRIVATE_UPSTREAMS=1 on the gateway "+
				"(and restart it) to allow local/private upstreams such as Ollama or another service on the same box.",
			host,
		)
	}
	return parsed, nil
}
