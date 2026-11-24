package settings

import (
	"errors"
	"net/netip"
	"strconv"
	"strings"
	"time"
)

// The settings of who may reach the API from where, and how long a key
// lives. Each is read on the request path by the server's front step, which
// keeps a copy for a few seconds at most, so a change applies within that
// time without a restart.
const (
	// AccessTrustedProxies is the networks of the proxies and load
	// balancers whose forwarded client address (Forwarded, RFC 7239, or
	// X-Forwarded-For) the server believes (ParseNetworks). A request from
	// any other address is recorded under its own address, whatever its
	// headers say. Installation-only.
	AccessTrustedProxies = "access.trusted-proxies"
	// AccessForwardedHeader names the one header a trusted proxy's
	// forwarded client address is read from: "x-forwarded-for" (the
	// default) or "forwarded" (RFC 7239) (ParseForwardedHeader). The other
	// header is ignored, never merged nor preferred: a proxy commonly
	// appends to one of them and passes a client's copy of the other
	// through unread, so believing the other would let a client write its
	// own address. Installation-only.
	AccessForwardedHeader = "access.forwarded-header"
	// AccessAllowedNetworks is the networks a request off the local socket
	// may come from; empty for every network (ParseNetworks).
	// Installation-only.
	AccessAllowedNetworks = "access.allowed-networks"
	// AccessDeniedNetworks is the networks a request off the local socket
	// is refused from, before anything else is looked at (ParseNetworks).
	// Installation-only.
	AccessDeniedNetworks = "access.denied-networks"
	// AccessTenantNetworks is the networks every key used on a tenant must
	// be used from; empty for every network (ParseNetworks). Ordinary: a
	// tenant's admin sets the tenant's own, and the installation's value is
	// the default of every tenant that sets none.
	AccessTenantNetworks = "access.tenant-networks"
	// AccessFailureLimit is how many failed calls an address (bad or
	// unknown keys) or a key (refused for its grants) may make within
	// AccessFailureWindow before its refusals are slowed; at twice as many
	// it is blocked for AccessBlockTime (ParseFailureLimit).
	// Installation-only.
	AccessFailureLimit = "access.failure-limit"
	// AccessFailureWindow is the time over which failed calls are counted
	// (ParseFailureWindow). Installation-only.
	AccessFailureWindow = "access.failure-window"
	// AccessBlockTime is how long an address or a key that reached twice
	// AccessFailureLimit is refused (ParseBlockTime). Installation-only.
	AccessBlockTime = "access.block-time"
	// KeyMaxLifetime is the longest a key may be made to live, by its
	// maker or by a rotation (ParseKeyLifetime). A key without an expiry is
	// asked for in so many words and is not bound by it. Installation-only.
	KeyMaxLifetime = "key.max-lifetime"
	// KeyExpiryWarning is how long before its expiry a key is reported as
	// expiring, in the status and as an event (ParseExpiryWarning).
	// Installation-only.
	KeyExpiryWarning = "key.expiry-warning"
)

// The bounds of the access settings.
const (
	// MaxNetworks bounds the networks one value lists: far more than any
	// site writes by hand, and few enough that an address is compared with
	// every one on each request at no measurable cost.
	MaxNetworks = 256
	// MaxFailureLimit bounds access.failure-limit.
	MaxFailureLimit = 10000
	// MinFailureWindow and MaxFailureWindow bound access.failure-window.
	MinFailureWindow = time.Second
	MaxFailureWindow = 24 * time.Hour
	// MinBlockTime and MaxBlockTime bound access.block-time.
	MinBlockTime = time.Second
	MaxBlockTime = 7 * 24 * time.Hour
	// MinExpiryWarning and MaxExpiryWarning bound key.expiry-warning.
	MinExpiryWarning = time.Hour
	MaxExpiryWarning = 365 * 24 * time.Hour
)

// accessDefinitions returns the definitions of the access settings, for
// Builtin.
func accessDefinitions() []Definition {
	networks := func(v string) error { _, err := ParseNetworks(v); return err }
	return []Definition{{
		// Empty by default: no forwarded address is believed until the
		// installation's admin names the proxies in front of the server.
		Name: AccessTrustedProxies, Default: "", Validate: networks,
	}, {
		// X-Forwarded-For is the header most proxies and load balancers
		// append to by default.
		Name: AccessForwardedHeader, Default: string(ForwardedXFF),
		Validate: func(v string) error { _, err := ParseForwardedHeader(v); return err },
	}, {
		Name: AccessAllowedNetworks, Default: "", Validate: networks,
	}, {
		Name: AccessDeniedNetworks, Default: "", Validate: networks,
	}, {
		Name: AccessTenantNetworks, Default: "", TenantOverride: true, Validate: networks,
	}, {
		// Ten failures a minute is far above a client that mistyped its
		// key and far below what guessing needs.
		Name: AccessFailureLimit, Default: "10",
		Validate: func(v string) error { _, err := ParseFailureLimit(v); return err },
	}, {
		Name: AccessFailureWindow, Default: "1m",
		Validate: func(v string) error { _, err := ParseFailureWindow(v); return err },
	}, {
		Name: AccessBlockTime, Default: "15m",
		Validate: func(v string) error { _, err := ParseBlockTime(v); return err },
	}, {
		Name: KeyMaxLifetime, Default: "3650d",
		Validate: func(v string) error { _, err := ParseKeyLifetime(v); return err },
	}, {
		Name: KeyExpiryWarning, Default: "7d",
		Validate: func(v string) error { _, err := ParseExpiryWarning(v); return err },
	}}
}

// Networks is a list of IP networks, as the access settings and a key's
// networks hold them. The empty list holds no address; what an empty list
// means (every network, or none) is said by each setting.
type Networks []netip.Prefix

// Contains reports whether a is in one of the networks. An IPv4 address
// written in its IPv4-mapped IPv6 form (::ffff:192.0.2.1), as a socket that
// listens on both families reports it, is compared as the IPv4 address it
// stands for, so a network written in IPv4 matches it.
func (n Networks) Contains(a netip.Addr) bool {
	a = a.Unmap().WithZone("")
	for _, p := range n {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// String returns the value as ParseNetworks reads it.
func (n Networks) String() string {
	terms := make([]string, len(n))
	for i, p := range n {
		terms[i] = FormatNetwork(p)
	}
	return strings.Join(terms, ",")
}

// FormatNetwork writes one network as ParseNetworks reads it: a single
// address (a /32 for IPv4, a /128 for IPv6) as the address alone, any other
// as address/bits.
func FormatNetwork(p netip.Prefix) string {
	if p.IsSingleIP() {
		return p.Addr().String()
	}
	return p.String()
}

// ParseNetworks reads a list of networks: terms separated by commas, with no
// spaces, the empty value listing none. A term is an address, standing for
// itself alone, or a network in CIDR form (RFC 4632 3.1), address/bits,
// whose address has no bit set past its prefix: "10.0.0.0/8", not
// "10.1.2.3/8", which would read as a mistake either way. An address is
// IPv4 in dotted form or IPv6 in the form of RFC 5952, no zone, an IPv4
// address never in its IPv4-mapped form, so each network has one spelling;
// a single address is written alone, not as /32 or /128. None is listed
// twice; at most 256.
func ParseNetworks(v string) (Networks, error) {
	if v == "" {
		return nil, nil
	}
	terms := strings.Split(v, ",")
	if len(terms) > MaxNetworks {
		return nil, errors.New("lists at most " + strconv.Itoa(MaxNetworks) + " networks")
	}
	out := make(Networks, 0, len(terms))
	seen := make(map[netip.Prefix]bool, len(terms))
	for _, t := range terms {
		p, err := parseNetwork(t)
		if err != nil {
			return nil, err
		}
		if seen[p] {
			return nil, errors.New("lists the network " + t + " twice")
		}
		seen[p] = true
		out = append(out, p)
	}
	return out, nil
}

// parseNetwork reads one term of ParseNetworks.
func parseNetwork(t string) (netip.Prefix, error) {
	bad := errors.New("is networks separated by commas, each an address or address/bits such as 10.0.0.0/8 or 2001:db8::/32, " +
		"in its one written form: IPv4 dotted, IPv6 as RFC 5952 writes it, no zone, no bit set past the prefix, a single address without /32 or /128")
	var p netip.Prefix
	if strings.Contains(t, "/") {
		q, err := netip.ParsePrefix(t)
		if err != nil {
			return netip.Prefix{}, bad
		}
		p = q
	} else {
		a, err := netip.ParseAddr(t)
		if err != nil {
			return netip.Prefix{}, bad
		}
		p = netip.PrefixFrom(a, a.BitLen())
	}
	if p.Addr().Zone() != "" || p.Addr().Is4In6() || p.Masked() != p || FormatNetwork(p) != t {
		return netip.Prefix{}, bad
	}
	return p, nil
}

// ForwardedHeader is a value of access.forwarded-header: the header a
// trusted proxy's forwarded client address is read from.
type ForwardedHeader string

// The values of access.forwarded-header.
const (
	// ForwardedXFF reads X-Forwarded-For.
	ForwardedXFF ForwardedHeader = "x-forwarded-for"
	// ForwardedRFC7239 reads Forwarded (RFC 7239).
	ForwardedRFC7239 ForwardedHeader = "forwarded"
)

// ParseForwardedHeader reads access.forwarded-header: "x-forwarded-for" or
// "forwarded", written so.
func ParseForwardedHeader(v string) (ForwardedHeader, error) {
	switch h := ForwardedHeader(v); h {
	case ForwardedXFF, ForwardedRFC7239:
		return h, nil
	}
	return "", errors.New("is x-forwarded-for or forwarded")
}

// ParseFailureLimit reads access.failure-limit: a whole number from 1 to
// MaxFailureLimit.
func ParseFailureLimit(v string) (int, error) {
	n, err := wholeNumber(v, MaxFailureLimit)
	if err != nil || n < 1 {
		return 0, errors.New("is a whole number from 1 to " + strconv.Itoa(MaxFailureLimit))
	}
	return int(n), nil
}

// ParseFailureWindow reads access.failure-window: a duration (ParseDuration)
// from 1s to 24h.
func ParseFailureWindow(v string) (time.Duration, error) {
	return durationIn(v, MinFailureWindow, MaxFailureWindow, "1s", "24h")
}

// ParseBlockTime reads access.block-time: a duration (ParseDuration) from 1s
// to 7d.
func ParseBlockTime(v string) (time.Duration, error) {
	return durationIn(v, MinBlockTime, MaxBlockTime, "1s", "7d")
}

// ParseKeyLifetime reads key.default-lifetime and key.max-lifetime: a
// duration (ParseDuration) from MinKeyLifetime (1h) to MaxKeyLifetime
// (3650d).
func ParseKeyLifetime(v string) (time.Duration, error) {
	return durationIn(v, MinKeyLifetime, MaxKeyLifetime, "1h", "3650d")
}

// ParseExpiryWarning reads key.expiry-warning: a duration (ParseDuration)
// from 1h to 365d.
func ParseExpiryWarning(v string) (time.Duration, error) {
	return durationIn(v, MinExpiryWarning, MaxExpiryWarning, "1h", "365d")
}

// durationIn reads a duration and holds it to lo and hi, both included,
// written loText and hiText in the message.
func durationIn(v string, lo, hi time.Duration, loText, hiText string) (time.Duration, error) {
	d, err := ParseDuration(v)
	if err != nil {
		return 0, err
	}
	if d < lo || d > hi {
		return 0, errors.New("is at least " + loText + " and at most " + hiText)
	}
	return d, nil
}
