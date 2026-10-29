package ipacl

import (
	"net/netip"
	"reflect"
	"testing"
)

func TestNormalize(t *testing.T) {
	got, err := Normalize([]string{
		" 10.0.0.0/8 ",
		"10.0.0.0/8", // duplicate after canonicalization
		"192.168.1.5",
		"::ffff:192.0.2.1",
		"2001:db8::/32",
		"", // blank lines from the UI
	})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	want := []string{"10.0.0.0/8", "192.168.1.5", "192.0.2.1", "2001:db8::/32"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("normalized = %v, want %v", got, want)
	}

	if _, err := Normalize([]string{"not-an-ip"}); err == nil {
		t.Fatal("invalid entry was accepted")
	}
}

func TestRulesAllowed(t *testing.T) {
	addr := func(value string) netip.Addr { return netip.MustParseAddr(value) }

	empty, err := Compile(nil, nil)
	if err != nil {
		t.Fatalf("compile empty: %v", err)
	}
	if empty.Active() {
		t.Fatal("empty rules report active")
	}
	if !empty.Allowed(addr("198.51.100.1")) {
		t.Fatal("empty rules denied an address")
	}

	allow, err := Compile([]string{"192.0.2.0/24", "2001:db8::/32"}, nil)
	if err != nil {
		t.Fatalf("compile whitelist: %v", err)
	}
	if !allow.Allowed(addr("192.0.2.10")) || !allow.Allowed(addr("2001:db8::1")) {
		t.Fatal("whitelisted address was denied")
	}
	if allow.Allowed(addr("192.0.3.10")) || allow.Allowed(addr("2001:db9::1")) {
		t.Fatal("non-whitelisted address was allowed")
	}

	deny, err := Compile(nil, []string{"192.0.2.10", "2001:db8::/32"})
	if err != nil {
		t.Fatalf("compile blacklist: %v", err)
	}
	if deny.Allowed(addr("192.0.2.10")) || deny.Allowed(addr("2001:db8::1")) {
		t.Fatal("blacklisted address was allowed")
	}
	if !deny.Allowed(addr("192.0.2.11")) {
		t.Fatal("non-blacklisted address was denied")
	}

	both, err := Compile([]string{"192.0.2.0/24"}, []string{"192.0.2.10"})
	if err != nil {
		t.Fatalf("compile both: %v", err)
	}
	if both.Allowed(addr("192.0.2.10")) {
		t.Fatal("blacklist did not override the whitelist")
	}
	if !both.Allowed(addr("192.0.2.11")) {
		t.Fatal("whitelisted address was denied")
	}

	mapped, err := Compile([]string{"::ffff:192.0.2.0/120"}, nil)
	if err != nil {
		t.Fatalf("compile mapped CIDR: %v", err)
	}
	if !mapped.Allowed(addr("192.0.2.77")) {
		t.Fatal("IPv4-mapped whitelist did not match plain IPv4")
	}
}

func TestStoreSwap(t *testing.T) {
	store := NewStore(nil)
	if store.Load().Active() {
		t.Fatal("new store is active")
	}

	rules, err := Compile([]string{"192.0.2.0/24"}, nil)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	store.Store(rules)
	if !store.Load().Allowed(netip.MustParseAddr("192.0.2.5")) {
		t.Fatal("swapped rules were not visible")
	}
	store.Store(nil)
	if store.Load().Active() {
		t.Fatal("nil swap did not reset the rules")
	}
}
