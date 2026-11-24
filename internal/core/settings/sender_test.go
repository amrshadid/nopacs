package settings

import (
	"errors"
	"net/netip"
	"strings"
	"testing"
)

// TestParseDestinations guards the form of sender.destinations: a host name
// or a range with no host bits, then ports=, private-address and link-local
// each once; no line that names a cloud metadata address;
// a flag only where it can mean something, and no line twice, so every
// accepted value says exactly one thing.
func TestParseDestinations(t *testing.T) {
	for v, ok := range map[string]bool{
		"":                            true,
		"pacs.lab.example":            true,
		"PACS.Lab.Example ports=4242": true,
		"pacs.lab.example ports=104,11112-11120 private-address": true,
		"pacs.lab.example private-address ports=104":             true,
		"10.20.0.0/24 ports=104\n":                               true,
		"10.20.0.0/24\n2001:db8::/32":                            true,
		"192.0.2.7":                                              true,
		"2001:db8::1 ports=104":                                  true,
		"localhost private-address":                              true,
		"meta.lab.example link-local":                            true,
		"meta.lab.example private-address link-local ports=80":   true,
		"meta.lab.example link-local link-local":                 false,
		"169.254.0.0/16 link-local":                              false, // a range needs no flag
		"169.254.169.254":                                        false, // the metadata address
		"fd00:ec2::254":                                          false,
		"10.20.0.1/24":                                           false, // host bits
		"10.20.0.0/33":                                           false,
		"::ffff:10.0.0.1":                                        false, // mapped
		"::ffff:10.0.0.0/104":                                    false,
		"2001:DB8::1":                                            false, // not in its shortest form
		"10.20.0.0/24 private-address":                           false, // a range needs no flag
		"pacs.lab.example ports=":                                false,
		"pacs.lab.example ports=0":                               false,
		"pacs.lab.example ports=104,104":                         false,
		"pacs.lab.example ports=104 ports=105":                   false,
		"pacs.lab.example private-address private-address":       false,
		"pacs.lab.example plain-http":                            false,
		"pacs.lab.example  ports=104":                            false, // two spaces
		"pacs.lab.example\npacs.lab.example":                     false,
		"PACS.lab.example\npacs.lab.example":                     false,
		"10.0.0.0/8\n10.0.0.0/8":                                 false,
		"1.2.3":                                                  false, // a malformed address, not a name
		"-a.example":                                             false,
		"a..example":                                             false,
		"pacs_lab.example":                                       false,
		"https://pacs.lab.example":                               false,
		"pacs.lab.example:104":                                   false,
		"\n":                                                     true,
		"0.0.0.0/0":                                              false, // the whole space
		"::/0":                                                   false,
		"0.0.0.0/1":                                              false, // starts at the unspecified address
		"0.0.0.0":                                                false,
		"::":                                                     false,
		"224.0.0.0/4":                                            false, // multicast
		"224.0.0.1":                                              false,
		"ff02::1":                                                false,
		"255.255.255.255":                                        false, // broadcast
		"0x7f000001":                                             false, // a resolver reads these as addresses
		"127.1":                                                  false,
		"0177.0.0.1":                                             false,
		"2130706433":                                             false,
		"a.0x7f000001":                                           false,
		"a.0x":                                                   false,
		"x0x.example":                                            true,
		"0xg":                                                    true,
		"abc.def":                                                true,
		"10.0.0.0/8":                                             true,
		"1.2.3.4/32":                                             true,
		"a.example\n\nb.example":                                 false,
	} {
		_, err := ParseDestinations(v)
		if (err == nil) != ok {
			t.Errorf("%q: accepted %v, want %v (%v)", v, err == nil, ok, err)
		}
	}
	if _, err := ParseDestinations(strings.Repeat("a.example\n", 257)); err == nil {
		t.Error("257 lines accepted")
	}
	// The text a value is shown as reads back as the same value.
	d, err := ParseDestinations("PACS.lab.example ports=11112-11120,104 private-address\n10.20.0.0/24\n192.0.2.7")
	if err != nil {
		t.Fatal(err)
	}
	want := "pacs.lab.example ports=104,11112-11120 private-address\n10.20.0.0/24\n192.0.2.7/32"
	if d.String() != want {
		t.Errorf("String:\n%s\nwant\n%s", d, want)
	}
	if again, err := ParseDestinations(d.String()); err != nil || again.String() != want {
		t.Errorf("round trip: %v %v", again, err)
	}
}

// TestDestinationsAllowed guards the check a sender is held to at the time
// it is added and at every connection: a host is allowed by its name or, as
// an address, by a range; a name that resolves to a special-purpose address
// is refused unless its line says private-address (loopback, private) or
// link-local (link-local); the cloud metadata addresses are refused
// whatever a line says, a range's included; a port the line leaves
// out is refused; an address spelled as an IPv4-mapped IPv6 address is the
// IPv4 address it holds; and a range never admits a name.
func TestDestinationsAllowed(t *testing.T) {
	d, err := ParseDestinations(strings.Join([]string{
		"pacs.lab.example ports=104,11112-11120",
		"open.lab.example",
		"local.lab.example private-address ports=4242",
		"link.lab.example link-local",
		"both.lab.example private-address link-local",
		"169.254.0.0/16",
		"fd00::/8",
		"10.20.0.0/24 ports=104",
		"192.0.2.7",
		"2001:db8::/32",
	}, "\n"))
	if err != nil {
		t.Fatal(err)
	}
	ip := netip.MustParseAddr
	for _, tc := range []struct {
		name string
		host string
		ip   netip.Addr
		port int
		want error
	}{
		{"a name, not resolved", "pacs.lab.example", netip.Addr{}, 104, nil},
		{"a name in another case", "PACS.Lab.Example", netip.Addr{}, 104, nil},
		{"a name, a port in a range", "pacs.lab.example", netip.Addr{}, 11115, nil},
		{"a name, a port left out", "pacs.lab.example", netip.Addr{}, 105, ErrDestinationPort},
		{"a name not listed", "other.lab.example", netip.Addr{}, 104, ErrDestinationNotListed},
		{"a name that is a suffix of a listed one", "x.pacs.lab.example", netip.Addr{}, 104, ErrDestinationNotListed},
		{"a name resolved to a public address", "pacs.lab.example", ip("8.8.8.8"), 104, nil},
		{"a name resolved to loopback", "pacs.lab.example", ip("127.0.0.1"), 104, ErrDestinationAddress},
		{"a name resolved to the metadata address", "open.lab.example", ip("169.254.169.254"), 80, ErrDestinationAddress},
		{"a name resolved to a private address", "open.lab.example", ip("10.1.2.3"), 104, ErrDestinationAddress},
		{"a name resolved to shared address space", "open.lab.example", ip("100.64.0.1"), 104, ErrDestinationAddress},
		{"a name resolved to an IPv4-mapped loopback", "open.lab.example", ip("::ffff:127.0.0.1"), 104, ErrDestinationAddress},
		{"a name resolved to a unique local address", "open.lab.example", ip("fd00::1"), 104, ErrDestinationAddress},
		{"a name resolved to an IPv6 loopback", "open.lab.example", ip("::1"), 104, ErrDestinationAddress},
		{"a name with private-address, resolved to loopback", "local.lab.example", ip("127.0.0.1"), 4242, nil},
		{"a name with private-address, a port left out", "local.lab.example", ip("127.0.0.1"), 4243, ErrDestinationPort},
		{"an address in a range", "10.20.0.9", netip.Addr{}, 104, nil},
		{"an address in a range, connected to itself", "10.20.0.9", ip("10.20.0.9"), 104, nil},
		{"an address in a range, a port left out", "10.20.0.9", netip.Addr{}, 105, ErrDestinationPort},
		{"an address outside the range", "10.20.1.9", netip.Addr{}, 104, ErrDestinationNotListed},
		{"a single address", "192.0.2.7", netip.Addr{}, 1, nil},
		{"an IPv6 address in a range", "2001:db8::5", netip.Addr{}, 104, nil},
		{"a name is never allowed by a range", "pacs.net.example", ip("10.20.0.9"), 104, ErrDestinationNotListed},
		{"private-address admits a shared-space address", "local.lab.example", ip("100.64.0.1"), 4242, nil},
		{"private-address does not admit a link-local address", "local.lab.example", ip("169.254.10.1"), 4242, ErrDestinationAddress},
		{"private-address does not admit an IPv6 link-local address", "local.lab.example", ip("fe80::1"), 4242, ErrDestinationAddress},
		{"private-address does not admit the IPv6 metadata address", "local.lab.example", ip("fd00:ec2::254"), 4242, ErrDestinationAddress},
		{"link-local admits a link-local address", "link.lab.example", ip("169.254.10.1"), 80, nil},
		{"link-local admits an IPv6 link-local address", "link.lab.example", ip("fe80::1"), 80, nil},
		{"link-local does not admit a private address", "link.lab.example", ip("10.1.2.3"), 80, ErrDestinationAddress},
		{"link-local never admits the metadata address", "link.lab.example", ip("169.254.169.254"), 80, ErrDestinationAddress},
		{"link-local never admits an IPv4-mapped metadata address", "link.lab.example", ip("::ffff:169.254.169.254"), 80, ErrDestinationAddress},
		{"both flags never admit the IPv6 metadata address", "both.lab.example", ip("fd00:ec2::254"), 80, ErrDestinationAddress},
		{"a range never admits the metadata address", "169.254.169.254", netip.Addr{}, 80, ErrDestinationAddress},
		{"a range admits its other link-local addresses", "169.254.1.1", netip.Addr{}, 80, nil},
		{"a range never admits the IPv6 metadata address", "fd00:ec2::254", netip.Addr{}, 80, ErrDestinationAddress},
		{"private-address admits a unique local address", "local.lab.example", ip("fd00::1"), 4242, nil},
		{"private-address admits a private address", "local.lab.example", ip("192.168.1.1"), 4242, nil},
		{"private-address admits a public address", "local.lab.example", ip("8.8.8.8"), 4242, nil},
		{"private-address does not admit documentation space", "local.lab.example", ip("192.0.2.1"), 4242, ErrDestinationAddress},
		{"private-address does not admit benchmarking space", "local.lab.example", ip("198.18.0.1"), 4242, ErrDestinationAddress},
		{"private-address does not admit reserved space", "local.lab.example", ip("240.0.0.1"), 4242, ErrDestinationAddress},
		{"private-address does not admit the unspecified address", "local.lab.example", ip("0.0.0.0"), 4242, ErrDestinationAddress},
		{"private-address does not admit the unspecified IPv6 address", "local.lab.example", ip("::"), 4242, ErrDestinationAddress},
		{"private-address does not admit multicast", "local.lab.example", ip("224.0.0.1"), 4242, ErrDestinationAddress},
		{"private-address does not admit IPv6 multicast", "local.lab.example", ip("ff02::1"), 4242, ErrDestinationAddress},
		{"private-address does not admit broadcast", "local.lab.example", ip("255.255.255.255"), 4242, ErrDestinationAddress},
		{"private-address does not admit NAT64", "local.lab.example", ip("64:ff9b::a9fe:a9fe"), 4242, ErrDestinationAddress},
		{"a port that is not one", "pacs.lab.example", netip.Addr{}, 0, ErrDestinationNotListed},
		{"a port past 65535", "open.lab.example", netip.Addr{}, 65536, ErrDestinationNotListed},
	} {
		if got := d.Allowed(tc.host, tc.ip, tc.port); !errors.Is(got, tc.want) || (tc.want == nil) != (got == nil) {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
	if err := (Destinations{}).Allowed("pacs.lab.example", netip.Addr{}, 104); !errors.Is(err, ErrDestinationNotListed) {
		t.Errorf("no destinations listed: %v", err)
	}
}

// TestSenderSettingsRules guards the bounds and defaults of the other
// sender settings and of job.failures-kept at each edge and one past it.
func TestSenderSettingsRules(t *testing.T) {
	tab := MustBuiltin()
	check := func(name string, accept, refuse []string) {
		t.Helper()
		d, ok := tab.Lookup(name)
		if !ok {
			t.Fatalf("%s is not built in", name)
		}
		for _, v := range accept {
			if err := d.Validate(v); err != nil {
				t.Errorf("%s %q: %v", name, v, err)
			}
		}
		for _, v := range refuse {
			if err := d.Validate(v); err == nil {
				t.Errorf("%s %q accepted", name, v)
			}
		}
	}
	check(SenderMaxAssociations, []string{"1", "32", "1024"}, []string{"0", "1025", "", "-1", "032", "1.5", " 8"})
	check(SenderTenantMaxAssociations, []string{"1", "8", "1024"}, []string{"0", "1025", "", "08", "eight"})
	check(JobFailuresKept, []string{"100", "10000", "1000000"}, []string{"99", "1000001", "", "0", "1e4", "010000"})
	check(SenderAllowPlainHTTP, []string{"true", "false"}, []string{"", "True", "1", "yes", "false "})
	if d, _ := tab.Lookup(SenderTenantMaxAssociations); d.TenantOnly || !d.TenantOverride || !d.TenantSetByInstallation {
		t.Errorf("the tenant's share is set on a tenant by the installation, with an installation default: %+v", d)
	}
	if d, _ := tab.Lookup(JobFailuresKept); d.Kind() != Ordinary {
		t.Errorf("job.failures-kept is ordinary: %v", d.Kind())
	}
}
