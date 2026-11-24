package server_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect/v2"

	nopacsv1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/access"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/server"
)

// The stand-in routes of the tests: a download of the contract's shape and
// a subtree, each needing reader on the tenant its path names.
const (
	downloadPattern = "GET /tenants/{tenant}/instances/{subject}/{session}/{scan}/{instance}"
	subtreePattern  = "GET /tenants/{tenant}/dicom-web/{rest...}"
)

// readerOnPathTenant is the stand-in requirement: reader on the tenant the
// path names, or on the installation when the server left the name empty;
// the subject as the audit object.
func readerOnPathTenant(r *http.Request) (access.Requirement, access.Request) {
	facts := access.Request{Object: r.PathValue("subject")}
	if t := r.PathValue("tenant"); t != "" {
		return access.OnTenant(t, master.RoleReader), facts
	}
	return access.OnInstallation(master.RoleReader), facts
}

// testPatterns are the pattern routes every test server has unless a test
// gives its own: both stand-ins, answered by server.NotImplemented.
func testPatterns() []server.PatternRoute {
	return []server.PatternRoute{
		{Pattern: downloadPattern, Need: readerOnPathTenant, Handler: server.NotImplemented},
		{Pattern: subtreePattern, Need: readerOnPathTenant, Handler: server.NotImplemented},
	}
}

// countedPatterns are testPatterns with handlers that count their runs and
// write the path values they were given before answering 501.
func countedPatterns(ran *atomic.Int64) []server.PatternRoute {
	h := func(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
		ran.Add(1)
		w.Header().Set("X-Values", r.PathValue("tenant")+"|"+r.PathValue("subject")+"|"+r.PathValue("instance")+"|"+r.PathValue("rest"))
		return server.NotImplemented(ctx, w, r)
	}
	return []server.PatternRoute{
		{Pattern: downloadPattern, Need: readerOnPathTenant, Handler: h},
		{Pattern: subtreePattern, Need: readerOnPathTenant, Handler: h},
	}
}

// makeTenants adds ready tenants of these names to the master.
func (e *env) makeTenants(names ...string) {
	e.t.Helper()
	for _, name := range names {
		if err := e.master.CreateTenant(e.ctx, master.Tenant{ID: "id-" + name, Name: name, Driver: "sqlite", SealedAddress: []byte{1}, State: master.StateReady, Created: time.Now().UTC().Truncate(time.Microsecond)},
			master.AuditEntry{ID: server.NewID(), Time: time.Now(), Method: "test", Outcome: master.OutcomeDone}, master.LocalOperator()); err != nil {
			e.t.Fatal(err)
		}
	}
}

// rawGet sends method to the path exactly as written, never cleaned or
// re-encoded by the client, with authorization when it is not empty, and
// returns the status, the body and the headers.
func rawGet(t *testing.T, c *http.Client, base, method, rawPath, authorization string) (int, string, http.Header) {
	t.Helper()
	req, err := http.NewRequest(method, base+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.URL.Opaque = rawPath
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode, string(body), resp.Header
}

// TestPatternRouteRefusedLikeEveryRoute guards the check of a route whose
// tenant comes from its path: no key and a wrong key are refused with the
// same status and body as any other route's (/metrics here), a key with no
// grant and a key whose grant is on another tenant with the same 403 as a
// plain route's denial; none of them reaches the handler; a reader of the
// tenant the path names reaches it (501 from the stand-in, also for HEAD),
// with the path values it was given; and every one of these requests
// writes its one audit entry under the route's pattern, never its path.
func TestPatternRouteRefusedLikeEveryRoute(t *testing.T) {
	var ran atomic.Int64
	e := newEnv(t, options{patterns: countedPatterns(&ran)})
	e.makeTenants("lab", "clinic")
	noGrant := "Bearer " + e.makeKey(nopacsv1.Role_ROLE_UNSPECIFIED, nil)
	labReader := "Bearer " + e.makeKey(nopacsv1.Role_ROLE_READER, &nopacsv1.Scope{Kind: &nopacsv1.Scope_Tenant{Tenant: "lab"}})
	clinicReader := "Bearer " + e.makeKey(nopacsv1.Role_ROLE_READER, &nopacsv1.Scope{Kind: &nopacsv1.Scope_Tenant{Tenant: "clinic"}})
	const instance = "1.2.826.0.1.3680043.8.498.77"
	path := "/tenants/lab/instances/S01/E01/T1/" + instance
	hc := e.tlsClient(false)

	for _, c := range []struct {
		name, auth string
		want       int
	}{
		{"no key", "", http.StatusUnauthorized},
		{"a wrong key", "Bearer " + unknownKey, http.StatusUnauthorized},
		{"a malformed key", "Bearer nope", http.StatusUnauthorized},
		{"a key with no grant", noGrant, http.StatusForbidden},
	} {
		status, body, h := rawGet(t, hc, e.netURL, "GET", path, c.auth)
		mStatus, mBody, _ := rawGet(t, hc, e.netURL, "GET", "/metrics", c.auth)
		if status != c.want || status != mStatus || body != mBody {
			t.Errorf("%s: %d %s; /metrics answers %d %s", c.name, status, body, mStatus, mBody)
		}
		if h.Get(server.RequestIDHeader) == "" {
			t.Errorf("%s: no request id", c.name)
		}
	}
	// A grant on another tenant, and a tenant that does not exist, are
	// refused as a key with no grant is.
	_, denied, _ := rawGet(t, hc, e.netURL, "GET", "/metrics", noGrant)
	for _, p := range []string{path, "/tenants/nosuch/instances/S01/E01/T1/" + instance} {
		if status, body, _ := rawGet(t, hc, e.netURL, "GET", p, clinicReader); status != http.StatusForbidden || body != denied {
			t.Errorf("a reader of clinic asking %s: %d %s", p, status, body)
		}
	}
	if n := ran.Load(); n != 0 {
		t.Fatalf("a refused request ran the handler %d times", n)
	}

	status, body, h := rawGet(t, hc, e.netURL, "GET", path, labReader)
	if status != http.StatusNotImplemented || !strings.Contains(body, `"unimplemented"`) || ran.Load() != 1 {
		t.Fatalf("a reader of lab: %d %s, handler ran %d", status, body, ran.Load())
	}
	if v := h.Get("X-Values"); v != "lab|S01|"+instance+"|" {
		t.Errorf("the handler was given %q", v)
	}
	if status, _, _ := rawGet(t, hc, e.netURL, "HEAD", path, labReader); status != http.StatusNotImplemented || ran.Load() != 2 {
		t.Errorf("HEAD from a reader of lab: %d", status)
	}

	if err := e.access.FlushAudit(e.ctx); err != nil {
		t.Fatal(err)
	}
	entries, _, err := e.master.ListAudit(e.ctx, master.AuditFilter{}, master.Page{Limit: master.MaxPageLimit})
	if err != nil {
		t.Fatal(err)
	}
	var refused, denials, allowed int
	for _, en := range entries {
		if strings.Contains(en.Method, instance) || strings.Contains(en.Object, instance) {
			t.Errorf("an entry holds the path's instance id: %+v", en)
		}
		if en.Method != downloadPattern {
			continue
		}
		switch {
		case en.Outcome == master.OutcomeRefused && en.Object == "S01":
			denials++
		case en.Outcome == master.OutcomeRefused:
			refused++
		case en.Outcome == master.OutcomeFailed && en.Object == "S01":
			allowed++
		}
	}
	// Three unauthenticated, one with no grant, two of clinic's reader;
	// two allowed (GET and HEAD), each failed as not implemented.
	if refused != 3 || denials != 3 || allowed != 2 {
		t.Errorf("entries under the pattern: %d refused before the check, %d denied, %d allowed; want 3, 3, 2", refused, denials, allowed)
	}
}

// TestPatternRouteHostilePaths guards a path a stranger shaped: a path
// with percent-encoding (an encoded slash, NUL or dots), a dot segment, an
// empty segment, a trailing slash or an over-long segment never reaches the
// handler and is never redirected. Without a key it is refused as any
// request is; from a key without a grant on the tenant it names it is
// refused before the rest of the path is read; from a reader of that
// tenant it is answered 404. A tenant's name that is not one is held to
// the role on the installation, which a reader of one tenant lacks.
func TestPatternRouteHostilePaths(t *testing.T) {
	var ran atomic.Int64
	e := newEnv(t, options{patterns: countedPatterns(&ran)})
	e.makeTenants("lab", "clinic")
	labReader := "Bearer " + e.makeKey(nopacsv1.Role_ROLE_READER, &nopacsv1.Scope{Kind: &nopacsv1.Scope_Tenant{Tenant: "lab"}})
	clinicReader := "Bearer " + e.makeKey(nopacsv1.Role_ROLE_READER, &nopacsv1.Scope{Kind: &nopacsv1.Scope_Tenant{Tenant: "clinic"}})
	hc := e.tlsClient(false)
	long := strings.Repeat("a", 257)
	for _, c := range []struct {
		path string
		// shaped is a path of a route's shape, whose values are what is
		// wrong: checked before it is read. Any other path is one nothing
		// serves, 404 to any authenticated caller.
		shaped bool
	}{
		{"/tenants/lab/instances/S01/E01/T1/..", true},
		{"/tenants/lab/instances/S01/E01/./x", true},
		{"/tenants/lab/instances/S01/E01/%2E%2E/x", true},
		{"/tenants/lab/instances/S01%2FE01/E01/T1/x", true},
		{"/tenants/lab/instances/S01/E01/T1/a%00b", true},
		{"/tenants/lab/instances/S01/E01/T1/a%2fb", true},
		{"/tenants/lab/instances/S01/E01/T1/" + long, true},
		{"/tenants/lab/instances//E01/T1/x", true},
		{"/tenants/lab/dicom-web/studies/../x", true},
		{"/tenants/lab/dicom-web/studies//x", true},
		{"/tenants/lab/dicom-web/studies%2Fx", true},
		{"/tenants/lab/dicom-web/", true},
		{"/tenants/lab/instances/S01//T1/x", true},
		{"/tenants/lab/instances/S01/E01/T1/x/", false},
	} {
		if status, _, _ := rawGet(t, hc, e.netURL, "GET", c.path, ""); status != http.StatusUnauthorized {
			t.Errorf("%s without a key: %d", c.path, status)
		}
		status, body, h := rawGet(t, hc, e.netURL, "GET", c.path, labReader)
		if status != http.StatusNotFound || h.Get("Location") != "" {
			t.Errorf("%s from a reader of lab: %d %s", c.path, status, body)
		}
		want := http.StatusNotFound
		if c.shaped {
			want = http.StatusForbidden
		}
		if status, _, _ := rawGet(t, hc, e.netURL, "GET", c.path, clinicReader); status != want {
			t.Errorf("%s from a reader of clinic: %d, want %d", c.path, status, want)
		}
	}
	for _, p := range []string{
		"/tenants/Lab/instances/S01/E01/T1/x",
		"/tenants/l%61b/instances/S01/E01/T1/x",
		"/tenants/lab%2F..%2Fclinic/instances/S01/E01/T1/x",
		"/tenants/../instances/S01/E01/T1/x",
	} {
		if status, _, _ := rawGet(t, hc, e.netURL, "GET", p, labReader); status != http.StatusForbidden {
			t.Errorf("%s from a reader of lab: %d, want 403 (the role on the installation)", p, status)
		}
		local := e.socketClient(false)
		if status, _, _ := rawGet(t, local, "http://local", "GET", p, ""); status != http.StatusNotFound {
			t.Errorf("%s from the local operator: %d, want 404", p, status)
		}
	}
	if n := ran.Load(); n != 0 {
		t.Fatalf("a hostile path reached the handler %d times", n)
	}
	// The subtree serves a path of plain segments, given whole.
	status, _, h := rawGet(t, hc, e.netURL, "GET", "/tenants/lab/dicom-web/studies/1.2.3/series", labReader)
	if status != http.StatusNotImplemented || h.Get("X-Values") != "lab|||studies/1.2.3/series" {
		t.Errorf("the subtree: %d %q", status, h.Get("X-Values"))
	}
	// Another method on a route's path is a path nothing serves.
	if status, body, _ := rawGet(t, hc, e.netURL, "POST", "/tenants/lab/instances/S01/E01/T1/x", labReader); status != http.StatusNotFound || !strings.Contains(body, "no such route") {
		t.Errorf("POST to a GET route: %d %s", status, body)
	}
}

// noChecker is a Checker that allows nothing it is asked about, for a
// server only built, never started.
type noChecker struct{}

// Authenticate accepts nothing it is given.
func (noChecker) Authenticate(context.Context, access.Credential, access.Request) (access.Credential, error) {
	return access.Credential{}, nil
}

// Check allows nothing.
func (noChecker) Check(context.Context, access.Credential, access.Requirement, access.Request) (access.Decision, error) {
	return access.Decision{}, nil
}

// Record records nothing.
func (noChecker) Record(access.Decision, master.Outcome, string, access.Facts) {}

// RecordUnchecked records nothing.
func (noChecker) RecordUnchecked(access.Credential, access.Request, string) {}

// RecordRefusal records nothing and answers unauthenticated.
func (noChecker) RecordRefusal(access.Request, string, string, bool, time.Time) error {
	return fault.New(fault.Unauthenticated, access.MessageUnauthenticated)
}

// AuditStats counts nothing.
func (noChecker) AuditStats() access.AuditStats { return access.AuditStats{} }

// MarkStats counts nothing.
func (noChecker) MarkStats() access.MarkStats { return access.MarkStats{} }

// Close has nothing to close.
func (noChecker) Close(context.Context) error { return nil }

// TestNewRefusesBadPatternRoutes guards the table of pattern routes: a
// pattern without a method, without a {tenant}, starting with a wildcard,
// with a {name...} that is not last or a malformed wildcard, one taking a
// procedure's or a plain route's path, one overlapping another route
// (but for the same paths under another method), and one without a
// handler or a requirement, each stop New.
func TestNewRefusesBadPatternRoutes(t *testing.T) {
	build := func(patterns ...string) error {
		var routes []server.PatternRoute
		for _, p := range patterns {
			routes = append(routes, server.PatternRoute{Pattern: p, Need: readerOnPathTenant, Handler: server.NotImplemented})
		}
		_, err := server.New(server.Config{NoSocket: true, Access: noChecker{}, Register: func(*connect.Server) {}, PatternRoutes: routes})
		return err
	}
	for _, ok := range [][]string{
		{downloadPattern, subtreePattern},
		{"GET /tenants/{tenant}/x", "POST /tenants/{tenant}/x"},
		{"GET /tenants/{tenant}/a/{x}", "GET /tenants/{tenant}/b/{x}"},
	} {
		if err := build(ok...); err != nil {
			t.Errorf("%v refused: %v", ok, err)
		}
	}
	for _, bad := range [][]string{
		{"/tenants/{tenant}/x"},
		{"get /tenants/{tenant}/x"},
		{"GET /tenants/{name}/x"},
		{"GET /{tenant}/x"},
		{"GET /tenants/{tenant}/x/"},
		{"GET /tenants/{tenant}/{rest...}/x"},
		{"GET /tenants/{tenant...}"},
		{"GET /tenants/{tenant}/{Bad}"},
		{"GET /tenants/{tenant}/{x}/{x}"},
		{"GET /tenants/{tenant}/%2e"},
		{"GET /tenants/{tenant}/.."},
		{"GET /tenants/{tenant}/{x"},
		{"POST /grpc.health.v1.Health/{tenant}"},
		{"GET /tenants/{tenant}/x/{y}", "GET /tenants/{tenant}/{z}/y"},
		{"GET /tenants/{tenant}/x", "GET /tenants/{tenant}/{y}"},
		{"GET /tenants/{tenant}/x", "HEAD /tenants/{tenant}/x"},
		{"GET /tenants/{tenant}/x", "GET /tenants/{tenant}/x"},
		{"GET /tenants/{tenant}/a/b", "GET /tenants/{tenant}/{rest...}"},
	} {
		if err := build(bad...); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
	plain := []server.HTTPRoute{{Pattern: "GET /tenants/lab", Need: access.OnInstallation(master.RoleReader), Handler: server.NotImplemented}}
	if _, err := server.New(server.Config{NoSocket: true, Access: noChecker{}, Register: func(*connect.Server) {}, HTTPRoutes: plain,
		PatternRoutes: []server.PatternRoute{{Pattern: "GET /tenants/{tenant}", Need: readerOnPathTenant, Handler: server.NotImplemented}}}); err == nil {
		t.Error("a pattern route taking a plain route's path accepted")
	}
	for _, r := range []server.PatternRoute{
		{Pattern: downloadPattern, Need: readerOnPathTenant},
		{Pattern: downloadPattern, Handler: server.NotImplemented},
	} {
		if _, err := server.New(server.Config{NoSocket: true, Access: noChecker{}, Register: func(*connect.Server) {}, PatternRoutes: []server.PatternRoute{r}}); err == nil {
			t.Errorf("a route without its handler or requirement accepted")
		}
	}
}
