package s3

import (
	"net/netip"
	"testing"
)

// registryV4 and registryV6 are the blocks of the IANA special-purpose
// address registries (www.iana.org/assignments/iana-ipv4-special-registry
// and iana-ipv6-special-registry), written out again here from the registries
// so that the test does not read the table it checks, with the blocks that
// are not unicast, and for IPv6 the part outside 2000::/3.
var registryV4 = []string{
	"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16", "172.16.0.0/12",
	"192.0.0.0/24", "192.0.2.0/24", "192.31.196.0/24", "192.52.193.0/24", "192.88.99.0/24",
	"192.168.0.0/16", "192.175.48.0/24", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24",
	"224.0.0.0/4", "240.0.0.0/4", // multicast; reserved, with the broadcast address 255.255.255.255
	"255.255.255.255/32",
}

var registryV6 = []string{
	"::/128", "::1/128", "::ffff:0:0/96", "64:ff9b::/96", "64:ff9b:1::/48", "100::/64", "100:0:0:1::/64",
	"2001::/23", "2001::/32" /* Teredo */, "2001:1::1/128", "2001:1::2/128", "2001:1::3/128", "2001:2::/48",
	"2001:3::/32", "2001:4:112::/48", "2001:10::/28", "2001:20::/28", "2001:30::/28",
	"2001:db8::/32", "2002::/16", "2620:4f:8000::/48", "3fff::/20", "5f00::/16", "fc00::/7",
	"fe80::/10", "fec0::/10" /* site local, deprecated */, "ff00::/8",
	"::/96",                // IPv4-compatible, deprecated
	"4000::/2", "8000::/1", // the rest of the space outside 2000::/3
}

// last returns the last address of p.
func lastAddr(p netip.Prefix) netip.Addr {
	a := p.Masked().Addr()
	b := a.AsSlice()
	for i := p.Bits(); i < len(b)*8; i++ {
		b[i/8] |= 1 << (7 - i%8)
	}
	out, _ := netip.AddrFromSlice(b)
	return out
}

// TestAllowPublicRefusesEverySpecialBlock guards that AllowPublic passes
// global unicast addresses only: every block of both registries is refused
// at its first and its last address, the addresses just outside each block
// are refused exactly when another block holds them, the prefixes that embed
// an IPv4 address are refused whatever address is inside, and a list of
// public addresses passes.
func TestAllowPublicRefusesEverySpecialBlock(t *testing.T) {
	var blocks []netip.Prefix
	for _, s := range append(append([]string{}, registryV4...), registryV6...) {
		blocks = append(blocks, netip.MustParsePrefix(s))
	}
	inSomeBlock := func(a netip.Addr) bool {
		a = a.Unmap()
		for _, b := range blocks {
			if b.Contains(a) && b.Addr().Is4() == a.Is4() {
				return true
			}
		}
		return false
	}
	for _, b := range blocks {
		first, last := b.Masked().Addr(), lastAddr(b)
		for _, a := range []netip.Addr{first, last} {
			if AllowPublic(a) {
				t.Errorf("AllowPublic(%v) = true, inside %v", a, b)
			}
		}
		for _, a := range []netip.Addr{first.Prev(), last.Next()} {
			if !a.IsValid() {
				continue
			}
			// Outside this block: allowed unless another block holds it or it is
			// outside the space allocated for global unicast.
			want := !inSomeBlock(a) && (a.Unmap().Is4() || netip.MustParsePrefix("2000::/3").Contains(a))
			if got := AllowPublic(a); got != want {
				t.Errorf("AllowPublic(%v), next to %v = %v, want %v", a, b, got, want)
			}
		}
	}
	// Prefixes that embed an IPv4 address: refused for every embedded address,
	// the ones a gateway would forward to metadata and private networks too.
	for _, a := range []string{
		"64:ff9b::a00:1", "64:ff9b::7f00:1", "64:ff9b::a9fe:a9fe", "64:ff9b::808:808", "64:ff9b:1::a00:1", "64:ff9b:1::808:808",
		"2002:a00:1::1", "2002:7f00:1::1", "2002:a9fe:a9fe::1", "2002:808:808::1",
		"::7f00:1", "::a9fe:a9fe", "::808:808", "2001:0:4136:e378:8000:63bf:3fff:fdd2", // Teredo
		"::ffff:7f00:1", "::ffff:10.0.0.1", "::ffff:169.254.169.254",
	} {
		if AllowPublic(netip.MustParseAddr(a)) {
			t.Errorf("AllowPublic(%s) = true: it embeds an IPv4 address", a)
		}
	}
	// Addresses of special blocks that an allow list written by hand tends to miss.
	for _, a := range []string{
		"0.1.2.3", "192.0.0.192", "192.0.2.1", "198.18.0.1", "198.51.100.1", "203.0.113.1", "240.0.0.1",
		"255.255.255.255", "fec0::1", "2001:db8::1", "100::1", "2001::1", "224.0.0.1", "169.254.169.254",
	} {
		if AllowPublic(netip.MustParseAddr(a)) {
			t.Errorf("AllowPublic(%s) = true", a)
		}
	}
	for _, a := range []string{
		"1.1.1.1", "8.8.8.8", "9.255.255.255", "11.0.0.0", "93.184.216.34", "100.63.255.255", "100.128.0.0", "126.255.255.255", "128.0.0.0",
		"172.15.255.255", "172.32.0.0", "191.255.255.255", "192.0.1.0", "192.169.0.0", "198.17.255.255", "198.20.0.0", "223.255.255.255",
		"2606:4700:4700::1111", "2a00:1450:4001:81b::200e", "2001:200::1", "2001:db7:ffff::1", "2001:db9::1", "2003::1",
		"2000::1", "3ffe::1", "3fff:1000::1", "2620:4f:8001::1", "2620:4f:7fff::1", "::ffff:8.8.8.8",
	} {
		if !AllowPublic(netip.MustParseAddr(a)) {
			t.Errorf("AllowPublic(%s) = false, a public address", a)
		}
	}
	if AllowPublic(netip.Addr{}) {
		t.Error("AllowPublic of the zero address = true")
	}
	if AllowPublic(netip.MustParseAddr("fe80::1%eth0")) {
		t.Error("AllowPublic of a zoned link-local address = true")
	}
}
