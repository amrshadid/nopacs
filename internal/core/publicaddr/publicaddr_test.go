package publicaddr

import (
	"net/netip"
	"testing"
)

// TestIs guards the rule at its edges: a global unicast address passes, and
// the loopback, private, shared, link-local (the clouds' metadata address),
// documentation, multicast, unique local and embedded-IPv4 blocks do not,
// whether written as IPv4, IPv6 or IPv4-mapped.
func TestIs(t *testing.T) {
	for s, want := range map[string]bool{
		"8.8.8.8":              true,
		"2606:4700:4700::1111": true,
		"127.0.0.1":            false,
		"10.0.0.1":             false,
		"172.16.0.1":           false,
		"192.168.1.1":          false,
		"100.64.0.1":           false,
		"169.254.169.254":      false,
		"192.0.2.1":            false,
		"224.0.0.1":            false,
		"0.0.0.0":              false,
		"::1":                  false,
		"::":                   false,
		"fd00::1":              false,
		"fe80::1":              false,
		"64:ff9b::a9fe:a9fe":   false,
		"2001:db8::1":          false,
		"::ffff:8.8.8.8":       true,
		"::ffff:127.0.0.1":     false,
	} {
		if got := Is(netip.MustParseAddr(s)); got != want {
			t.Errorf("%s: %v, want %v", s, got, want)
		}
	}
	if Is(netip.Addr{}) {
		t.Error("the zero address passes")
	}
}
