// Package publicaddr says whether an address is a global unicast address a
// server may connect to on a stranger's word: the one rule every part of the
// server that connects where a person pointed it (an S3 storage's endpoint,
// a sender's peer) holds an address to, written once. It is part of the
// core: it reads no clock, file or network.
package publicaddr

import "net/netip"

// The special-purpose address blocks Is refuses. They are the
// entries of the IANA IPv4 Special-Purpose Address Registry
// (www.iana.org/assignments/iana-ipv4-special-registry) and of the IPv6
// Special-Purpose Address Registry
// (www.iana.org/assignments/iana-ipv6-special-registry), whether or not an
// entry is marked globally reachable (an anycast service such as AS112 is
// no storage), with the blocks that are not unicast.
var (
	// refusedV4 is every IPv4 block of the registry, and 224.0.0.0/3,
	// which is multicast (224.0.0.0/4) and the rest of the reserved space
	// (240.0.0.0/4, which holds the broadcast address).
	refusedV4 = mustPrefixes(
		"0.0.0.0/8",       // "This network" (RFC 791)
		"10.0.0.0/8",      // private use (RFC 1918)
		"100.64.0.0/10",   // shared address space (RFC 6598)
		"127.0.0.0/8",     // loopback (RFC 1122)
		"169.254.0.0/16",  // link local, with the clouds' metadata services (RFC 3927)
		"172.16.0.0/12",   // private use (RFC 1918)
		"192.0.0.0/24",    // IETF protocol assignments (RFC 6890)
		"192.0.2.0/24",    // documentation, TEST-NET-1 (RFC 5737)
		"192.31.196.0/24", // AS112-v4 (RFC 7535)
		"192.52.193.0/24", // AMT (RFC 7450)
		"192.88.99.0/24",  // deprecated 6to4 relay anycast (RFC 7526)
		"192.168.0.0/16",  // private use (RFC 1918)
		"192.175.48.0/24", // direct delegation AS112 service (RFC 7534)
		"198.18.0.0/15",   // benchmarking (RFC 2544)
		"198.51.100.0/24", // documentation, TEST-NET-2 (RFC 5737)
		"203.0.113.0/24",  // documentation, TEST-NET-3 (RFC 5737)
		"224.0.0.0/3",     // multicast (RFC 5771), reserved (RFC 1112), broadcast (RFC 919)
	)
	// globalV6 is the part of the IPv6 space allocated for global unicast
	// (2000::/3, RFC 4291); everything outside it is refused, which covers
	// the unspecified and loopback addresses, the IPv4-compatible block
	// (::/96), NAT64 (64:ff9b::/96 and 64:ff9b:1::/48), the discard prefixes
	// (100::/64, 100:0:0:1::/64), unique local (fc00::/7), link local
	// (fe80::/10), site local (fec0::/10), SRv6 (5f00::/16) and multicast
	// (ff00::/8).
	globalV6 = netip.MustParsePrefix("2000::/3")
	// refusedV6 is every block of the IPv6 registry inside 2000::/3.
	refusedV6 = mustPrefixes(
		"2001::/23",         // IETF protocol assignments: Teredo (2001::/32), benchmarking, AMT, AS112, ORCHID (RFC 6890)
		"2001:db8::/32",     // documentation (RFC 3849)
		"2002::/16",         // 6to4, which embeds an IPv4 address (RFC 3056)
		"2620:4f:8000::/48", // direct delegation AS112 service (RFC 7534)
		"3fff::/20",         // documentation (RFC 9637)
	)
)

func mustPrefixes(s ...string) []netip.Prefix {
	out := make([]netip.Prefix, len(s))
	for i, p := range s {
		out[i] = netip.MustParsePrefix(p)
	}
	return out
}

// Is reports whether a is a global unicast address, which passes and nothing
// else does: it refuses every block of the IANA special-purpose
// registries (see refusedV4 and refusedV6), the multicast and reserved
// space, and every IPv6 address outside 2000::/3. The IPv6 prefixes that
// embed an IPv4 address (NAT64, 6to4, Teredo, the IPv4-compatible block)
// are refused outright, without judging the address inside: a NAT64
// gateway forwards 64:ff9b::a9fe:a9fe to 169.254.169.254. An address mapped
// into IPv6 (::ffff:a.b.c.d) is judged as the IPv4 address it holds.
func Is(a netip.Addr) bool {
	a = a.Unmap().WithZone("")
	switch {
	case !a.IsValid():
		return false
	case a.Is4():
		for _, p := range refusedV4 {
			if p.Contains(a) {
				return false
			}
		}
		return true
	}
	if !globalV6.Contains(a) {
		return false
	}
	for _, p := range refusedV6 {
		if p.Contains(a) {
			return false
		}
	}
	return true
}
