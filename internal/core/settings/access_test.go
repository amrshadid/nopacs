package settings

import (
	"net/netip"
	"testing"
	"time"
)

// TestParseNetworks guards the one written form of a list of networks: each
// address or CIDR network in its canonical spelling, no host bits past the
// prefix, no mapped or zoned address, no repeat, and the round trip through
// String.
func TestParseNetworks(t *testing.T) {
	for v, ok := range map[string]bool{
		"":                         true,
		"10.0.0.0/8":               true,
		"192.0.2.7":                true,
		"2001:db8::/32":            true,
		"2001:db8::1":              true,
		"10.0.0.0/8,192.0.2.7,::1": true,
		"0.0.0.0/0,::/0":           true,
		"10.1.2.3/8":               false,
		"192.0.2.7/32":             false,
		"2001:db8::1/128":          false,
		"2001:DB8::/32":            false,
		"::ffff:192.0.2.7":         false,
		"fe80::1%eth0":             false,
		"10.0.0.0/8, 192.0.2.7":    false,
		"10.0.0.0/8,10.0.0.0/8":    false,
		"10.0.0.0/33":              false,
		"host.example":             false,
		"10.0.0.0/8,":              false,
		"010.0.0.0/8":              false,
	} {
		n, err := ParseNetworks(v)
		if (err == nil) != ok {
			t.Errorf("%q: accepted %v, want %v (%v)", v, err == nil, ok, err)
			continue
		}
		if ok && n.String() != v {
			t.Errorf("%q reads back as %q", v, n.String())
		}
	}
}

// TestNetworksContains guards the match of an address: inside and outside a
// network, an IPv4-mapped IPv6 address matched as its IPv4 address, and the
// empty list holding nothing.
func TestNetworksContains(t *testing.T) {
	n, err := ParseNetworks("10.0.0.0/8,2001:db8::/32,192.0.2.7")
	if err != nil {
		t.Fatal(err)
	}
	for a, want := range map[string]bool{
		"10.9.8.7": true, "11.0.0.1": false, "192.0.2.7": true, "192.0.2.8": false,
		"2001:db8::5": true, "2001:db9::5": false, "::ffff:10.1.1.1": true, "::ffff:11.1.1.1": false,
	} {
		if got := n.Contains(netip.MustParseAddr(a)); got != want {
			t.Errorf("Contains(%s) = %v, want %v", a, got, want)
		}
	}
	if (Networks(nil)).Contains(netip.MustParseAddr("10.0.0.1")) {
		t.Error("the empty list holds an address")
	}
}

// TestAccessBounds guards each access setting's rule at its bounds and one
// past them.
func TestAccessBounds(t *testing.T) {
	tab := MustBuiltin()
	cases := map[string]map[string]bool{
		AccessFailureLimit:  {"1": true, "10000": true, "0": false, "10001": false, "01": false, "-1": false, "": false},
		AccessFailureWindow: {"1s": true, "24h": true, "1d": true, "0s": false, "25h": false, "2d": false, "1m": true},
		AccessBlockTime:     {"1s": true, "7d": true, "8d": false, "0m": false, "15m": true},
		KeyMaxLifetime:      {"1h": true, "3650d": true, "59m": false, "3651d": false},
		KeyExpiryWarning:    {"1h": true, "365d": true, "7d": true, "59m": false, "366d": false},
		// One header, written one way: no list, no other case, no other
		// header name.
		AccessForwardedHeader: {"x-forwarded-for": true, "forwarded": true, "X-Forwarded-For": false, "Forwarded": false,
			"forwarded,x-forwarded-for": false, "x-real-ip": false, "": false},
	}
	for name, values := range cases {
		d, ok := tab.Lookup(name)
		if !ok {
			t.Fatalf("%s is not built in", name)
		}
		for v, want := range values {
			if err := d.Validate(v); (err == nil) != want {
				t.Errorf("%s=%q: accepted %v, want %v (%v)", name, v, err == nil, want, err)
			}
		}
	}
	if d, _ := ParseFailureWindow("1m"); d != time.Minute {
		t.Errorf("1m is %v", d)
	}
}

// TestParseDurationUnits guards the four units and the forms refused.
func TestParseDurationUnits(t *testing.T) {
	for v, want := range map[string]time.Duration{"30s": 30 * time.Second, "15m": 15 * time.Minute, "2h": 2 * time.Hour, "3d": 72 * time.Hour} {
		if d, err := ParseDuration(v); err != nil || d != want {
			t.Errorf("%q = %v, %v; want %v", v, d, err, want)
		}
	}
	for _, v := range []string{"1ms", "1w", "1M", "1.5h", "1h30m", "0s", "s"} {
		if _, err := ParseDuration(v); err == nil {
			t.Errorf("%q accepted", v)
		}
	}
}
