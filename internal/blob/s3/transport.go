package s3

import (
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"syscall"
	"time"

	"github.com/amrshadid/nopacs/internal/core/publicaddr"
)

// AllowPublic is an AllowAddress that passes global unicast addresses and
// nothing else (publicaddr.Is): it refuses every block of the IANA
// special-purpose registries, the multicast and reserved space, and every
// IPv6 address outside 2000::/3.
func AllowPublic(a netip.Addr) bool { return publicaddr.Is(a) }

// newTransport returns the HTTP transport a store uses. It reads no
// environment, so HTTP_PROXY and its kind change nothing, and its dialer
// asks s.allow about the address of every connection it makes, before the
// connection is opened: Control runs after the name is resolved, with the
// address about to be connected to, so no window lies between the check and
// the connection in which a resolver could answer differently
// (net.Dialer.Control).
func newTransport(s settings) *http.Transport {
	d := &net.Dialer{
		Timeout:   dialTimeout,
		KeepAlive: 30 * time.Second,
		Resolver:  s.resolver,
		Control: func(_, address string, _ syscall.RawConn) error {
			ap, err := netip.ParseAddrPort(address)
			if err != nil || !s.allow(ap.Addr().Unmap().WithZone("")) {
				return ErrAddressNotAllowed
			}
			return nil
		},
	}
	return &http.Transport{
		Proxy:                 nil,
		DialContext:           d.DialContext,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: s.roots},
		TLSHandshakeTimeout:   dialTimeout,
		ResponseHeaderTimeout: 60 * time.Second,
		ExpectContinueTimeout: time.Second,
		MaxIdleConns:          64,
		MaxIdleConnsPerHost:   64,
		IdleConnTimeout:       90 * time.Second,
	}
}

// maxAnswer bounds every answer that is not an object's bytes, which the
// driver reads whole and parses: a listing page of 1,000 entries, each a key
// of up to 1,024 bytes and about 512 bytes of markup (modified time, ETag,
// size, class, owner), is about 1.5 MB; a list of 1,000 unfinished uploads
// is of the same size; an error or a completion is a few hundred bytes.
// 4 MiB is more than twice the largest, for escaped characters, and far
// below what a hostile endpoint could otherwise make the process hold.
const maxAnswer = 4 << 20

// errAnswerTooLarge marks an answer longer than maxAnswer.
var errAnswerTooLarge = errors.New("the storage's answer is longer than any answer should be")

// objectStream marks a request whose answer, when it is a success, is the
// bytes of an object, which is streamed to the caller and counted there
// against what was asked. The value is carried in the request's context.
type objectStream struct{}

// limitTransport cuts every answer that is not an object's bytes at
// maxAnswer: it refuses at once an answer that says it is longer, and ends
// one that turns out to be, with errAnswerTooLarge, which the driver treats
// as a malformed answer and does not retry. An error status is never an
// object's bytes, whatever the request.
type limitTransport struct{ rt http.RoundTripper }

func (l limitTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := l.rt.RoundTrip(r)
	if err != nil {
		return nil, err
	}
	if r.Context().Value(objectStream{}) != nil && (resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusPartialContent) {
		return resp, nil
	}
	if r.Method != http.MethodHead && resp.ContentLength > maxAnswer { // A HEAD answer states a length and has no body.
		_ = resp.Body.Close() // The answer is refused whatever closing says.
		return nil, errAnswerTooLarge
	}
	resp.Body = &limitedBody{rc: resp.Body, left: maxAnswer}
	return resp, nil
}

// limitedBody reads at most left bytes and then fails.
type limitedBody struct {
	rc   io.ReadCloser
	left int64
}

func (b *limitedBody) Read(p []byte) (int, error) {
	if b.left <= 0 {
		// One byte more tells an answer of exactly the limit from a longer one.
		var one [1]byte
		if n, err := b.rc.Read(one[:]); n > 0 {
			return 0, errAnswerTooLarge
		} else if err != nil {
			return 0, err
		}
		return 0, nil
	}
	if int64(len(p)) > b.left {
		p = p[:b.left]
	}
	n, err := b.rc.Read(p)
	b.left -= int64(n)
	return n, err
}

func (b *limitedBody) Close() error { return b.rc.Close() }

// noRedirect makes the HTTP client return a redirect as the answer it is,
// instead of following it to a server the caller never named.
func noRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
