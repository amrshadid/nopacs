package settings

import (
	"errors"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/amrshadid/nopacs/internal/core/ascii"
	"github.com/amrshadid/nopacs/internal/core/pathname"
)

// The bounds of the values below. Each keeps a value a person writes by hand
// far from anything that would make a setting slow to read or to check: a
// value is read on every request that depends on it.
const (
	// MaxDirectoryRoots bounds the roots of storage.directory-roots.
	MaxDirectoryRoots = 64
	// MaxRootPathLength bounds one root's path, in bytes.
	MaxRootPathLength = 4096
	// MaxS3Endpoints bounds the entries of storage.s3-endpoints.
	MaxS3Endpoints = 256
	// MaxEndpointLength bounds one endpoint's URL, the API contract's bound
	// of S3Storage.endpoint.
	MaxEndpointLength = 2048
	// MaxPortTerms bounds the ports and ranges of receiver.ports.
	MaxPortTerms = 1024
	// MaxBindAddresses bounds the addresses of receiver.bind-addresses.
	MaxBindAddresses = 64
	// MaxSize is the largest size a setting may state: 2^62 bytes, the
	// bound of a storage's capacity.
	MaxSize = 1 << 62
	// MinRetention and MaxRetention bound events.retention: a day, so a
	// client that was away overnight can still resume, and ten years.
	MinRetention = 24 * time.Hour
	MaxRetention = 3650 * 24 * time.Hour
)

// The flags an entry of storage.s3-endpoints may carry after its URL.
const (
	// FlagPlainHTTP allows an http URL, whose requests and archived files
	// cross the network unencrypted.
	FlagPlainHTTP = "plain-http"
	// FlagPrivateAddress allows the endpoint's host to resolve to a
	// loopback, link-local or private address.
	FlagPrivateAddress = "private-address"
)

// DirectoryRoot is one root of storage.directory-roots: a name a storage
// names, and the absolute path on the server it stands for.
type DirectoryRoot struct {
	// Name is under the rule of a tenant's name.
	Name string
	// Path is an absolute path on the server, as the installation wrote it.
	Path string
}

// DirectoryRoots is the value of storage.directory-roots, in the order
// written.
type DirectoryRoots []DirectoryRoot

// Lookup returns the root named name, and whether there is one.
func (r DirectoryRoots) Lookup(name string) (DirectoryRoot, bool) {
	for _, root := range r {
		if root.Name == name {
			return root, true
		}
	}
	return DirectoryRoot{}, false
}

// String returns the value as ParseDirectoryRoots reads it.
func (r DirectoryRoots) String() string {
	lines := make([]string, len(r))
	for i, root := range r {
		lines[i] = root.Name + "=" + root.Path
	}
	return strings.Join(lines, "\n")
}

// ParseDirectoryRoots reads storage.directory-roots: one root per line,
// "name=path", such as "fast=/srv/archive"; a final newline is allowed and
// the empty value lists none. A name follows the rule of a tenant's name and
// is used once. A path is absolute, in the form of Unix ("/srv/archive") or
// of Windows ("D:\archive", where "/" separates segments too), with no
// empty, "." or ".." segment, no separator at its end, and no control
// character; at most 4096 bytes. A segment of a Windows path may not end in
// "." or " ", which Windows drops when it opens a path, so "D:\srv." is
// "D:\srv" there.
//
// Two roots must not be the same directory or lie one inside the other,
// compared segment by segment with case folded: a tenant's directory is
// <path>/<tenant>, so with "a=/srv" and "b=/srv/x" the tenant named x
// would own, under a, the directory holding every tenant's directories
// under b. Case is folded because a file system that ignores case makes two
// spellings one directory. This is the rule of the text. The server applies
// two more, which need its file system (package place): each path is
// absolute on its own system, and no two roots nest once every symbolic
// link in their paths is followed, checked when the value is set and again
// whenever a root is used, since a link can change after the value is set.
func ParseDirectoryRoots(v string) (DirectoryRoots, error) {
	lines, err := splitLines(v, MaxDirectoryRoots, "roots")
	if err != nil {
		return nil, err
	}
	out := make(DirectoryRoots, 0, len(lines))
	for _, line := range lines {
		name, p, ok := strings.Cut(line, "=")
		if !ok {
			return nil, errors.New("is one root per line, written name=path, such as fast=/srv/archive")
		}
		if !pathname.ValidName(name) {
			return nil, errors.New("has a root name that does not follow the rule of a tenant's name: 1 to 63 of a-z, 0-9 and -, starting with a letter, not a device name Windows reserves")
		}
		if _, dup := out.Lookup(name); dup {
			return nil, errors.New("names the root " + name + " twice")
		}
		if err := checkRootPath(p); err != nil {
			return nil, errors.New("has a path for the root " + name + " that " + err.Error())
		}
		for _, o := range out {
			if pathsNest(o.Path, p) {
				return nil, errors.New("has the roots " + o.Name + " and " + name + " in one directory or one inside the other, which would let one tenant's directory hold another's")
			}
		}
		out = append(out, DirectoryRoot{Name: name, Path: p})
	}
	return out, nil
}

// checkRootPath applies the rule of a root's path, in either form.
func checkRootPath(p string) error {
	if p == "" || len(p) > MaxRootPathLength {
		return errors.New("is not 1 to 4096 bytes")
	}
	for i := 0; i < len(p); i++ {
		if p[i] < 0x20 || p[i] == 0x7f {
			return errors.New("holds a control character")
		}
	}
	segs, windows, ok := rootSegments(p)
	if !ok {
		return errors.New("is not absolute: it starts with / on Unix, or with a drive such as D:\\ on Windows")
	}
	for _, s := range segs {
		if s == "" || s == "." || s == ".." {
			return errors.New("has an empty, . or .. segment, or a separator at its end")
		}
		if windows && pathname.TrailingDotOrSpace(s) {
			return errors.New("has a segment that ends in . or a space, which Windows drops, so the path would stand for another")
		}
	}
	return nil
}

// rootSegments returns the segments of an absolute path below its root,
// whether the path is in the form of Windows, and false for a path that is
// not absolute: "/a/b" is a and b; "D:\a\b" is d:, a and b, its drive
// first, so two drives never nest. In a Windows path "/" separates segments
// as "\" does, since Windows reads it so: "D:\a/b" is "D:\a\b". The root
// itself, "/" or "D:\", has one empty segment and so is refused by its
// caller: a root must be a directory below it.
func rootSegments(p string) ([]string, bool, bool) {
	switch {
	case strings.HasPrefix(p, "/"):
		return strings.Split(p[1:], "/"), false, true
	case len(p) >= 3 && isASCIILetter(p[0]) && p[1] == ':' && (p[2] == '\\' || p[2] == '/'):
		segs := strings.Split(strings.ReplaceAll(p[3:], "/", "\\"), "\\")
		return append([]string{strings.ToLower(p[:2])}, segs...), true, true
	}
	return nil, false, false
}

// pathsNest reports whether two root paths, each accepted by checkRootPath,
// are one directory or one lies inside the other (pathname.Nest). It
// compares spellings only: two spellings of one name in Unicode (a
// composed and a decomposed accent, which APFS and HFS+ take for one), and
// one directory under two paths (a bind mount), are found by the server,
// which compares names in Unicode normalization form C and the
// directories' identity when the value is set and when a root is used
// (package place).
func pathsNest(a, b string) bool {
	sa, _, _ := rootSegments(a)
	sb, _, _ := rootSegments(b)
	return pathname.Nest(sa, sb)
}

// S3Endpoint is one entry of storage.s3-endpoints.
type S3Endpoint struct {
	// URL is the endpoint as the installation wrote it. The server compares
	// it with a storage's endpoint after putting both in one spelling.
	URL string
	// PlainHTTP allows the URL to be http.
	PlainHTTP bool
	// PrivateAddress allows the host to resolve to a loopback, link-local
	// or private address.
	PrivateAddress bool
}

// S3Endpoints is the value of storage.s3-endpoints, in the order written.
type S3Endpoints []S3Endpoint

// String returns the value as ParseS3Endpoints reads it.
func (e S3Endpoints) String() string {
	lines := make([]string, len(e))
	for i, ep := range e {
		lines[i] = ep.URL
		if ep.PlainHTTP {
			lines[i] += " " + FlagPlainHTTP
		}
		if ep.PrivateAddress {
			lines[i] += " " + FlagPrivateAddress
		}
	}
	return strings.Join(lines, "\n")
}

// ParseS3Endpoints reads storage.s3-endpoints: one entry per line, a final
// newline allowed, the empty value listing none. An entry is the endpoint's
// base URL, then, each after one space and each at most once, "plain-http"
// and "private-address" (FlagPlainHTTP, FlagPrivateAddress). The URL is
// http or https, of at most 2048 bytes of printable ASCII; an http URL
// carries plain-http and an https one does not, so an entry never says
// something it cannot mean. Whether the URL is one an S3 storage can use,
// and whether two entries spell one endpoint, the server decides with the
// rule it applies to a storage's endpoint.
func ParseS3Endpoints(v string) (S3Endpoints, error) {
	lines, err := splitLines(v, MaxS3Endpoints, "endpoints")
	if err != nil {
		return nil, err
	}
	out := make(S3Endpoints, 0, len(lines))
	for _, line := range lines {
		fields := strings.Split(line, " ")
		u := fields[0]
		if u == "" || len(u) > MaxEndpointLength || !ascii.Printable(u) {
			return nil, errors.New("is one endpoint per line: a URL of at most 2048 bytes of printable ASCII, then plain-http or private-address, each after one space")
		}
		ep := S3Endpoint{URL: u}
		for _, f := range fields[1:] {
			switch {
			case f == FlagPlainHTTP && !ep.PlainHTTP:
				ep.PlainHTTP = true
			case f == FlagPrivateAddress && !ep.PrivateAddress:
				ep.PrivateAddress = true
			default:
				return nil, errors.New("has an endpoint followed by something other than plain-http and private-address, each once after one space")
			}
		}
		lower := strings.ToLower(u)
		switch {
		case strings.HasPrefix(lower, "http://"):
			if !ep.PlainHTTP {
				return nil, errors.New("lists an http endpoint without plain-http: plain http is allowed only in so many words")
			}
		case strings.HasPrefix(lower, "https://"):
			if ep.PlainHTTP {
				return nil, errors.New("lists an https endpoint with plain-http, which only an http endpoint carries")
			}
		default:
			return nil, errors.New("lists an endpoint that is not an http or https URL")
		}
		out = append(out, ep)
	}
	return out, nil
}

// Reserve is the value of storage.reserve: a size, a percentage, or both,
// of which the larger applies.
type Reserve struct {
	// Bytes is the size term, when HasBytes.
	Bytes int64
	// HasBytes says there is a size term.
	HasBytes bool
	// Percent is the percentage term, 0 to 100, when HasPercent.
	Percent int
	// HasPercent says there is a percentage term.
	HasPercent bool
}

// Of returns the bytes kept free on a disk or store of total bytes: the
// larger of the two terms. A percentage of a total that is not known (0)
// is nothing.
func (r Reserve) Of(total int64) int64 {
	var out int64
	if r.HasBytes {
		out = r.Bytes
	}
	if r.HasPercent && total > 0 {
		// Divided first, so no total up to 2^62 overflows; the remainder is
		// below 100 bytes.
		if p := total / 100 * int64(r.Percent); p > out {
			out = p
		}
	}
	return out
}

// String returns the value as ParseReserve reads it, the percentage first.
func (r Reserve) String() string {
	var terms []string
	if r.HasPercent {
		terms = append(terms, strconv.Itoa(r.Percent)+"%")
	}
	if r.HasBytes {
		terms = append(terms, FormatSize(r.Bytes))
	}
	return strings.Join(terms, ",")
}

// ParseReserve reads storage.reserve: one or two terms separated by a
// comma, each a size (ParseSize) or a whole percentage from 0 to 100
// followed by "%", such as "10%,10GiB". With two, one is a size and the
// other a percentage, in either order, and the larger applies (Reserve.Of).
func ParseReserve(v string) (Reserve, error) {
	bad := errors.New("is one or two terms separated by a comma, each a size such as 10GiB or a whole percentage such as 10%; with two, one of each")
	terms := strings.Split(v, ",")
	if v == "" || len(terms) > 2 {
		return Reserve{}, bad
	}
	var r Reserve
	for _, t := range terms {
		if digits, ok := strings.CutSuffix(t, "%"); ok {
			n, err := wholeNumber(digits, 100)
			if err != nil || r.HasPercent {
				return Reserve{}, bad
			}
			r.Percent, r.HasPercent = int(n), true
			continue
		}
		n, err := ParseSize(t)
		if err != nil || r.HasBytes {
			return Reserve{}, bad
		}
		r.Bytes, r.HasBytes = n, true
	}
	return r, nil
}

// sizeUnits are the units a size takes, each a power of 1024, the IEC's
// binary prefixes: a size names the bytes a disk holds, which no one writes
// in thousands by mistake when the unit says KiB.
var sizeUnits = []struct {
	suffix string
	bytes  int64
}{
	{"PiB", 1 << 50}, {"TiB", 1 << 40}, {"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10},
}

// ParseSize reads a size: a whole number of bytes ("1048576", "0"), or a
// whole number followed by KiB, MiB, GiB, TiB or PiB ("200GiB"). It is the
// one form a size setting takes: no fractions, no signs, no spaces, no
// leading zeros, no decimal units, nothing to misread. At most 2^62 bytes.
func ParseSize(s string) (int64, error) {
	bad := errors.New("is a whole number of bytes, or a whole number followed by KiB, MiB, GiB, TiB or PiB, such as 200GiB; at most 2^62 bytes")
	digits, unit := s, int64(1)
	for _, u := range sizeUnits {
		if d, ok := strings.CutSuffix(s, u.suffix); ok {
			digits, unit = d, u.bytes
			break
		}
	}
	n, err := wholeNumber(digits, MaxSize/unit)
	if err != nil {
		return 0, bad
	}
	return n * unit, nil
}

// The bounds of receiver.header-limit.
const (
	// MinHeaderLimit is the smallest header bound: below it ordinary CT and
	// MR headers, a few KiB to a few hundred, would start to be held.
	MinHeaderLimit = 1 << 20
	// MaxHeaderLimit is the largest: a header is held in memory while it is
	// read, once per file in flight, so the bound is the memory a single
	// file may take; 4 GiB is the receivers' default bound on a whole file.
	MaxHeaderLimit = 4 << 30
)

// ParseHeaderLimit reads receiver.header-limit: a size (ParseSize) from
// MinHeaderLimit to MaxHeaderLimit.
func ParseHeaderLimit(v string) (int64, error) {
	n, err := ParseSize(v)
	if err != nil {
		return 0, err
	}
	if n < MinHeaderLimit || n > MaxHeaderLimit {
		return 0, errors.New("is a size from 1MiB to 4GiB")
	}
	return n, nil
}

// The bounds of deid.pixel-limit.
const (
	// MinPixelLimit is the smallest: below it a single ordinary
	// ultrasound frame would be held.
	MinPixelLimit = 1 << 20
	// MaxPixelLimit is the largest: past it a de-identified file, which
	// the check after reads whole, would pass any memory a server has.
	MaxPixelLimit = 64 << 30
)

// ParsePixelLimit reads deid.pixel-limit: a size (ParseSize) from
// MinPixelLimit to MaxPixelLimit.
func ParsePixelLimit(v string) (int64, error) {
	n, err := ParseSize(v)
	if err != nil {
		return 0, err
	}
	if n < MinPixelLimit || n > MaxPixelLimit {
		return 0, errors.New("is a size from 1MiB to 64GiB")
	}
	return n, nil
}

// The bounds of deid.memory.
const (
	// MinDeidMemory is the smallest: below it the check after of an
	// ordinary multi-frame file would be held.
	MinDeidMemory = 256 << 20
	// MaxDeidMemory is the largest, as deid.pixel-limit's.
	MaxDeidMemory = 64 << 30
)

// The bounds of deid.list-cache-bytes.
const (
	// MinListCacheBytes is the smallest: one version's lists at their
	// largest fit in it.
	MinListCacheBytes = 64 << 20
	// MaxListCacheBytes is the largest.
	MaxListCacheBytes = 16 << 30
)

// ParseListCacheBytes reads deid.list-cache-bytes: a size (ParseSize)
// from MinListCacheBytes to MaxListCacheBytes.
func ParseListCacheBytes(v string) (int64, error) {
	n, err := ParseSize(v)
	if err != nil {
		return 0, err
	}
	if n < MinListCacheBytes || n > MaxListCacheBytes {
		return 0, errors.New("is a size from 64MiB to 16GiB")
	}
	return n, nil
}

// ParseDeidMemory reads deid.memory: a size (ParseSize) from MinDeidMemory
// to MaxDeidMemory.
func ParseDeidMemory(v string) (int64, error) {
	n, err := ParseSize(v)
	if err != nil {
		return 0, err
	}
	if n < MinDeidMemory || n > MaxDeidMemory {
		return 0, errors.New("is a size from 256MiB to 64GiB")
	}
	return n, nil
}

// FormatSize writes a size in the largest unit that divides it, as
// ParseSize reads it.
func FormatSize(n int64) string {
	for _, u := range sizeUnits {
		if n != 0 && n%u.bytes == 0 {
			return strconv.FormatInt(n/u.bytes, 10) + u.suffix
		}
	}
	return strconv.FormatInt(n, 10)
}

// PortRange is ports Lo to Hi, both included.
type PortRange struct {
	Lo, Hi int
}

// Ports is the value of receiver.ports: ranges in increasing order that do
// not overlap.
type Ports []PortRange

// Contains reports whether port is one of the ports.
func (p Ports) Contains(port int) bool {
	for _, r := range p {
		if port >= r.Lo && port <= r.Hi {
			return true
		}
	}
	return false
}

// Overlap returns a port both p and o hold, and whether there is one.
func (p Ports) Overlap(o Ports) (int, bool) {
	for _, a := range p {
		for _, b := range o {
			if lo, hi := max(a.Lo, b.Lo), min(a.Hi, b.Hi); lo <= hi {
				return lo, true
			}
		}
	}
	return 0, false
}

// String returns the value as ParsePorts reads it.
func (p Ports) String() string {
	terms := make([]string, len(p))
	for i, r := range p {
		terms[i] = strconv.Itoa(r.Lo)
		if r.Hi != r.Lo {
			terms[i] += "-" + strconv.Itoa(r.Hi)
		}
	}
	return strings.Join(terms, ",")
}

// ParsePorts reads receiver.ports: ports and ranges separated by commas,
// such as "11112,11120-11129", with no spaces; the empty value allots none.
// A port is 1 to 65535 with no leading zero, and a range is lo-hi with lo
// not above hi. No port is listed twice, so a value has one meaning however
// its terms are ordered; the ranges come back sorted.
func ParsePorts(v string) (Ports, error) {
	if v == "" {
		return nil, nil
	}
	terms := strings.Split(v, ",")
	if len(terms) > MaxPortTerms {
		return nil, errors.New("lists at most 1024 ports and ranges")
	}
	out := make(Ports, 0, len(terms))
	for _, t := range terms {
		lo, hi, isRange := strings.Cut(t, "-")
		a, err := port(lo)
		if err != nil {
			return nil, err
		}
		b := a
		if isRange {
			if b, err = port(hi); err != nil {
				return nil, err
			}
			if b < a {
				return nil, errors.New("has a range whose first port is above its last")
			}
		}
		out = append(out, PortRange{Lo: a, Hi: b})
	}
	slices.SortFunc(out, func(x, y PortRange) int { return x.Lo - y.Lo })
	for i := 1; i < len(out); i++ {
		if out[i].Lo <= out[i-1].Hi {
			return nil, errors.New("lists the port " + strconv.Itoa(out[i].Lo) + " twice")
		}
	}
	return out, nil
}

// port reads one port of receiver.ports.
func port(s string) (int, error) {
	n, err := wholeNumber(s, 65535)
	if err != nil || n == 0 {
		return 0, errors.New("is ports and ranges separated by commas, such as 11112,11120-11129: each port 1 to 65535")
	}
	return int(n), nil
}

// BindAddresses is the value of receiver.bind-addresses, in the order
// written; the first is the default of a receiver that names none.
type BindAddresses []netip.Addr

// Contains reports whether a is one of the addresses.
func (b BindAddresses) Contains(a netip.Addr) bool { return slices.Contains(b, a) }

// String returns the value as ParseBindAddresses reads it.
func (b BindAddresses) String() string {
	terms := make([]string, len(b))
	for i, a := range b {
		terms[i] = a.String()
	}
	return strings.Join(terms, ",")
}

// ParseBindAddresses reads receiver.bind-addresses: IP addresses separated
// by commas, with no spaces; the empty value lists none, and every receiver
// then listens on every address. Each address is written as a receiver's
// bind address must be (receiver.proto, DimseReceiver.bind_address): IPv4 in
// dotted form, IPv6 in the form of RFC 5952, no zone, and an IPv4 address
// never in its IPv4-mapped IPv6 form, so each address has one spelling and
// a receiver's address matches the list by its text. None is listed twice.
func ParseBindAddresses(v string) (BindAddresses, error) {
	if v == "" {
		return nil, nil
	}
	terms := strings.Split(v, ",")
	if len(terms) > MaxBindAddresses {
		return nil, errors.New("lists at most 64 addresses")
	}
	out := make(BindAddresses, 0, len(terms))
	for _, t := range terms {
		a, err := netip.ParseAddr(t)
		if err != nil || a.Zone() != "" || a.Is4In6() || a.String() != t {
			return nil, errors.New("is IP addresses separated by commas, each an IPv4 address in dotted form or an IPv6 address in the form of RFC 5952, without a zone")
		}
		if out.Contains(a) {
			return nil, errors.New("lists the address " + t + " twice")
		}
		out = append(out, a)
	}
	return out, nil
}

// ParseRetention reads events.retention: a duration as ParseDuration reads
// it, from MinRetention (1d) to MaxRetention (3650d).
func ParseRetention(v string) (time.Duration, error) {
	d, err := ParseDuration(v)
	if err != nil {
		return 0, err
	}
	if d < MinRetention || d > MaxRetention {
		return 0, errors.New("is at least 1d and at most 3650d")
	}
	return d, nil
}

// splitLines returns the lines of a value of one item per line, a final
// newline allowed, none of them empty; the empty value has none.
func splitLines(v string, limit int, what string) ([]string, error) {
	v = strings.TrimSuffix(v, "\n")
	if v == "" {
		return nil, nil
	}
	lines := strings.Split(v, "\n")
	if len(lines) > limit {
		return nil, errors.New("lists at most " + strconv.Itoa(limit) + " " + what)
	}
	for _, l := range lines {
		if l == "" {
			return nil, errors.New("has an empty line")
		}
	}
	return lines, nil
}

// wholeNumber reads a decimal number of no more than limit, with no sign,
// no leading zero and nothing but digits.
func wholeNumber(s string, limit int64) (int64, error) {
	bad := errors.New("not a whole number in range")
	if s == "" || len(s) > 19 || (len(s) > 1 && s[0] == '0') || strings.Trim(s, "0123456789") != "" {
		return 0, bad
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n > limit {
		return 0, bad
	}
	return n, nil
}

// isASCIILetter reports whether b is an ASCII letter.
func isASCIILetter(b byte) bool { return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') }
