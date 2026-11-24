package server

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"github.com/amrshadid/nopacs/internal/core/settings"
)

// mustNetworks parses a list of networks a test wrote.
func mustNetworks(t *testing.T, v string) settings.Networks {
	t.Helper()
	n, err := settings.ParseNetworks(v)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// TestCallerBehindProxies guards the reading of a forwarded client address
// (RFC 7239 5.2, and X-Forwarded-For): from the right, past trusted proxies,
// to the first address that is not one; from the one header the
// installation names, the other ignored even when it is the only one;
// several fields read as one list; and a node that is not an address ending
// the walk at the last proxy that can be vouched for.
func TestCallerBehindProxies(t *testing.T) {
	trusted := mustNetworks(t, "10.0.0.0/8,2001:db8::/32")
	xff, fwd := settings.ForwardedXFF, settings.ForwardedRFC7239
	for _, tc := range []struct {
		name   string
		header settings.ForwardedHeader
		h      http.Header
		want   string
	}{
		{"one hop", xff, http.Header{"X-Forwarded-For": {"203.0.113.9"}}, "203.0.113.9"},
		{"the default is X-Forwarded-For", "", http.Header{"X-Forwarded-For": {"203.0.113.9"}}, "203.0.113.9"},
		{"a client's own entry is passed over", xff, http.Header{"X-Forwarded-For": {"198.51.100.1, 203.0.113.9, 10.0.0.2"}}, "203.0.113.9"},
		{"two fields are one list", xff, http.Header{"X-Forwarded-For": {"198.51.100.1", "203.0.113.9"}}, "203.0.113.9"},
		{"Forwarded", fwd, http.Header{"Forwarded": {`for=192.0.2.60;proto=http;by=10.0.0.1, For="[2001:db8:cafe::17]:4711"`}}, "192.0.2.60"},
		{"Forwarded, IPv6 client", fwd, http.Header{"Forwarded": {`for="[2001:db9::17]:4711"`}}, "2001:db9::17"},
		{"a client's Forwarded is ignored when X-Forwarded-For is read", xff, http.Header{"Forwarded": {"for=10.9.9.9"}, "X-Forwarded-For": {"203.0.113.9"}}, "203.0.113.9"},
		{"a client's X-Forwarded-For is ignored when Forwarded is read", fwd, http.Header{"Forwarded": {"for=203.0.113.9"}, "X-Forwarded-For": {"10.9.9.9"}}, "203.0.113.9"},
		{"with a port", xff, http.Header{"X-Forwarded-For": {"203.0.113.9:5000"}}, "203.0.113.9"},
		{"every entry trusted", xff, http.Header{"X-Forwarded-For": {"10.0.0.7, 10.0.0.8"}}, "10.0.0.7"},
		{"unknown ends the walk", fwd, http.Header{"Forwarded": {"for=unknown, for=10.0.0.5"}}, "10.0.0.5"},
		{"garbage ends the walk", xff, http.Header{"X-Forwarded-For": {"203.0.113.9, <script>, 10.0.0.5"}}, "10.0.0.5"},
		{"a quoted comma does not split", fwd, http.Header{"Forwarded": {`for=192.0.2.1;x="a,b"`}}, "192.0.2.1"},
	} {
		got, ok := callerBehind(tc.h, trusted, tc.header)
		if !ok || got.String() != tc.want {
			t.Errorf("%s: %v, %v; want %s", tc.name, got, ok, tc.want)
		}
	}
	for _, tc := range []struct {
		header settings.ForwardedHeader
		h      http.Header
	}{
		{xff, http.Header{}}, {xff, http.Header{"X-Forwarded-For": {"nonsense"}}}, {fwd, http.Header{"Forwarded": {"by=10.0.0.1"}}},
		// Only the other header holds an address: nothing is believed.
		{xff, http.Header{"Forwarded": {"for=10.9.9.9"}}}, {fwd, http.Header{"X-Forwarded-For": {"10.9.9.9"}}},
	} {
		if a, ok := callerBehind(tc.h, trusted, tc.header); ok {
			t.Errorf("%s %v: %v from a header with no address", tc.header, tc.h, a)
		}
	}
	long := strings.Repeat("10.0.0.1,", 100) + "10.0.0.2"
	if a, _ := callerBehind(http.Header{"X-Forwarded-For": {long}}, trusted, xff); !mustNetworks(t, "10.0.0.0/8").Contains(a) {
		t.Errorf("a header of a hundred proxies read as %v", a)
	}
}

// TestClientWrittenForwardedIsNotBelieved guards a proxy that appends
// X-Forwarded-For and passes a client's Forwarded header through: the
// client, at 203.0.113.9, writes "Forwarded: for=10.9.9.9" to pass for an
// allowed address or to have another caller's address blocked. With the
// default header the request is judged by the address the proxy appended.
func TestClientWrittenForwardedIsNotBelieved(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/x", nil)
	r.RemoteAddr = "127.0.0.1:5555"
	r.Header.Set("Forwarded", "for=10.9.9.9")
	r.Header.Set("X-Forwarded-For", "203.0.113.9")
	o := originOf(context.Background(), r, mustNetworks(t, "127.0.0.1"), "")
	if o.addr != netip.MustParseAddr("203.0.113.9") || o.remote != "203.0.113.9" {
		t.Errorf("caller judged as %q, want the proxy's 203.0.113.9", o.remote)
	}
}

// TestOriginOf guards which address a request is judged by: the peer when it
// is not a trusted proxy, whatever the headers say; the forwarded client when
// it is; and the client certificate's fingerprint when one was presented.
func TestOriginOf(t *testing.T) {
	ctx := context.Background()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "192.0.2.50:4000"
	r.Header.Set("X-Forwarded-For", "203.0.113.9")
	if o := originOf(ctx, r, nil, ""); o.remote != "192.0.2.50:4000" || o.addr.String() != "192.0.2.50" || o.local {
		t.Errorf("untrusted peer: %+v", o)
	}
	if o := originOf(ctx, r, mustNetworks(t, "192.0.2.50"), ""); o.remote != "203.0.113.9" || o.addr.String() != "203.0.113.9" {
		t.Errorf("trusted peer: %+v", o)
	}
	r.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{{Raw: []byte("certificate")}}}
	sum := sha256.Sum256([]byte("certificate"))
	if o := originOf(ctx, r, nil, ""); o.clientCert != hex.EncodeToString(sum[:]) {
		t.Errorf("fingerprint %q", o.clientCert)
	}
	local := context.WithValue(ctx, localKey{}, localPeer{user: "501"})
	if o := originOf(local, r, mustNetworks(t, "0.0.0.0/0"), ""); !o.local || o.remote != "" {
		t.Errorf("local socket: %+v", o)
	}
}

// TestAddressKey guards what an address is counted under: an IPv4 address
// alone, an IPv6 address by its /64.
func TestAddressKey(t *testing.T) {
	for in, want := range map[string]string{
		"192.0.2.7":          "192.0.2.7",
		"2001:db8:1:2:3::9":  "2001:db8:1:2::/64",
		"2001:db8:1:2:ff::1": "2001:db8:1:2::/64",
		"::ffff:192.0.2.7":   "192.0.2.7",
	} {
		if got := addressKey(netip.MustParseAddr(in)); got != want {
			t.Errorf("%s: %s, want %s", in, got, want)
		}
	}
}
