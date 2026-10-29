package ipacl

import (
	"fmt"
	"net/netip"
	"testing"
)

func BenchmarkAllowedEmpty(b *testing.B) {
	rules, err := Compile(nil, nil)
	if err != nil {
		b.Fatal(err)
	}
	addr := netip.MustParseAddr("192.0.2.1")
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if !rules.Allowed(addr) {
			b.Fatal("empty rules denied")
		}
	}
}

func BenchmarkAllowedWhitelist(b *testing.B) {
	for _, count := range []int{10, 100, 1000, 4096} {
		b.Run(fmt.Sprintf("entries_%d", count), func(b *testing.B) {
			entries := make([]string, count)
			for i := range entries {
				entries[i] = fmt.Sprintf("10.%d.%d.0/24", i/256, i%256)
			}
			rules, err := Compile(entries, nil)
			if err != nil {
				b.Fatal(err)
			}
			// Worst case: the address matches the last prefix, so the scan
			// visits every entry.
			addr := netip.MustParseAddr(fmt.Sprintf("10.%d.%d.5", (count-1)/256, (count-1)%256))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if !rules.Allowed(addr) {
					b.Fatal("whitelisted address denied")
				}
			}
		})
	}
}

func BenchmarkStoreLoad(b *testing.B) {
	rules, err := Compile([]string{"192.0.2.0/24"}, []string{"198.51.100.9"})
	if err != nil {
		b.Fatal(err)
	}
	store := NewStore(rules)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if !store.Load().Active() {
			b.Fatal("rules disappeared")
		}
	}
}
