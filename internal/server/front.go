package server

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect/v2"

	"github.com/amrshadid/nopacs/internal/access"
	"github.com/amrshadid/nopacs/internal/fault"
)

// The audit entries the front step writes for a caller it authenticated
// whose request no handler took up. The contract has every request write
// one entry; each of these is failed, as a request that did not do what it
// asked.
const (
	// notServedReason is the reason of a request for a path nothing serves.
	notServedReason = "no handler serves this method"
	// notServedDetail is the detail key of that entry, with the value
	// "false".
	notServedDetail = "served"
	// uncheckedReason is the reason of a request for a served method that
	// was answered before its check: the RPC library refused its form (the
	// HTTP method, the content type, the compression, the timeout header,
	// the codec), so the check never ran and no handler did.
	uncheckedReason = "answered before the check: not a well-formed call of this method"
	// statusDetail is the detail key that records the HTTP status of that
	// answer: three digits, whatever the request sent.
	statusDetail = "http_status"
)

// The points of a request at which a test may stop it (Server.at), to
// show what each moment of a cancel leaves in the audit log.
const (
	// pointFront is the front step, before the credential is judged.
	pointFront = "front"
	// pointChecked is the request path, after the check allowed the
	// request and before its handler runs.
	pointChecked = "checked"
	// pointReceive is the handler's read of the request's message from
	// the wire, before it reads.
	pointReceive = "receive"
	// pointHandled is the request path, after the handler returned and
	// before the entry of a request that has none is written.
	pointHandled = "handled"
)

// at runs the server's test hook at point, if one is set; in a server
// that is serving, it is nil and at does nothing.
func (s *Server) at(ctx context.Context, point string) {
	if s.hook != nil {
		s.hook(ctx, point)
	}
}

// noSuchMethod and noSuchRoute are the answers to an authenticated caller
// who asks for something nothing serves: UNIMPLEMENTED in an RPC protocol,
// NOT_FOUND to a plain HTTP request (RFC 9110 15.5.5).
const (
	noSuchMethod = "no such method"
	noSuchRoute  = "no such route"
)

// identityKey is the context key of the credential the front step
// accepted for a request.
type identityKey struct{}

// identityFrom returns the credential the front step accepted for the
// request ctx belongs to.
func identityFrom(ctx context.Context) (access.Credential, bool) {
	c, ok := ctx.Value(identityKey{}).(access.Credential)
	return c, ok
}

// public reports whether r asks for one of the paths anyone may call
// without a key: the health service's Check and Watch, matched exactly
// (isPath). Nothing else is public.
func public(r *http.Request) bool {
	return isPath(r, HealthCheckProcedure) || isPath(r, HealthWatchProcedure)
}

// isPath reports whether r's path is exactly p, as sent: byte for byte, case
// included, with no percent-encoding (an empty raw path). It is the one
// match by which the server names a path as public or as the health check.
func isPath(r *http.Request, p string) bool {
	return r.URL.RawPath == "" && r.URL.Path == p
}

// front is the step every request takes before any route is chosen, inside
// the envelope. It decides who the caller is and nothing else:
//
//  1. A public path (public) goes straight to the router, unauthenticated,
//     as the health service has always been.
//  2. Otherwise the credential is read from the request's headers alone (a
//     bearer key, else the local operator on the local socket, else none)
//     and Authenticate judges it, reading the key once.
//  3. A caller who is not authenticated is answered at once, by
//     writeWireError, with a refusal chosen by the request's protocol
//     family alone (wireOf): not by the path, the HTTP method, whether
//     anything serves the path, or the body, which is never read.
//  4. Only an authenticated caller reaches the router (route), with the
//     accepted credential in its context, which the request path then uses.
//
// Each request writes exactly one audit entry, but for those deliberately
// not audited: a request to a public path (the health service), a request
// refused because the server is stopping (answered by the envelope before
// this step), and an allowed scrape of the metrics. The one entry is
// written by:
//
//   - Authenticate, for a caller who is not authenticated (refused), or
//     whose check could not finish (failed);
//   - notServed, for a path nothing serves (failed, served=false);
//   - the check, for a caller it refuses;
//   - the handler, for a request the check allowed; or pass, when such a
//     request ends without one (its message could not be read or came too
//     late, the handler failed, panicked, was canceled or ran past its
//     deadline, or succeeded without recording);
//   - this step, when the request was answered and pass never took it up
//     (the RPC library refused the call's form before the check): failed,
//     with the HTTP status.
func (s *Server) front(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if public(r) {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		ctx := r.Context()
		id, _ := requestIDFrom(ctx)
		method, metric := s.nameOf(r)
		x := exchange{method: method, metric: metric}
		refuse := func(err error) {
			ce := s.answer(err)
			writeWireError(w, wireOf(r), ce.Code(), ce.Message())
			if r.ProtoMajor == 1 {
				// The answer says Connection: close, but net/http reads what
				// is left of a body (up to 256 KiB) looking for its end
				// before it closes, which a body that never ends stretches
				// to the read bound. Nothing of this request is read: the
				// read deadline is moved to now, so the connection closes as
				// soon as the refusal is written.
				_ = http.NewResponseController(w).SetReadDeadline(time.Now())
			}
			s.finish(ctx, x, &Call{RequestID: id, Method: method}, start, err)
		}
		// The local socket is judged by no network rule nor limit, so its
		// requests do not read the rules: the operator on the machine is
		// let in even when they cannot be read.
		var rules AccessRules
		if _, isLocal := localFrom(ctx); !isLocal {
			var ok bool
			if rules, ok = s.guard.current(ctx); !ok {
				// The installation's access settings were never read: the
				// master does not answer, and no request can be judged.
				refuse(errRulesUnread)
				return
			}
		}
		o := originOf(ctx, r, rules.TrustedProxies, rules.ForwardedHeader)
		ctx = context.WithValue(ctx, originKey{}, o)
		x.remote = o.remote
		local, isLocal := localFrom(ctx)
		facts := access.Request{Method: method, RemoteAddress: o.remote, RequestID: id, Local: o.local, ClientCert: o.clientCert}
		s.at(ctx, pointFront)
		// The installation's networks are judged before anything else, and
		// a blocked address before its key is read: neither costs the
		// master a read.
		if rule := installationRefusal(rules, o); rule != "" {
			// Counted and slowed as a bad key is, so a refused network
			// cannot probe the server at full speed either.
			err := s.access.RecordRefusal(facts, "", rule, false, time.Time{})
			if s.failAddress(o, rules, facts) {
				s.guard.wait(ctx)
			}
			refuse(err)
			return
		}
		if !o.local && o.addr.IsValid() {
			if blocked, _ := s.guard.addresses.blocked(addressKey(o.addr), s.guard.now()); blocked {
				refuse(s.access.RecordRefusal(facts, "", access.RuleAddressBlocked, false, time.Time{}))
				return
			}
		}
		accepted, err := s.access.Authenticate(ctx, credentialFrom(r.Header, local, isLocal), facts)
		if err != nil {
			if fault.KindOf(err) == fault.Unauthenticated && s.failAddress(o, rules, facts) {
				s.guard.wait(ctx)
			}
			refuse(err)
			return
		}
		if keyID := accepted.KeyID(); keyID != "" && !o.local {
			if blocked, _ := s.guard.keys.blocked(keyID, s.guard.now()); blocked {
				refuse(s.access.RecordRefusal(facts, keyID, access.RuleKeyBlocked, false, time.Time{}))
				return
			}
		}
		ctx = context.WithValue(ctx, identityKey{}, accepted)
		defer func() {
			if m, ok := ctx.Value(pathMarkKey{}).(*pathMark); ok && m.audited.Load() {
				return
			}
			f := facts
			if sw, ok := w.(*statusWriter); ok {
				f.Details = map[string]string{statusDetail: strconv.Itoa(sw.Status())}
			}
			reason := uncheckedReason
			switch {
			case errors.Is(ctx.Err(), context.Canceled):
				// The caller went away before any check took the request
				// up: what was answered, if anything, is not its fault.
				reason = reasonCanceled
			case errors.Is(ctx.Err(), context.DeadlineExceeded):
				reason = reasonDeadline
			}
			s.access.RecordUnchecked(accepted, f, reason)
		}()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// errRulesUnread answers a request when the installation's access settings
// have never been read: the master does not answer, so no request can be
// judged by them, and none is let through unjudged.
var errRulesUnread = fault.New(fault.Unavailable, "the master database does not answer")

// credentialFrom reads the credential of a request from its headers and
// the connection it came on. More than one Authorization header is a
// malformed key: the field holds one credential (RFC 9110 11.6.2), and
// which of several to believe is not the server's to guess.
func credentialFrom(h http.Header, local localPeer, isLocal bool) access.Credential {
	switch v := h["Authorization"]; len(v) {
	case 0:
		return credential("", local, isLocal)
	case 1:
		return credential(v[0], local, isLocal)
	default:
		return access.APIKey(malformedKey)
	}
}

// nameOf returns the name a request is audited and logged under, and its
// metric label. A served procedure or a plain route, asked for exactly, is
// named by itself; any other path by methodName (the path when it has the
// shape of a procedure, else one fixed name) and counted under "unknown",
// so a caller cannot write what it likes into the log or make metric series
// without bound. A path with percent-encoding is never exact: it is asked
// for under a name the server does not serve.
func (s *Server) nameOf(r *http.Request) (method, metric string) {
	if m, ok := s.matchPattern(r); ok {
		// A pattern route is named by its pattern whatever its path holds:
		// the path holds the tenant's labels and ids, which no log keeps.
		return m.route.Pattern, m.route.Pattern
	}
	if r.URL.RawPath != "" {
		return unnamedMethod, unknownMetric
	}
	p := r.URL.Path
	if _, ok := s.procs[p]; ok {
		return p, p
	}
	if pattern, ok := s.plain[p]; ok {
		return pattern, pattern
	}
	return methodName(p), unknownMetric
}

// unknownMetric is the metric label of every request for a path nothing
// serves.
const unknownMetric = "unknown"

// route is the router, which only a request front let through reaches:
// one whose caller is authenticated, or one for a public path. It
// dispatches a served procedure to its handler from the one checked
// connect.Server, and a plain route's path to the mux, which holds the
// route's HTTP method, by exact match only; a path with the shape and the
// method of a pattern route to servePattern, which checks the route's
// requirement before it reads the rest of the path; anything else, a path
// with any percent-encoding, a double slash, a trailing slash or a
// different case included, is answered by notServed and reaches no
// handler.
func (s *Server) route(w http.ResponseWriter, r *http.Request) {
	if r.URL.RawPath == "" {
		if h, ok := s.procs[r.URL.Path]; ok {
			h.ServeHTTP(w, r)
			return
		}
		if _, ok := s.plain[r.URL.Path]; ok {
			s.mux.ServeHTTP(w, r)
			return
		}
	}
	if m, ok := s.matchPattern(r); ok {
		s.servePattern(w, r, m)
		return
	}
	s.notServed(w, r)
}

// notServed answers an authenticated caller who asked for something nothing
// serves, over any HTTP version: UNIMPLEMENTED in its RPC protocol, NOT_FOUND
// to a plain HTTP request; and writes the request's one audit entry, failed,
// with the detail served=false, in the installation's log. The request is
// logged and counted as one request path's would be, under the label
// "unknown".
func (s *Server) notServed(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	ctx := r.Context()
	id, _ := requestIDFrom(ctx)
	method, _ := s.nameOf(r)
	x := exchange{method: method, metric: unknownMetric, remote: remoteOf(ctx, r.RemoteAddr)}
	p := wireOf(r)
	ce := connect.NewError(connect.CodeUnimplemented, noSuchMethod)
	if !p.rpc() {
		ce = connect.NewError(connect.CodeNotFound, noSuchRoute)
	}
	if cred, ok := identityFrom(ctx); ok {
		s.access.RecordUnchecked(cred, access.Request{
			Method: method, RemoteAddress: x.remote, RequestID: id,
			Details: map[string]string{notServedDetail: "false"},
		}, notServedReason)
		// The caller was authenticated; as for an allowed request, its
		// connection may be kept.
		markAuthenticated(ctx)
	}
	markAudited(ctx)
	writeWireError(w, p, ce.Code(), ce.Message())
	s.finish(ctx, x, &Call{RequestID: id, Method: method}, start, ce)
}

// plainPattern checks a plain route's pattern and returns its path: an
// exact path, with or without an HTTP method before it, and no wildcard, no
// trailing slash (a subtree), no host, nothing a client could write in more
// than one way. The router matches a path exactly, so a pattern that is not
// one path could never be reached as written.
func plainPattern(pattern string) (string, bool) {
	p := pattern
	if m, rest, ok := strings.Cut(pattern, " "); ok {
		if m == "" || strings.ContainsAny(m, "/{}") {
			return "", false
		}
		p = rest
	}
	if len(p) < 2 || p[0] != '/' || p[len(p)-1] == '/' || strings.ContainsAny(p, "{}%? \t") || strings.Contains(p, "//") ||
		strings.Contains(p, "/./") || strings.Contains(p, "/../") || strings.HasSuffix(p, "/.") || strings.HasSuffix(p, "/..") {
		return "", false
	}
	return p, true
}
