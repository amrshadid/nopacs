package server

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/amrshadid/nopacs/internal/core/settings"
)

// Cross-origin requests from pages in a browser (the Fetch standard, "CORS
// protocol"). A browser lets a page read an answer from another origin only
// when the answer names the page's origin in Access-Control-Allow-Origin,
// and sends a preflight (an OPTIONS request without the page's headers or
// credentials) before a request that carries a header outside the
// CORS-safelisted few, as every call of the API does (Authorization, a
// Content-Type of application/proto or application/json). The server is
// closed to pages by default: only an origin the installation's
// api.cors-origins lists, exactly, gets any CORS header; an origin not
// listed gets none, and its page cannot read the answer or send the call.
//
// What a browser client sends and reads is what @connectrpc/connect-web
// needs, as Connect's documentation lists it (connectrpc.com/docs/cors):
// the methods GET and POST; the request headers Content-Type,
// Connect-Protocol-Version, Connect-Timeout-Ms (the Connect protocol),
// Grpc-Timeout, X-Grpc-Web (gRPC-Web) and X-User-Agent; and the response
// headers Grpc-Status, Grpc-Message and Grpc-Status-Details-Bin exposed,
// which a gRPC-Web client reads from the answer's headers when an error
// has no body. The same documentation says a header such as Authorization
// must be listed by name: "*" in Access-Control-Allow-Headers does not
// cover it (the Fetch standard, "CORS-non-wildcard request-header name").
// To those the server adds what its own clients use: Authorization for the
// key, HEAD, Range and If-Range for a download resumed (RFC 9110 14.2,
// 13.1.5), and Nopacs-Request-Id, ETag, Content-Range and Accept-Ranges
// exposed, so a page can quote the request id of a failure and check a
// download against its SHA-256. The tenant's DICOMweb service adds Accept
// and Warning. Accept is CORS-safelisted only while its value holds none
// of the bytes the Fetch standard calls unsafe, which include the double
// quote, and a WADO-RS media type names its parts' type in quotes
// (multipart/related; type="application/dicom", PS3.18 8.7.3), so such a
// request is preflighted and Accept must be allowed. Warning is how a
// QIDO-RS answer says that more results exist than it lists, or that
// fuzzy matching is not done (PS3.18 10.6.3.2, 8.3.4.4, 10.6.1.2.2),
// which a page can read only when it is exposed.
//
// No answer ever carries Access-Control-Allow-Credentials. A key travels
// in the Authorization header, set by the page's own code, which the Fetch
// standard does not count as credentials (cookies, HTTP authentication a
// browser keeps, TLS client certificates), so a client never needs them;
// and without them "*" stays what the standard makes it, an answer any
// page may read but that no browser sends a user's cookies to.
const (
	corsAllowMethods = "GET, HEAD, POST"
	corsAllowHeaders = "Authorization, Content-Type, Connect-Protocol-Version, Connect-Timeout-Ms, " +
		"Grpc-Timeout, X-Grpc-Web, X-User-Agent, Range, If-Range, Accept"
	corsExposeHeaders = "Nopacs-Request-Id, Grpc-Status, Grpc-Message, Grpc-Status-Details-Bin, " +
		"ETag, Content-Range, Accept-Ranges, Warning"
	// corsMaxAge is how long a browser may keep a preflight's answer: two
	// hours, the most Chromium keeps one (Connect's documentation gives
	// the same number), so a page does not send a preflight before every
	// call, and an origin removed from the list is refused within two
	// hours by a browser that already asked.
	corsMaxAge = 2 * time.Hour
)

// corsRefresh is how long the server keeps the list of origins it read
// before it reads the installation's setting again: a change takes effect
// within 10 seconds, and a page that calls the API many times a second
// costs the master one read every 10 seconds.
const corsRefresh = 10 * time.Second

// corsReadTimeout bounds one read of the setting, so a master that does
// not answer holds the request that reads it for at most 2 seconds; the
// list read last is used meanwhile.
const corsReadTimeout = 2 * time.Second

// corsPolicy is which origins the server answers to, as last read from the
// installation's setting.
type corsPolicy struct {
	read func(ctx context.Context) (settings.Origins, error)
	log  *slog.Logger
	now  func() time.Time

	mu      sync.Mutex
	origins settings.Origins
	until   time.Time
	reading bool
}

// current returns the origins allowed. When the list read last is older
// than corsRefresh, the one request that finds it so reads it again, under
// corsReadTimeout, while every other request uses the old list; a read
// that fails keeps the old list (none, before the first read that works),
// and is tried again after corsRefresh, so the server stays as open or as
// closed as it last knew it to be and a master that does not answer is not
// asked on every request.
func (p *corsPolicy) current(ctx context.Context) settings.Origins {
	if p.read == nil {
		return settings.Origins{}
	}
	p.mu.Lock()
	if p.reading || p.now().Before(p.until) {
		o := p.origins
		p.mu.Unlock()
		return o
	}
	p.reading = true
	p.mu.Unlock()

	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), corsReadTimeout)
	o, err := p.read(rctx)
	cancel()

	p.mu.Lock()
	defer p.mu.Unlock()
	p.reading = false
	p.until = p.now().Add(corsRefresh)
	if err != nil {
		// The error names the setting and why it could not be read; it
		// never holds the value (package settingvalue).
		p.log.Warn("the allowed CORS origins could not be read; the last list read is kept", "error", err)
		return p.origins
	}
	p.origins = o
	return o
}

// decorate sets the CORS headers of the answer to r: Vary: Origin always,
// so a cache never gives one origin's answer to another (the Fetch
// standard, "CORS protocol and HTTP caches"); and, only when r carries one
// Origin header that the installation allows, Access-Control-Allow-Origin
// (the origin itself, or "*" when every origin is allowed) and
// Access-Control-Expose-Headers. It reports whether the origin is allowed.
// More than one Origin header is not an origin a browser sends, and is
// allowed none.
func (p *corsPolicy) decorate(h http.Header, r *http.Request) bool {
	h.Add("Vary", "Origin")
	values := r.Header.Values("Origin")
	if len(values) != 1 {
		return false
	}
	o := p.current(r.Context())
	if !o.Allows(values[0]) {
		return false
	}
	if o.Any {
		h.Set("Access-Control-Allow-Origin", "*")
	} else {
		h.Set("Access-Control-Allow-Origin", values[0])
	}
	h.Set("Access-Control-Expose-Headers", corsExposeHeaders)
	return true
}

// isPreflight reports whether r is a CORS-preflight request: OPTIONS with
// an Origin and an Access-Control-Request-Method header (the Fetch
// standard, "CORS-preflight request"). Any other OPTIONS request is an
// ordinary request, checked like every other.
func isPreflight(r *http.Request) bool {
	return r.Method == http.MethodOptions && r.Header.Get("Origin") != "" && r.Header.Get("Access-Control-Request-Method") != ""
}

// answerPreflight answers a preflight from an allowed origin, whose
// Access-Control-Allow-Origin decorate has set: 204 with the methods and
// headers the API's clients send, for every path alike, so the answer says
// nothing of which paths are served. A preflight carries no key (the
// Fetch standard leaves credentials and the page's headers out of it), so
// it is not authenticated, and it is answered before the front step and
// not audited, like the health service: it reads nothing and changes
// nothing, and the request it announces is checked and audited as every
// request is. The answer varies with the asked method and headers too, as
// a cache must know.
func answerPreflight(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Access-Control-Allow-Methods", corsAllowMethods)
	h.Set("Access-Control-Allow-Headers", corsAllowHeaders)
	h.Set("Access-Control-Max-Age", strconv.Itoa(int(corsMaxAge/time.Second)))
	h.Add("Vary", "Access-Control-Request-Method")
	h.Add("Vary", "Access-Control-Request-Headers")
	w.WriteHeader(http.StatusNoContent)
}
