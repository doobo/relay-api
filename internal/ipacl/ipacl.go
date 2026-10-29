// Package ipacl implements the gateway's IP allow/deny rules. Entries are
// single addresses or CIDR prefixes (IPv4 or IPv6); an empty list means "no
// rule", so the default configuration allows every caller.
package ipacl

import (
	"fmt"
	"net/netip"
	"strings"
	"sync/atomic"
)

// Rules is a compiled pair of address lists. It is immutable after Compile, so
// requests can read it concurrently.
type Rules struct {
	whitelist []netip.Prefix
	blacklist []netip.Prefix
}

// Normalize trims, validates, canonicalizes and de-duplicates a list of
// addresses/CIDRs while preserving input order. Empty entries (blank lines in
// the UI) are ignored. The returned strings are what the settings API stores.
func Normalize(entries []string) ([]string, error) {
	_, normalized, err := parseEntries(entries)
	if err != nil {
		return nil, err
	}
	return normalized, nil
}

// Compile validates both lists and builds the matcher used by the middleware.
func Compile(whitelist, blacklist []string) (*Rules, error) {
	allow, _, err := parseEntries(whitelist)
	if err != nil {
		return nil, err
	}
	deny, _, err := parseEntries(blacklist)
	if err != nil {
		return nil, err
	}
	return &Rules{whitelist: allow, blacklist: deny}, nil
}

// Active reports whether either list has at least one entry. When it is false
// the middleware can skip address parsing entirely.
func (r *Rules) Active() bool {
	return r != nil && (len(r.whitelist) > 0 || len(r.blacklist) > 0)
}

// Allowed applies the lists to one address:
//   - a non-empty whitelist requires the address to match it;
//   - a non-empty blacklist denies a matching address, even if it was also
//     whitelisted, so an explicit deny always wins.
//
// Callers handle loopback bypass separately; this method is deliberately pure.
func (r *Rules) Allowed(addr netip.Addr) bool {
	if r == nil {
		return true
	}
	addr = addr.Unmap()
	if len(r.whitelist) > 0 && !contains(r.whitelist, addr) {
		return false
	}
	if len(r.blacklist) > 0 && contains(r.blacklist, addr) {
		return false
	}
	return true
}

func contains(prefixes []netip.Prefix, addr netip.Addr) bool {
	for _, prefix := range prefixes {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

func parseEntries(entries []string) ([]netip.Prefix, []string, error) {
	prefixes := make([]netip.Prefix, 0, len(entries))
	normalized := make([]string, 0, len(entries))
	seen := make(map[string]struct{}, len(entries))
	for _, raw := range entries {
		entry := strings.TrimSpace(raw)
		if entry == "" {
			continue
		}
		prefix, err := parseEntry(entry)
		if err != nil {
			return nil, nil, err
		}
		// Keep a plain address as a plain address; only CIDR input is written
		// back with a prefix length. Both forms compile to the same matcher.
		key := prefix.String()
		text := key
		if !strings.Contains(entry, "/") {
			text = prefix.Addr().String()
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		prefixes = append(prefixes, prefix)
		normalized = append(normalized, text)
	}
	return prefixes, normalized, nil
}

func parseEntry(entry string) (netip.Prefix, error) {
	if strings.Contains(entry, "/") {
		prefix, err := netip.ParsePrefix(entry)
		if err != nil {
			return netip.Prefix{}, fmt.Errorf("invalid IP or CIDR %q", entry)
		}
		return normalizePrefix(prefix), nil
	}
	addr, err := netip.ParseAddr(entry)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("invalid IP or CIDR %q", entry)
	}
	addr = addr.Unmap()
	return netip.PrefixFrom(addr, addr.BitLen()), nil
}

// normalizePrefix masks host bits and rewrites IPv4-mapped IPv6 prefixes as
// plain IPv4, so ::ffff:192.0.2.0/120 matches the same addresses as
// 192.0.2.0/24.
func normalizePrefix(prefix netip.Prefix) netip.Prefix {
	prefix = prefix.Masked()
	addr := prefix.Addr()
	if addr.Is4In6() && prefix.Bits() >= 96 {
		return netip.PrefixFrom(addr.Unmap(), prefix.Bits()-96).Masked()
	}
	return prefix
}

// Store swaps compiled rules atomically so a Settings save takes effect for the
// next request without restarting the gateway.
type Store struct {
	rules atomic.Pointer[Rules]
}

// NewStore wraps rules; nil becomes an empty (allow-all) rule set.
func NewStore(rules *Rules) *Store {
	store := &Store{}
	store.Store(rules)
	return store
}

// Load returns the current rules. It never returns nil.
func (s *Store) Load() *Rules {
	if s == nil {
		return &Rules{}
	}
	if rules := s.rules.Load(); rules != nil {
		return rules
	}
	return &Rules{}
}

// Store replaces the current rules; nil becomes allow-all.
func (s *Store) Store(rules *Rules) {
	if rules == nil {
		rules = &Rules{}
	}
	s.rules.Store(rules)
}
