package server_test

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"

	nopacsv1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/gen/nopacs/v1/nopacsv1connect"
	"github.com/amrshadid/nopacs/internal/core/settings"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/server"
)

// listedOrigin is the one origin the CORS tests' installation allows.
const listedOrigin = "https://viewer.example.org"

// originsOf returns a reading of the allowed origins that answers v.
func originsOf(t *testing.T, v string) func(context.Context) (settings.Origins, error) {
	t.Helper()
	o, err := settings.ParseOrigins(v)
	if err != nil {
		t.Fatal(err)
	}
	return func(context.Context) (settings.Origins, error) { return o, nil }
}

// corsRequest sends method to path from origin (none when empty) with the
// headers given in pairs, and returns the answer's status and headers.
func corsRequest(t *testing.T, e *env, method, path, origin string, headers ...string) (int, http.Header) {
	t.Helper()
	req, err := http.NewRequest(method, e.netURL+path, strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Add(headers[i], headers[i+1])
	}
	resp, err := e.tlsClient(false).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode, resp.Header
}

// corsHeaders returns the names of the CORS headers h holds.
func corsHeaders(h http.Header) []string {
	var out []string
	for k := range h {
		if strings.HasPrefix(k, "Access-Control-") {
			out = append(out, k)
		}
	}
	slices.Sort(out)
	return out
}

// hasToken reports whether the comma-separated header value holds token,
// compared without regard to case.
func hasToken(v, token string) bool {
	for f := range strings.SplitSeq(v, ",") {
		if strings.EqualFold(strings.TrimSpace(f), token) {
			return true
		}
	}
	return false
}

// varies reports whether h's Vary names field.
func varies(h http.Header, field string) bool {
	for _, v := range h.Values("Vary") {
		if hasToken(v, field) {
			return true
		}
	}
	return false
}

// TestCORSPreflight guards the preflight of a listed origin, to a procedure
// and to a pattern route alike: 204 with the origin itself, every method
// and request header a Connect, gRPC-Web, download or DICOMweb client
// sends (Accept, which a WADO-RS media type with quotes takes out of the
// safelist), the response headers they read exposed (Warning, by which
// QIDO-RS says more results exist), a max age, Vary on the origin and
// the asked method and headers, never Access-Control-Allow-Credentials,
// no audit entry, and no line of the log holding the path's ids; and the preflight of an origin not listed: no CORS
// header at all, refused for its missing key like any request.
func TestCORSPreflight(t *testing.T) {
	e := newEnv(t, options{corsOrigins: originsOf(t, listedOrigin+"\nhttp://localhost:5173")})
	count := func() int {
		if err := e.access.FlushAudit(e.ctx); err != nil {
			t.Fatal(err)
		}
		entries, _, err := e.master.ListAudit(e.ctx, master.AuditFilter{}, master.Page{Limit: master.MaxPageLimit})
		if err != nil {
			t.Fatal(err)
		}
		return len(entries)
	}
	before := count()
	const instance = "1.2.826.0.1.3680043.8.498.99"
	for _, path := range []string{nopacsv1connect.AccessServiceWhoAmIProcedure, "/tenants/lab/instances/S01/E01/T1/" + instance, "/nothing/here"} {
		status, h := corsRequest(t, e, "OPTIONS", path, listedOrigin,
			"Access-Control-Request-Method", "POST", "Access-Control-Request-Headers", "authorization,content-type,connect-protocol-version")
		if status != http.StatusNoContent || h.Get("Access-Control-Allow-Origin") != listedOrigin {
			t.Fatalf("%s: a listed preflight answered %d, %v", path, status, h)
		}
		for _, m := range []string{"GET", "HEAD", "POST"} {
			if !hasToken(h.Get("Access-Control-Allow-Methods"), m) {
				t.Errorf("%s: %s is not an allowed method: %q", path, m, h.Get("Access-Control-Allow-Methods"))
			}
		}
		for _, name := range []string{"authorization", "content-type", "connect-protocol-version", "connect-timeout-ms", "grpc-timeout", "x-grpc-web", "x-user-agent", "range", "if-range", "accept"} {
			if !hasToken(h.Get("Access-Control-Allow-Headers"), name) {
				t.Errorf("%s: %s is not an allowed header: %q", path, name, h.Get("Access-Control-Allow-Headers"))
			}
		}
		for _, name := range []string{"nopacs-request-id", "grpc-status", "grpc-message", "grpc-status-details-bin", "etag", "content-range", "accept-ranges", "warning"} {
			if !hasToken(h.Get("Access-Control-Expose-Headers"), name) {
				t.Errorf("%s: %s is not exposed: %q", path, name, h.Get("Access-Control-Expose-Headers"))
			}
		}
		if h.Get("Access-Control-Max-Age") != "7200" || h.Get("Access-Control-Allow-Credentials") != "" ||
			!varies(h, "Origin") || !varies(h, "Access-Control-Request-Method") || !varies(h, "Access-Control-Request-Headers") {
			t.Errorf("%s: preflight headers %v", path, h)
		}
		if h.Get(server.RequestIDHeader) == "" {
			t.Errorf("%s: no request id on the preflight", path)
		}
	}
	if n := count(); n != before {
		t.Errorf("preflights wrote %d audit entries", n-before)
	}
	if log := e.log.String(); contains(log, instance) || !contains(log, `"path":"/tenants/..."`) {
		t.Errorf("the preflight to a tenant's path is logged with its ids, or not at all:\n%s", log)
	}

	for _, origin := range []string{"https://evil.example", "https://viewer.example.org:443", "https://VIEWER.example.org", "null", "http://viewer.example.org"} {
		status, h := corsRequest(t, e, "OPTIONS", nopacsv1connect.AccessServiceWhoAmIProcedure, origin,
			"Access-Control-Request-Method", "POST", "Access-Control-Request-Headers", "authorization")
		if got := corsHeaders(h); len(got) != 0 || status != http.StatusUnauthorized || !varies(h, "Origin") {
			t.Errorf("%s: an unlisted preflight answered %d with %v", origin, status, got)
		}
	}
}

// TestCORSActualRequests guards the CORS headers of a request itself: a
// listed origin gets its origin and the exposed headers on a success and on
// a refusal (so its page can read why it was refused and the request id),
// never credentials; an origin not listed, two Origin headers and no Origin
// get no CORS header; and every answer varies on Origin.
func TestCORSActualRequests(t *testing.T) {
	e := newEnv(t, options{corsOrigins: originsOf(t, listedOrigin)})
	key := "Bearer " + e.makeKey(nopacsv1.Role_ROLE_UNSPECIFIED, nil)
	whoami := nopacsv1connect.AccessServiceWhoAmIProcedure
	for _, c := range []struct {
		name, origin string
		headers      []string
		status       int
		allowed      bool
	}{
		{"listed, allowed", listedOrigin, []string{"Content-Type", "application/json", "Authorization", key}, http.StatusOK, true},
		{"listed, no key", listedOrigin, []string{"Content-Type", "application/json"}, http.StatusUnauthorized, true},
		{"unlisted, allowed", "https://evil.example", []string{"Content-Type", "application/json", "Authorization", key}, http.StatusOK, false},
		{"unlisted, no key", "https://evil.example", []string{"Content-Type", "application/json"}, http.StatusUnauthorized, false},
		{"two origins", listedOrigin, []string{"Content-Type", "application/json", "Authorization", key, "Origin", "https://evil.example"}, http.StatusOK, false},
		{"no origin", "", []string{"Content-Type", "application/json", "Authorization", key}, http.StatusOK, false},
	} {
		status, h := corsRequest(t, e, "POST", whoami, c.origin, c.headers...)
		if status != c.status || !varies(h, "Origin") || h.Get("Access-Control-Allow-Credentials") != "" {
			t.Errorf("%s: %d, %v", c.name, status, h)
		}
		if !c.allowed {
			if got := corsHeaders(h); len(got) != 0 {
				t.Errorf("%s: CORS headers %v", c.name, got)
			}
			continue
		}
		if h.Get("Access-Control-Allow-Origin") != listedOrigin || !hasToken(h.Get("Access-Control-Expose-Headers"), server.RequestIDHeader) {
			t.Errorf("%s: %v", c.name, h)
		}
	}
	// A pattern route's refusal carries them too.
	if status, h := corsRequest(t, e, "GET", "/tenants/lab/instances/S01/E01/T1/x", listedOrigin); status != http.StatusUnauthorized || h.Get("Access-Control-Allow-Origin") != listedOrigin {
		t.Errorf("a download without a key from a listed origin: %d, %v", status, h)
	}
}

// TestCORSWildcardAndClosed guards "*": any origin is answered "*", in a
// preflight and a request, never with credentials; and a server whose
// setting cannot be read, or that is given none, answers no origin.
func TestCORSWildcardAndClosed(t *testing.T) {
	e := newEnv(t, options{corsOrigins: originsOf(t, "*")})
	status, h := corsRequest(t, e, "OPTIONS", nopacsv1connect.AccessServiceWhoAmIProcedure, "https://any.example", "Access-Control-Request-Method", "POST")
	if status != http.StatusNoContent || h.Get("Access-Control-Allow-Origin") != "*" || h.Get("Access-Control-Allow-Credentials") != "" || !varies(h, "Origin") {
		t.Errorf("a preflight under *: %d, %v", status, h)
	}
	status, h = corsRequest(t, e, "POST", nopacsv1connect.AccessServiceWhoAmIProcedure, "https://any.example", "Content-Type", "application/json")
	if status != http.StatusUnauthorized || h.Get("Access-Control-Allow-Origin") != "*" || h.Get("Access-Control-Allow-Credentials") != "" {
		t.Errorf("a request under *: %d, %v", status, h)
	}

	failing := newEnv(t, options{corsOrigins: func(context.Context) (settings.Origins, error) {
		return settings.Origins{}, errors.New("the master does not answer")
	}})
	none := newEnv(t, options{})
	for name, e := range map[string]*env{"a setting that cannot be read": failing, "no setting": none} {
		status, h := corsRequest(t, e, "OPTIONS", nopacsv1connect.AccessServiceWhoAmIProcedure, listedOrigin, "Access-Control-Request-Method", "POST")
		if got := corsHeaders(h); len(got) != 0 || status != http.StatusUnauthorized || !varies(h, "Origin") {
			t.Errorf("%s: a preflight answered %d with %v", name, status, got)
		}
	}
}
