package server

import (
	"context"
	"net/http"
	"strings"

	"connectrpc.com/connect/v2"
	"google.golang.org/protobuf/proto"

	"github.com/amrshadid/nopacs/internal/access"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
)

// PatternRoute is a plain HTTP route inside one tenant, whose path names
// the tenant and the things it serves: the download of an instance, and
// DICOMweb. Its pattern is an http.ServeMux pattern with an HTTP method,
// a literal first segment, and a "{tenant}" wildcard, such as
//
//	GET /tenants/{tenant}/instances/{subject}/{session}/{scan}/{instance}
//
// A last wildcard may be "{name...}", for a subtree served by one handler.
// The requirement is computed for each request, from its path, before any
// handler runs (Need), and checked in the request path like every other
// route's.
type PatternRoute struct {
	// Pattern is the route, as above. It is also the method the request's
	// audit entry, log line and metrics name: never the request's path.
	Pattern string
	// Need returns what the request needs of its caller and its audit
	// facts (Object and Details). It reads the path through r.PathValue:
	// "tenant" is the path's tenant name when it follows the rule of a
	// tenant's name and empty otherwise, so Need never sees a malformed
	// one; every other value is the path's segment as sent (for a
	// "{name...}" wildcard, the segments joined by "/"), never decoded and
	// not yet checked, since a refusal must not depend on it.
	Need func(r *http.Request) (access.Requirement, access.Request)
	// Handler serves the request once the check has allowed it and every
	// value of the path has passed segmentOK. NotImplemented stands in for
	// a route whose handler is not written yet.
	Handler HTTPHandler
}

// notImplementedMessage is the answer of a route whose handler is not
// written yet.
const notImplementedMessage = "this route is not served yet"

// NotImplemented is the handler of a route registered before the code that
// serves it: the route is checked and audited as it will be, and an allowed
// request is answered 501 Not Implemented (RFC 9110 15.6.2), so the change
// that serves it only replaces this handler.
func NotImplemented(context.Context, http.ResponseWriter, *http.Request) error {
	return connect.NewError(connect.CodeUnimplemented, notImplementedMessage)
}

// errNoSuchPath answers an allowed request to a pattern route whose path is
// not one any of its values could be (segmentOK): 404, never a redirect to
// a cleaned path (data.proto, "Lookup"), and only after the check, so a
// caller without a grant on the tenant learns nothing from the rest of the
// path.
var errNoSuchPath = connect.NewError(connect.CodeNotFound, "not found")

// tenantWildcard is the wildcard every pattern route names its tenant by.
const tenantWildcard = "tenant"

// maxSegment bounds one segment of a pattern route's path: a label, an id
// or a UID of DICOM (64 bytes, PS3.5 6.2) fits many times over, and a
// segment longer than this names nothing the archive holds.
const maxSegment = 256

// maxPathSegments bounds the segments of a path the server will split to
// match a pattern route: deeper than any route of DICOMweb (PS3.18 10.4,
// at most nine), so a path of thousands of "/" costs no more than a short
// one.
const maxPathSegments = 32

// patternSeg is one segment of a pattern: a literal, or a wildcard's name.
type patternSeg struct {
	lit, name string
}

// wild reports whether the segment is a wildcard.
func (p patternSeg) wild() bool { return p.name != "" }

// patternRoute is a PatternRoute as the router holds it.
type patternRoute struct {
	PatternRoute
	// method is the pattern's HTTP method; GET also serves HEAD, as
	// http.ServeMux does (RFC 9110 9.3.2).
	method string
	// segs are the path's segments; the last is "{name...}" when rest.
	segs []patternSeg
	rest bool
}

// parsePattern reads a PatternRoute's pattern, or says why it is not one.
func parsePattern(pattern string) (*patternRoute, error) {
	bad := func(why string) error {
		return fault.New(fault.Internal, "server: the route "+pattern+" "+why)
	}
	method, path, ok := strings.Cut(pattern, " ")
	if !ok || method == "" || strings.Trim(method, "ABCDEFGHIJKLMNOPQRSTUVWXYZ") != "" {
		return nil, bad("does not start with an HTTP method")
	}
	if len(path) < 2 || path[0] != '/' || path[len(path)-1] == '/' {
		return nil, bad("is not a path without a trailing slash")
	}
	pr := &patternRoute{method: method}
	names := map[string]bool{}
	parts := strings.Split(path[1:], "/")
	for i, part := range parts {
		inner, isWild := strings.CutPrefix(part, "{")
		if !isWild {
			if !segmentOK(part) {
				return nil, bad("has a literal segment that a path cannot carry as it is")
			}
			pr.segs = append(pr.segs, patternSeg{lit: part})
			continue
		}
		name, ok := strings.CutSuffix(inner, "}")
		if !ok {
			return nil, bad("has a wildcard without its closing brace")
		}
		if n, isRest := strings.CutSuffix(name, "..."); isRest {
			if i != len(parts)-1 {
				return nil, bad("has a {name...} wildcard that is not last")
			}
			name, pr.rest = n, true
		}
		if !wildcardName(name) || names[name] {
			return nil, bad("has a wildcard name that is not a-z, 0-9 and _, or is given twice")
		}
		names[name] = true
		pr.segs = append(pr.segs, patternSeg{name: name})
	}
	switch {
	case pr.segs[0].wild():
		return nil, bad("starts with a wildcard, which would take paths of every other route")
	case !names[tenantWildcard] || (pr.rest && pr.segs[len(pr.segs)-1].name == tenantWildcard):
		return nil, bad("names no {tenant} segment")
	case len(pr.segs) > maxPathSegments:
		return nil, bad("is deeper than a path the router splits")
	}
	return pr, nil
}

// wildcardName reports whether s is a wildcard name the router accepts:
// a-z first, then a-z, 0-9 and "_".
func wildcardName(s string) bool {
	if s == "" || s[0] < 'a' || s[0] > 'z' {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '_' {
			return false
		}
	}
	return true
}

// segmentOK reports whether s is a segment a pattern route's path may hold:
// 1 to maxSegment bytes, each a character RFC 3986 3.3 lets a segment
// carry as it is (pchar without percent-encoding), and not "." or "..". A
// path with any percent-encoding ("%2F", "%00", "%2E%2E" included), an
// empty segment (a double or a trailing slash) or a dot segment names
// nothing (data.proto, "Lookup"): a label and an id hold only characters a
// segment carries as they are, and a dot segment is what a cleaning of the
// path would remove, so it could make two paths name one thing.
func segmentOK(s string) bool {
	if s == "" || len(s) > maxSegment || s == "." || s == ".." {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !pchar(s[i]) {
			return false
		}
	}
	return true
}

// pchar reports whether c is a character RFC 3986 3.3 lets a path segment
// carry as it is: unreserved, a sub-delimiter, ":" or "@".
func pchar(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	}
	return strings.IndexByte("-._~!$&'()*+,;=:@", c) >= 0
}

// overlaps reports whether some path could match both a and b, method
// aside.
func (a *patternRoute) overlaps(b *patternRoute) bool {
	for i := 0; i < len(a.segs) && i < len(b.segs); i++ {
		if (a.rest && i == len(a.segs)-1) || (b.rest && i == len(b.segs)-1) {
			return true
		}
		if !a.segs[i].wild() && !b.segs[i].wild() && a.segs[i].lit != b.segs[i].lit {
			return false
		}
	}
	return len(a.segs) == len(b.segs)
}

// sameShape reports whether a and b match exactly the same paths.
func (a *patternRoute) sameShape(b *patternRoute) bool {
	if len(a.segs) != len(b.segs) || a.rest != b.rest {
		return false
	}
	for i := range a.segs {
		if a.segs[i].wild() != b.segs[i].wild() || a.segs[i].lit != b.segs[i].lit {
			return false
		}
	}
	return true
}

// serves reports whether the route serves the HTTP method m.
func (a *patternRoute) serves(m string) bool {
	return m == a.method || (a.method == http.MethodGet && m == http.MethodHead)
}

// handlePattern adds a pattern route. Its path may not overlap a plain
// route's or a procedure's, nor another pattern route's unless both match
// exactly the same paths for different methods: the router never chooses
// between two routes by which is more specific, so which route serves a
// path is plain from the table.
func (s *Server) handlePattern(r PatternRoute) (err error) {
	if r.Handler == nil || r.Need == nil {
		return fault.New(fault.Internal, "server: the route "+r.Pattern+" needs a requirement and a handler")
	}
	pr, err := parsePattern(r.Pattern)
	if err != nil {
		return err
	}
	pr.PatternRoute = r
	exact := make([]string, 0, len(s.plain)+len(s.procs))
	for p := range s.plain {
		exact = append(exact, p)
	}
	for p := range s.procs {
		exact = append(exact, p)
	}
	for _, p := range exact {
		if pr.match(strings.Split(p[1:], "/")) {
			return fault.New(fault.Internal, "server: the route "+r.Pattern+" takes the path "+p)
		}
	}
	for _, o := range s.patterns {
		if pr.overlaps(o) && (!pr.sameShape(o) || o.serves(pr.method) || pr.serves(o.method)) {
			return fault.New(fault.Internal, "server: the route "+r.Pattern+" overlaps "+o.Pattern)
		}
	}
	defer func() {
		// http.ServeMux panics on a pattern it cannot take; at setup that is
		// a bug to report, not a crash.
		if p := recover(); p != nil {
			err = fault.New(fault.Internal, "server: the route "+r.Pattern+" cannot be registered")
		}
	}()
	// Recorded on the mux so Routes lists it and http.ServeMux checks its
	// syntax; the router dispatches it itself (route), on the path as sent,
	// which the mux would clean and decode first.
	s.mux.Handle(r.Pattern, http.HandlerFunc(s.route))
	s.patterns = append(s.patterns, pr)
	s.patternHeads[pr.segs[0].lit] = true
	return nil
}

// match reports whether the segments of a path, as sent, have the route's
// shape: as many (at least as many, for a "{name...}" route), with each
// literal equal byte for byte. The values are not checked here.
func (a *patternRoute) match(segs []string) bool {
	if len(segs) < len(a.segs) || (!a.rest && len(segs) != len(a.segs)) {
		return false
	}
	for i, p := range a.segs {
		if !p.wild() && segs[i] != p.lit {
			return false
		}
	}
	return true
}

// loggedPath is a request's path as a log line the envelope writes holds
// it (a preflight, an answer given before the request path ran, a panic
// outside a handler): under a pattern route's first segment, that segment
// alone, since the rest names a tenant's labels and ids, which no log
// keeps (data.proto, "Logs"); any other path cut to maxLoggedPath.
func (s *Server) loggedPath(r *http.Request) string {
	p := r.URL.EscapedPath()
	if len(p) > 1 && p[0] == '/' {
		head, _, found := strings.Cut(p[1:], "/")
		if found && s.patternHeads[head] {
			return "/" + head + "/..."
		}
	}
	return cut(r.URL.Path, maxLoggedPath)
}

// patternMatch is what the router found for a request among the pattern
// routes.
type patternMatch struct {
	route *patternRoute
	segs  []string
}

// matchPattern returns the pattern route that serves r: its path as sent
// (the escaped form, never decoded) has the route's shape and its method
// is the route's. A path with the shape of a route but another method is
// served by none, and answered as a path nothing serves.
func (s *Server) matchPattern(r *http.Request) (patternMatch, bool) {
	if len(s.patterns) == 0 {
		return patternMatch{}, false
	}
	p := r.URL.EscapedPath()
	if len(p) < 2 || p[0] != '/' || strings.Count(p, "/") > maxPathSegments {
		return patternMatch{}, false
	}
	head, _, _ := strings.Cut(p[1:], "/")
	if !s.patternHeads[head] {
		return patternMatch{}, false
	}
	segs := strings.Split(p[1:], "/")
	for _, pr := range s.patterns {
		if pr.serves(r.Method) && pr.match(segs) {
			return patternMatch{route: pr, segs: segs}, true
		}
	}
	return patternMatch{}, false
}

// values sets the path values the route's Need and handler read, and
// reports whether every one passes segmentOK and the tenant's name its
// rule. The tenant's value is set only when it is a valid name, so a
// malformed one never reaches Need, which then holds the request to the
// role on the installation.
func (m patternMatch) values(r *http.Request) bool {
	clean := true
	for i, p := range m.route.segs {
		if !p.wild() {
			continue
		}
		v := m.segs[i]
		if m.route.rest && i == len(m.route.segs)-1 {
			for _, s := range m.segs[i:] {
				clean = clean && segmentOK(s)
			}
			v = strings.Join(m.segs[i:], "/")
		} else {
			clean = clean && segmentOK(v)
		}
		if p.name == tenantWildcard {
			if master.ValidateTenantName(v) != nil {
				clean, v = false, ""
			}
		}
		r.SetPathValue(p.name, v)
	}
	return clean
}

// servePattern serves a request the router matched to a pattern route,
// through the request path: the route's requirement, computed from this
// request's path, is checked by pass with the credential the front step
// accepted; then a path whose values do not all pass is answered 404, and
// any other reaches the route's handler. The order is the contract's for
// every request into a tenant: authenticated, then the role on the tenant
// the path names, then the request (data.proto, "Order of checks").
func (s *Server) servePattern(w http.ResponseWriter, r *http.Request, m patternMatch) {
	clean := m.values(r)
	route := m.route
	x := exchange{
		method:        route.Pattern,
		metric:        route.Pattern,
		authorization: r.Header.Get("Authorization"),
		remote:        remoteOf(r.Context(), r.RemoteAddr),
		rule: rule{need: func(proto.Message) (access.Requirement, access.Request) {
			return route.Need(r)
		}},
	}
	err := s.pass(r.Context(), x, func(ctx context.Context) error {
		if !clean {
			return errNoSuchPath
		}
		return route.Handler(ctx, w, r.WithContext(ctx))
	})
	if err != nil {
		s.writeAnyError(w, r, s.answer(err))
	}
}
