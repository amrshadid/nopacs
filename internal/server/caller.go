package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/netip"
	"strings"

	"github.com/amrshadid/nopacs/internal/core/settings"
)

// origin is where a request came from, as the front step judged it once:
// the address its audit entry, its log line and a key's last use record,
// the address the network rules are judged against, whether it came on the
// local socket, and the client certificate its connection presented.
type origin struct {
	// remote is the caller's address as recorded: the connection's
	// "ip:port", or the bare address a trusted proxy forwarded; empty on
	// the local socket.
	remote string
	// addr is remote's address, the zero Addr on the local socket.
	addr netip.Addr
	// local is set for a request on the local socket.
	local bool
	// clientCert is the SHA-256 of the TLS client certificate, lowercase
	// hexadecimal, or empty.
	clientCert string
}

// originKey is the context key of a request's origin.
type originKey struct{}

// originFrom returns the origin the front step put in ctx.
func originFrom(ctx context.Context) (origin, bool) {
	o, ok := ctx.Value(originKey{}).(origin)
	return o, ok
}

// maxForwardedHops bounds how many entries of a forwarded header are read,
// from the right: far more proxies than any chain in front of a server has,
// so a header of thousands of entries costs no more than this.
const maxForwardedHops = 32

// originOf judges where r came from. On the local socket it is local, with
// no address. Otherwise the connection's peer is the caller, unless the peer
// is one of trusted (the installation's access.trusted-proxies): then the
// caller is the rightmost address of the forwarded header named by header
// (access.forwarded-header) that is not a trusted proxy itself
// (callerBehind), so a client cannot pass for another by writing the header
// itself, and the header from anyone else is ignored.
func originOf(ctx context.Context, r *http.Request, trusted settings.Networks, header settings.ForwardedHeader) origin {
	if _, ok := localFrom(ctx); ok {
		return origin{local: true}
	}
	o := origin{remote: r.RemoteAddr, addr: addrOf(r.RemoteAddr)}
	if r.TLS != nil && len(r.TLS.PeerCertificates) > 0 {
		sum := sha256.Sum256(r.TLS.PeerCertificates[0].Raw)
		o.clientCert = hex.EncodeToString(sum[:])
	}
	if !o.addr.IsValid() || !trusted.Contains(o.addr) {
		return o
	}
	if a, ok := callerBehind(r.Header, trusted, header); ok {
		o.remote, o.addr = a.String(), a
	}
	return o
}

// callerBehind reads the forwarded client address of a request whose peer
// is a trusted proxy from the one header the installation names: the
// Forwarded header's "for" parameters (RFC 7239 5.2) when header is
// "forwarded", else X-Forwarded-For, each read as one list in the order of
// its fields (RFC 9110 5.3). The other header is never read: a proxy that
// appends to one commonly passes a client's copy of the other through, and
// reading it, even only when present, would let the client name its own
// address. Each proxy appends the address it received from, so the list is
// read from the right: a trusted proxy's entry is passed over, and the
// first entry that is not a trusted proxy is the caller. An entry that is
// not an address ("unknown", an obfuscated identifier, garbage) ends the
// walk, and the caller is the last trusted proxy read, the furthest hop
// that can be vouched for; so is the leftmost entry when every one is
// trusted. It reports false when the header holds no address at all, and
// the peer is then the caller.
func callerBehind(h http.Header, trusted settings.Networks, header settings.ForwardedHeader) (netip.Addr, bool) {
	var entries []string
	if header == settings.ForwardedRFC7239 {
		for _, element := range splitList(h.Values("Forwarded")) {
			entries = append(entries, forwardedFor(element))
		}
	} else {
		entries = splitList(h.Values("X-Forwarded-For"))
	}
	var last netip.Addr
	for i, n := len(entries)-1, 0; i >= 0 && n < maxForwardedHops; i, n = i-1, n+1 {
		a := nodeAddr(entries[i])
		if !a.IsValid() {
			break
		}
		if !trusted.Contains(a) {
			return a, true
		}
		last = a
	}
	return last, last.IsValid()
}

// splitList splits header field values into the elements of a
// comma-separated list, trimmed, empty elements dropped (RFC 9110 5.6.1). A
// comma inside a quoted string does not split.
func splitList(values []string) []string {
	var out []string
	add := func(e string) {
		if e = strings.TrimSpace(e); e != "" {
			out = append(out, e)
		}
	}
	for _, v := range values {
		start, quoted := 0, false
		for i := 0; i < len(v); i++ {
			switch c := v[i]; {
			case c == '\\' && quoted:
				i++
			case c == '"':
				quoted = !quoted
			case c == ',' && !quoted:
				add(v[start:i])
				start = i + 1
			}
		}
		add(v[start:])
	}
	return out
}

// forwardedFor returns the value of the "for" parameter of one element of a
// Forwarded header (RFC 7239 4: parameters separated by ";", names compared
// without regard to case), unquoted; empty when it has none.
func forwardedFor(element string) string {
	for pair := range strings.SplitSeq(element, ";") {
		name, value, ok := strings.Cut(strings.TrimSpace(pair), "=")
		if ok && strings.EqualFold(strings.TrimSpace(name), "for") {
			value = strings.TrimSpace(value)
			if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
				value = strings.ReplaceAll(value[1:len(value)-1], `\`, "")
			}
			return value
		}
	}
	return ""
}

// nodeAddr reads a node of a forwarded header as an address: IPv4 with or
// without ":port", or IPv6 bare or in brackets with or without ":port"
// (RFC 7239 6). Anything else, "unknown" and obfuscated identifiers
// included, is the zero Addr. A zone is dropped.
func nodeAddr(node string) netip.Addr {
	if strings.HasPrefix(node, "[") {
		end := strings.IndexByte(node, ']')
		if end < 0 {
			return netip.Addr{}
		}
		node = node[1:end]
	} else if strings.Count(node, ":") == 1 {
		node, _, _ = strings.Cut(node, ":")
	}
	a, err := netip.ParseAddr(node)
	if err != nil {
		return netip.Addr{}
	}
	return a.Unmap().WithZone("")
}

// addrOf returns the address of a connection's "ip:port", the zero Addr
// when it is not one.
func addrOf(hostPort string) netip.Addr {
	ap, err := netip.ParseAddrPort(hostPort)
	if err != nil {
		return netip.Addr{}
	}
	return ap.Addr().Unmap().WithZone("")
}
