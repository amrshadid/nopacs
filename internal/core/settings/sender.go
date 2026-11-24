package settings

import (
	"errors"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	"github.com/amrshadid/nopacs/internal/core/publicaddr"
)

// The settings of where a sender may send and how much it may hold open,
// each named where it is used in the API contract (sender.proto and
// job.proto).
const (
	// SenderDestinations is the hosts and address ranges a tenant's senders
	// may reach, one per line (ParseDestinations). Installation-only: a
	// sender moves patient data out of the archive, and a tenant's admin
	// must not be able to point the server into the network it sits in.
	SenderDestinations = "sender.destinations"
	// SenderAllowPlainHTTP is "true" to let a stow sender's URL be http
	// (ParseBool). Installation-only.
	SenderAllowPlainHTTP = "sender.allow-plain-http"
	// SenderMaxAssociations is the most associations (or requests) all
	// senders of all tenants hold open at once (ParseAssociations).
	// Installation-only: it bounds the server's own connections.
	SenderMaxAssociations = "sender.max-associations"
	// SenderTenantMaxAssociations is the most associations (or requests)
	// the senders of one tenant hold open at once (ParseAssociations). Set
	// on a tenant by the installation's admin only; the installation's
	// value is every tenant's default.
	SenderTenantMaxAssociations = "sender.tenant-max-associations"
	// JobFailuresKept is how many refused instances, and instances sent with
	// a warning, a job lists (ParseFailuresKept). Ordinary: a tenant's admin
	// may override the installation's value for the tenant.
	JobFailuresKept = "job.failures-kept"
)

// The bounds and defaults of the values below.
const (
	// MaxDestinations bounds the lines of sender.destinations.
	MaxDestinations = 256
	// MinAssociations and MaxAssociations bound sender.max-associations and
	// sender.tenant-max-associations.
	MinAssociations = 1
	MaxAssociations = 1024
	// MinFailuresKept and MaxFailuresKept bound job.failures-kept.
	MinFailuresKept = 100
	MaxFailuresKept = 1000000
)

// The flags a line of sender.destinations may carry, and the prefix of its
// ports term.
const (
	// DestinationPrivateAddress allows a listed host name to resolve to a
	// loopback or private address (RFC 1918, RFC 4193, RFC 6598).
	DestinationPrivateAddress = "private-address"
	// DestinationLinkLocal allows a listed host name to resolve to a
	// link-local address (RFC 3927, RFC 4291 2.5.6). It is a flag of its
	// own because the link-local block is where cloud hosts answer their
	// instance metadata, which a name a tenant's admin points a sender at
	// must not reach by a DNS change; the metadata addresses themselves are
	// never a destination (neverDestination).
	DestinationLinkLocal   = "link-local"
	destinationPortsPrefix = "ports="
)

// The reasons Destinations.Allowed refuses a destination. Each is a case
// the caller can tell apart: a sender is not added at a destination that is
// not listed, and a connection that resolved to an address the entry does
// not allow is reported as such.
var (
	// ErrDestinationNotListed: neither the host's name nor, for an address,
	// any listed range lists it.
	ErrDestinationNotListed = errors.New("the host is not listed in sender.destinations")
	// ErrDestinationPort: the host is listed, with ports that leave this
	// one out.
	ErrDestinationPort = errors.New("the port is not among the ports sender.destinations lists for the host")
	// ErrDestinationAddress: the address is never a destination (the
	// unspecified, multicast and broadcast addresses and the cloud metadata
	// addresses, whatever the line says), or the host's name is listed and resolved to a special-purpose
	// address its line does not admit (see Destination.PrivateAddress).
	ErrDestinationAddress = errors.New("the host resolves to an address its entry in sender.destinations does not allow")
)

// Destination is one line of sender.destinations: a host name, or a range
// of addresses, with the ports it allows.
type Destination struct {
	// Host is the host name in lowercase, empty for a range.
	Host string
	// Range is the range of addresses, invalid for a host name. A single
	// address is the range of that one address.
	Range netip.Prefix
	// Ports are the ports allowed; none means every port.
	Ports Ports
	// PrivateAddress allows a host name to resolve to exactly these
	// addresses besides global unicast ones: loopback (127.0.0.0/8, ::1),
	// private use (RFC 1918, and fc00::/7 of RFC 4193) and shared address
	// space (100.64.0.0/10, RFC 6598). Any other special-purpose address
	// stays refused. A range never carries it: it lists its addresses
	// explicitly, those included.
	PrivateAddress bool
	// LinkLocal allows a host name to resolve to a link-local address
	// (169.254.0.0/16, fe80::/10), but for the cloud metadata addresses,
	// which are never a destination. A range never carries it.
	LinkLocal bool
}

// Destinations is the value of sender.destinations, in the order written.
// The zero value lists none, so no sender may be added until the
// installation lists one.
type Destinations []Destination

// String returns the value as ParseDestinations reads it.
func (d Destinations) String() string {
	lines := make([]string, len(d))
	for i, e := range d {
		if e.Range.IsValid() {
			lines[i] = e.Range.String()
		} else {
			lines[i] = e.Host
		}
		if len(e.Ports) > 0 {
			lines[i] += " " + destinationPortsPrefix + e.Ports.String()
		}
		if e.PrivateAddress {
			lines[i] += " " + DestinationPrivateAddress
		}
		if e.LinkLocal {
			lines[i] += " " + DestinationLinkLocal
		}
	}
	return strings.Join(lines, "\n")
}

// Allowed returns nil when a connection to host, at port, reaching ip, is
// one the installation lists, and otherwise one of ErrDestinationNotListed,
// ErrDestinationPort and ErrDestinationAddress.
//
// host is the name or address a sender is configured with, compared in
// lowercase. A host that is a name is allowed by a line that lists that
// name; one that is an address (an IPv4 address in dotted form or an IPv6
// address, with no brackets or zone), by a line whose range holds it. A
// name is never allowed by a range, though it resolves into one: whoever
// lists a range lists the addresses a sender is configured with, and a
// name's address may change.
//
// ip is the address host resolved to, which the caller checks again at
// each connection, so a name that later resolves elsewhere cannot carry a
// connection into the server's own network. A host name's line without
// private-address allows only a global unicast address (package
// publicaddr); with private-address, also the addresses
// Destination.PrivateAddress lists; with link-local, the link-local
// addresses. Whatever the line says, the unspecified (0.0.0.0, ::),
// multicast, IPv4 broadcast and cloud metadata addresses (169.254.169.254,
// fd00:ec2::254) are never a destination. The zero ip means the host is not resolved yet, as when a
// sender is added or changed, and only the name, or the address host is,
// and the port are checked; an address given as host is its own ip. An
// IPv4-mapped IPv6 address is judged as the IPv4 address it holds.
//
// port is 1 to 65535; anything else is not listed.
func (d Destinations) Allowed(host string, ip netip.Addr, port int) error {
	host = strings.ToLower(host)
	literal, isAddress := parseLiteral(host)
	if isAddress {
		ip = literal
	}
	ip = ip.Unmap().WithZone("")
	listed, portOK, addrOK := false, false, false
	for _, e := range d {
		switch {
		case isAddress && e.Range.IsValid() && e.Range.Contains(ip):
		case !isAddress && e.Host != "" && e.Host == host:
		default:
			continue
		}
		listed = true
		if len(e.Ports) > 0 && !e.Ports.Contains(port) {
			continue
		}
		portOK = true
		if neverDestination(ip) {
			continue
		}
		if e.Host != "" && ip.IsValid() && !e.admits(ip) {
			continue
		}
		addrOK = true
	}
	switch {
	case port < 1 || port > 65535 || !listed:
		return ErrDestinationNotListed
	case !portOK:
		return ErrDestinationPort
	case !addrOK:
		return ErrDestinationAddress
	}
	return nil
}

// admits reports whether a host name's line admits ip as the address the
// name resolved to: a global unicast address, or one its flags allow.
func (e Destination) admits(ip netip.Addr) bool {
	return publicaddr.Is(ip) || (e.PrivateAddress && privateUse(ip)) || (e.LinkLocal && linkLocal(ip))
}

// privateRanges are the addresses a line's private-address admits besides
// global unicast ones.
var privateRanges = []netip.Prefix{
	netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("::1/128"),
	netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("172.16.0.0/12"), netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("fc00::/7"), netip.MustParsePrefix("100.64.0.0/10"),
}

// privateUse reports whether a is one of privateRanges.
func privateUse(a netip.Addr) bool {
	return slices.ContainsFunc(privateRanges, func(p netip.Prefix) bool { return p.Contains(a) })
}

// linkLocalRanges are the addresses a line's link-local admits.
var linkLocalRanges = []netip.Prefix{netip.MustParsePrefix("169.254.0.0/16"), netip.MustParsePrefix("fe80::/10")}

// linkLocal reports whether a is one of linkLocalRanges.
func linkLocal(a netip.Addr) bool {
	return slices.ContainsFunc(linkLocalRanges, func(p netip.Prefix) bool { return p.Contains(a) })
}

// metadataAddresses are where cloud hosts answer an instance's metadata,
// credentials included: 169.254.169.254 (most clouds) and fd00:ec2::254
// (the IPv6 one of AWS). A sender reaching one would hand a tenant's admin
// the host's credentials, so no line allows them.
var metadataAddresses = []netip.Addr{netip.MustParseAddr("169.254.169.254"), netip.MustParseAddr("fd00:ec2::254")}

// neverDestination reports whether a is an address no sender may be
// pointed at, whatever a line says: the unspecified addresses (0.0.0.0,
// ::), which a connection reaches the local host by, the multicast blocks,
// the IPv4 broadcast address and the cloud metadata addresses. The zero
// address is not one.
func neverDestination(a netip.Addr) bool {
	a = a.Unmap().WithZone("")
	return a.IsValid() && (a.IsUnspecified() || a.IsMulticast() || a == netip.AddrFrom4([4]byte{255, 255, 255, 255}) ||
		slices.Contains(metadataAddresses, a))
}

// parseLiteral reads host as an address when it is one, in the one spelling
// a sender's host has: IPv4 in dotted form, IPv6 in the form of RFC 5952,
// no zone.
func parseLiteral(host string) (netip.Addr, bool) {
	a, err := netip.ParseAddr(host)
	if err != nil || a.Zone() != "" {
		return netip.Addr{}, false
	}
	return a, true
}

// ParseDestinations reads sender.destinations: one line per destination, a
// final newline allowed, the empty value listing none. A line is a host
// name (RFC 1123: labels of letters, digits and hyphens, at most 253 bytes,
// compared in lowercase) or a range of addresses (CIDR, RFC 4632 and RFC
// 4291, with no host bits set, or one address alone), then, each after one
// space and each at most once, "ports=" and the ports allowed, as
// ParsePorts reads them ("ports=104,11112-11120"; every port when left
// out), "private-address" (DestinationPrivateAddress) and "link-local"
// (DestinationLinkLocal), which only a host name's line carries, since a
// range lists its addresses explicitly.
// A host name, or a range, is listed once.
func ParseDestinations(v string) (Destinations, error) {
	lines, err := splitLines(v, MaxDestinations, "destinations")
	if err != nil {
		return nil, err
	}
	out := make(Destinations, 0, len(lines))
	for _, line := range lines {
		e, err := parseDestination(line)
		if err != nil {
			return nil, errors.New("lists " + strconv.Quote(clipDestination(line)) + ", which " + err.Error())
		}
		if slices.ContainsFunc(out, func(o Destination) bool { return o.Host == e.Host && o.Range == e.Range }) {
			return nil, errors.New("lists " + strconv.Quote(clipDestination(line)) + " twice")
		}
		out = append(out, e)
	}
	return out, nil
}

// clipDestination cuts a refused line for its error message, which may be
// shown to the person who wrote it.
func clipDestination(s string) string {
	if len(s) > 64 {
		return s[:64] + "..."
	}
	return s
}

// parseDestination reads one line of sender.destinations.
func parseDestination(line string) (Destination, error) {
	bad := errors.New("is not a host name or a range of addresses, then ports=, private-address and link-local, each once after one space")
	fields := strings.Split(line, " ")
	var e Destination
	target := fields[0]
	switch {
	case strings.Contains(target, "/"):
		p, err := netip.ParsePrefix(target)
		if err != nil || p.Addr().Zone() != "" || p.Addr().Is4In6() || p != p.Masked() || p.String() != target {
			return e, errors.New("has a range that is not an IPv4 or IPv6 address range in CIDR form with no host bits set, in the form of RFC 5952 for IPv6")
		}
		if p.Bits() == 0 {
			return e, errors.New("is the whole address space, which would allow every host: list the ranges that are meant")
		}
		if neverDestination(p.Addr()) {
			return e, errors.New("starts at an address that is never a destination (unspecified, multicast, broadcast or a cloud metadata address)")
		}
		e.Range = p
	default:
		if a, ok := parseLiteral(target); ok {
			if a.Is4In6() || a.String() != target {
				return e, errors.New("has an address that is not in its shortest form, IPv4 in dotted form, and not IPv4-mapped")
			}
			if neverDestination(a) {
				return e, errors.New("has an address that is never a destination (unspecified, multicast, broadcast or a cloud metadata address)")
			}
			e.Range = netip.PrefixFrom(a, a.BitLen())
			break
		}
		name := strings.ToLower(target)
		if err := checkDestinationName(name); err != nil {
			return e, err
		}
		e.Host = name
	}
	for _, f := range fields[1:] {
		switch {
		case f == DestinationPrivateAddress && !e.PrivateAddress:
			if e.Range.IsValid() {
				return e, errors.New("gives private-address to a range, which lists its addresses explicitly and needs no such flag")
			}
			e.PrivateAddress = true
		case f == DestinationLinkLocal && !e.LinkLocal:
			if e.Range.IsValid() {
				return e, errors.New("gives link-local to a range, which lists its addresses explicitly and needs no such flag")
			}
			e.LinkLocal = true
		case strings.HasPrefix(f, destinationPortsPrefix) && e.Ports == nil:
			ports, err := ParsePorts(strings.TrimPrefix(f, destinationPortsPrefix))
			if err != nil || len(ports) == 0 {
				return e, errors.New("has ports= that is not ports and ranges separated by commas, such as ports=104,11112-11120: each 1 to 65535, none twice")
			}
			e.Ports = ports
		default:
			return e, bad
		}
	}
	return e, nil
}

// checkDestinationName returns why s, in lowercase, is not a host name of
// RFC 1123: labels of 1 to 63 letters, digits and hyphens, not starting or
// ending with a hyphen, at most 253 bytes, and a last label that is not
// all digits, which would make it a malformed IPv4 address.
func checkDestinationName(s string) error {
	bad := errors.New("has a host that is not a DNS name of letters, digits, hyphens and dots, at most 253 bytes, or an address")
	if s == "" || len(s) > 253 {
		return bad
	}
	for label := range strings.SplitSeq(s, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return bad
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
				return bad
			}
		}
	}
	// A resolver reads a name whose last label is a number as an address
	// (inet_aton: "127.1", "0177.0.0.1", "0x7f000001", "2130706433"), so
	// such a name could reach what no line lists.
	last := s[strings.LastIndexByte(s, '.')+1:]
	if strings.Trim(last, "0123456789") == "" {
		return bad
	}
	if h, ok := strings.CutPrefix(last, "0x"); ok && strings.Trim(h, "0123456789abcdef") == "" {
		return bad
	}
	return nil
}

// ParseBool reads a setting that is "true" or "false" and nothing else, so
// a value reads the same everywhere it is shown.
func ParseBool(v string) (bool, error) {
	switch v {
	case "true":
		return true, nil
	case "false":
		return false, nil
	}
	return false, errors.New(`is "true" or "false"`)
}

// ParseAssociations reads sender.max-associations and
// sender.tenant-max-associations: a whole number from MinAssociations to
// MaxAssociations.
func ParseAssociations(v string) (int, error) {
	n, err := wholeNumber(v, MaxAssociations)
	if err != nil || n < MinAssociations {
		return 0, errors.New("is a whole number from 1 to 1024")
	}
	return int(n), nil
}

// ParseFailuresKept reads job.failures-kept: a whole number from
// MinFailuresKept to MaxFailuresKept.
func ParseFailuresKept(v string) (int, error) {
	n, err := wholeNumber(v, MaxFailuresKept)
	if err != nil || n < MinFailuresKept {
		return 0, errors.New("is a whole number from 100 to 1000000")
	}
	return int(n), nil
}

// senderDefinitions are the definitions of the sender and job settings.
func senderDefinitions() []Definition {
	return []Definition{{
		// Empty by default, so no sender can be added until the
		// installation's admin lists a destination.
		Name:     SenderDestinations,
		Default:  "",
		Validate: func(v string) error { _, err := ParseDestinations(v); return err },
	}, {
		// Off by default: over http every instance and the sender's
		// credential cross the network unencrypted.
		Name:     SenderAllowPlainHTTP,
		Default:  "false",
		Validate: func(v string) error { _, err := ParseBool(v); return err },
	}, {
		Name:     SenderMaxAssociations,
		Default:  "32",
		Validate: func(v string) error { _, err := ParseAssociations(v); return err },
	}, {
		// The installation's value is every tenant's default; a tenant's
		// own is set by the installation's admin, since the associations
		// are the server's own connections, which tenants share. A
		// separate name from sender.max-associations, since one name
		// cannot mean the whole server's bound and a tenant's share.
		Name:                    SenderTenantMaxAssociations,
		Default:                 "8",
		TenantOverride:          true,
		TenantSetByInstallation: true,
		Validate:                func(v string) error { _, err := ParseAssociations(v); return err },
	}, {
		Name:           JobFailuresKept,
		Default:        "10000",
		TenantOverride: true,
		Validate:       func(v string) error { _, err := ParseFailuresKept(v); return err },
	}}
}
