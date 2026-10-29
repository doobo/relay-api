package middleware

import (
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strings"

	"relay-api/internal/httperr"
	"relay-api/internal/ipacl"
	"relay-api/internal/util"
)

// IPAccess enforces the gateway-wide allow/deny lists. A non-empty whitelist
// restricts access to its entries; a blacklist always denies its entries. Loop-
// back callers are exempt so local administration cannot lock itself out.
//
// The loopback exemption is decided from the TCP peer, never from a forwarded
// header alone: a remote caller cannot claim CF-Connecting-IP: 127.0.0.1 to
// bypass the lists. When the TCP peer really is loopback (a local reverse
// proxy), the resolved client address is still filtered, so ACLs keep working
// behind a local proxy.
func IPAccess(store *ipacl.Store) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rules := store.Load()
			if !rules.Active() {
				next.ServeHTTP(w, r)
				return
			}

			client := util.ClientIP(r)
			addr, ok := parseIP(client)
			// A loopback TCP peer is exempt unless it is a local proxy that
			// forwarded a non-loopback client address.
			if isLoopbackPeer(r) && (!ok || addr.IsLoopback()) {
				next.ServeHTTP(w, r)
				return
			}
			if !ok || !rules.Allowed(addr) {
				slog.Warn("ip access denied",
					"client_ip", client,
					"remote_addr", r.RemoteAddr,
					"path", r.URL.Path,
				)
				httperr.Write(w, http.StatusForbidden,
					"Client IP is not allowed", "permission_error", "ip_forbidden")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// isLoopbackPeer reports whether the actual TCP peer is local. A local peer
// that forwards a non-loopback client address is a proxy request and is still
// filtered by the caller.
func isLoopbackPeer(r *http.Request) bool {
	peer, ok := parseIP(remoteHost(r.RemoteAddr))
	return ok && peer.IsLoopback()
}

func remoteHost(remoteAddr string) string {
	host, _, err := net.SplitHostPort(strings.TrimSpace(remoteAddr))
	if err == nil {
		return host
	}
	return strings.TrimSpace(remoteAddr)
}

// parseIP accepts a bare address, an address with a port, or a bracketed IPv6
// address, and normalizes IPv4-mapped IPv6 to plain IPv4.
func parseIP(raw string) (netip.Addr, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return netip.Addr{}, false
	}
	if strings.HasPrefix(raw, "[") && strings.HasSuffix(raw, "]") {
		raw = raw[1 : len(raw)-1]
	}
	if addr, err := netip.ParseAddr(raw); err == nil {
		return addr.Unmap(), true
	}
	if host, _, err := net.SplitHostPort(raw); err == nil {
		if addr, err := netip.ParseAddr(host); err == nil {
			return addr.Unmap(), true
		}
	}
	return netip.Addr{}, false
}
