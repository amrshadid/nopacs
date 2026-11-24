package settings

import (
	"errors"
	"net/netip"
	"slices"
	"strconv"
	"strings"
)

// APICORSOrigins is the web origins whose pages may call the API from a
// browser, one per line, or "*" alone for any origin (ParseOrigins).
// Installation-only: the API's port is the installation's, and a tenant
// cannot open it to a page its admin trusts on behalf of every other
// tenant.
const APICORSOrigins = "api.cors-origins"

// MaxOrigins bounds the origins api.cors-origins lists: far more than the
// handful of web applications an installation serves, and few enough that a
// request's origin is compared with every one at no measurable cost.
const MaxOrigins = 64

// maxOriginLength bounds one origin: a scheme, "://", a host name of at
// most 253 bytes (RFC 1035 2.3.4) or an IPv6 address, and a port.
const maxOriginLength = 300

// AnyOrigin is the one value of api.cors-origins that lets a page from any
// origin call the API.
const AnyOrigin = "*"

// Origins is the value of api.cors-origins: the origins a browser may call
// the API from. The zero value allows none, so the API is closed to pages
// until an operator lists one.
type Origins struct {
	// Any is set when the value is "*": every origin is allowed.
	Any bool
	// List is the origins allowed, each as a browser serializes it in the
	// Origin header (the Fetch standard, "origin"; RFC 6454 6.1).
	List []string
}

// Allows reports whether a request whose Origin header is origin may be
// answered to its page: listed byte for byte, since a browser always
// writes an origin in the one form ParseOrigins accepts, or allowed by
// "*". An empty origin is never allowed.
func (o Origins) Allows(origin string) bool {
	if origin == "" {
		return false
	}
	return o.Any || slices.Contains(o.List, origin)
}

// String returns the value as it is written: "*", or one origin per line.
func (o Origins) String() string {
	if o.Any {
		return AnyOrigin
	}
	return strings.Join(o.List, "\n")
}

// ParseOrigins reads api.cors-origins: empty (no origin), "*" alone (any
// origin), or one origin per line, each "<scheme>://<host>" or
// "<scheme>://<host>:<port>" exactly as a browser sends it in the Origin
// header (the Fetch standard, "serialization of an origin"): the scheme
// http or https; the host in lower case, a name of a-z, 0-9, "-" and "."
// (an internationalized name in its "xn--" form) or an IPv6 address in
// brackets in its shortest form; the port only when it is not the scheme's
// default, which a browser leaves out. No path, no trailing "/", no
// "null" (the origin of a sandboxed page or a file, which any page can
// take), and no origin twice. A value in any other form is refused rather
// than read loosely, since an origin written in a form no browser sends
// would never match, and the operator would not know why.
func ParseOrigins(v string) (Origins, error) {
	if v == AnyOrigin || v == AnyOrigin+"\n" {
		return Origins{Any: true}, nil
	}
	lines, err := splitLines(v, MaxOrigins, "origins")
	if err != nil {
		return Origins{}, err
	}
	out := Origins{List: make([]string, 0, len(lines))}
	for _, line := range lines {
		if line == AnyOrigin {
			return Origins{}, errors.New("lists * with other origins: * stands alone, for any origin")
		}
		if err := checkOrigin(line); err != nil {
			return Origins{}, errors.New("lists " + strconv.Quote(clipOrigin(line)) + ", which " + err.Error())
		}
		if slices.Contains(out.List, line) {
			return Origins{}, errors.New("lists " + strconv.Quote(line) + " twice")
		}
		out.List = append(out.List, line)
	}
	return out, nil
}

// clipOrigin cuts a refused line for its error message, which may be shown
// to the person who wrote it, so a long line does not fill the answer.
func clipOrigin(s string) string {
	if len(s) > 64 {
		return s[:64] + "..."
	}
	return s
}

// checkOrigin returns why s is not an origin as a browser serializes it.
func checkOrigin(s string) error {
	if len(s) > maxOriginLength {
		return errors.New("is longer than " + strconv.Itoa(maxOriginLength) + " bytes")
	}
	scheme, rest, ok := strings.Cut(s, "://")
	if !ok || (scheme != "http" && scheme != "https") {
		return errors.New("is not an http or https origin, such as https://viewer.example.org")
	}
	host, port := rest, ""
	if strings.HasPrefix(rest, "[") {
		end := strings.IndexByte(rest, ']')
		if end < 0 {
			return errors.New("has an IPv6 address without its closing bracket")
		}
		host, rest = rest[:end+1], rest[end+1:]
		if rest != "" {
			p, ok := strings.CutPrefix(rest, ":")
			if !ok {
				return errors.New("has something other than a port after its host")
			}
			port = p
		}
		a, err := netip.ParseAddr(host[1 : len(host)-1])
		if err != nil || !a.Is6() || a.Zone() != "" || a.Is4In6() || "["+a.String()+"]" != host {
			return errors.New("has an IPv6 address not in its shortest lower-case form")
		}
	} else {
		if h, p, found := strings.Cut(rest, ":"); found {
			host, port = h, p
		}
		if err := checkHostName(host); err != nil {
			return err
		}
	}
	if port == "" {
		return nil
	}
	n, err := wholeNumber(port, 65535)
	switch {
	case err != nil || n == 0:
		return errors.New("has a port that is not 1 to 65535 written plainly")
	case (scheme == "http" && n == 80) || (scheme == "https" && n == 443):
		return errors.New("names its scheme's default port, which a browser leaves out of an origin")
	}
	return nil
}

// checkHostName returns why h is not a host name or IPv4 address as a
// browser serializes it: labels of a-z, 0-9 and "-", joined by ".", none
// empty, at most 253 bytes in all.
func checkHostName(h string) error {
	if h == "" || len(h) > 253 {
		return errors.New("has no host, or one longer than 253 bytes")
	}
	// A host whose last label is all digits is an IPv4 address, which a
	// browser sends only in its canonical form (no leading zeros, four
	// parts).
	last := h[strings.LastIndexByte(h, '.')+1:]
	if last != "" && strings.Trim(last, "0123456789") == "" {
		if a, err := netip.ParseAddr(h); err != nil || !a.Is4() || a.String() != h {
			return errors.New("has a host that ends in digits but is not an IPv4 address written as a browser writes it")
		}
	}
	for label := range strings.SplitSeq(h, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return errors.New("has a host that is not a name of a-z, 0-9, - and . or an address, with nothing after it but a port")
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
				return errors.New("has a host that is not a name of a-z, 0-9, - and . or an address, with nothing after it but a port")
			}
		}
	}
	return nil
}
