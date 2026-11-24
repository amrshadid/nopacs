package tenant

import (
	"net/netip"
	"testing"
)

// TestAdmitsOnlyWhatARuleMatches guards the admission rule alone: any
// caller only when said; an empty list admits nobody; a rule matches when
// every part it names matches, the title exactly with its case, the address
// inside the range; an address that cannot be read matches no range; an
// IPv4 caller carried as IPv6 is compared as IPv4.
func TestAdmitsOnlyWhatARuleMatches(t *testing.T) {
	v4 := netip.MustParseAddr("192.0.2.7")
	v6 := netip.MustParseAddr("2001:db8::7")
	both := DIMSEReceiver{Callers: []CallerRule{{CallingAETitle: "PRISMA1", AddressRange: "192.0.2.0/24"}}}
	cases := []struct {
		name string
		d    DIMSEReceiver
		ae   string
		addr netip.Addr
		want bool
	}{
		{"any caller", DIMSEReceiver{AnyCaller: true}, "WHOEVER", netip.Addr{}, true},
		{"no rule", DIMSEReceiver{}, "PRISMA1", v4, false},
		{"title and range", both, "PRISMA1", v4, true},
		{"title in another case", both, "prisma1", v4, false},
		{"title outside the range", both, "PRISMA1", netip.MustParseAddr("198.51.100.1"), false},
		{"title, no address", both, "PRISMA1", netip.Addr{}, false},
		{"range only", DIMSEReceiver{Callers: []CallerRule{{AddressRange: "2001:db8::/48"}}}, "ANY", v6, true},
		{"an IPv4 caller and an IPv6 range", DIMSEReceiver{Callers: []CallerRule{{AddressRange: "2001:db8::/48"}}}, "ANY", v4, false},
		{"title only", DIMSEReceiver{Callers: []CallerRule{{CallingAETitle: "CT"}}}, "CT", netip.Addr{}, true},
		{"an IPv4 caller seen as IPv6 by a listener on both families", DIMSEReceiver{Callers: []CallerRule{{AddressRange: "192.0.2.0/24"}}}, "ANY",
			netip.MustParseAddr("::ffff:192.0.2.7"), true},
		{"title with a trailing space", both, "PRISMA1 ", v4, false},
		{"second rule", DIMSEReceiver{Callers: []CallerRule{{CallingAETitle: "CT"}, {CallingAETitle: "MR"}}}, "MR", v4, true},
	}
	for _, c := range cases {
		if got := c.d.Admits(c.ae, c.addr); got != c.want {
			t.Errorf("%s: admits %v, want %v", c.name, got, c.want)
		}
	}
}
