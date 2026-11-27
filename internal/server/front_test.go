package server_test

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	nopacsv1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/gen/nopacs/v1/nopacsv1connect"
	"github.com/amrshadid/nopacs/internal/access"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/secret"
	"github.com/amrshadid/nopacs/internal/server"
	"github.com/amrshadid/nopacs/internal/testkit/servertest"
)

// notServedMethod returns the method the first line of api/not-served.txt
// names: the first method of a service it names whole, or the one method
// it names of a service otherwise served, read from the generated
// descriptors, so the matrix asks for a method of the contract that no
// handler serves whichever it is; when the list is empty, a method of a
// service the contract lacks.
func notServedMethod(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "api", "not-served.txt"))
	if err != nil {
		t.Fatal(err)
	}
	for line := range strings.SplitSeq(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		svc, method, isMethod := strings.Cut(line, "/")
		d, err := protoregistry.GlobalFiles.FindDescriptorByName(protoreflect.FullName(svc))
		sd, ok := d.(protoreflect.ServiceDescriptor)
		if err != nil || !ok || sd.Methods().Len() == 0 {
			t.Fatalf("%s is not a service with methods: %v", svc, err)
		}
		if isMethod {
			if sd.Methods().ByName(protoreflect.Name(method)) == nil {
				t.Fatalf("%s has no method %s", svc, method)
			}
			return "/" + line
		}
		return "/" + line + "/" + string(sd.Methods().Get(0).Name())
	}
	// Every service of the contract is served: use a method of a service the
	// contract does not have, which the server answers as any method it does
	// not serve, so the matrices that need one still run.
	return "/nopacs.v1.NotServedService/Nothing"
}

// badCredential is one way of not being authenticated: the Authorization
// header values a request carries (none, one, or two).
type badCredential struct {
	name   string
	values []string
	// reason is the real cause its refusal's audit entry records.
	reason string
}

// badCredentials makes, on e, every kind of credential that is no good: no
// key, a malformed key, a key of an id nobody holds, a real key with a
// wrong secret, an expired key, a revoked key, another scheme, and two
// Authorization headers each holding a good key.
func badCredentials(t *testing.T, e *env) []badCredential {
	t.Helper()
	good := e.makeKey(nopacsv1.Role_ROLE_ADMIN, installation())
	last := "a"
	if strings.HasSuffix(good, "a") {
		last = "b"
	}
	wrong := good[:len(good)-1] + last
	now := time.Now().UTC()
	expired, err := secret.MakeAPIKey()
	if err != nil {
		t.Fatal(err)
	}
	entry := func() master.AuditEntry {
		return master.AuditEntry{ID: server.NewID(), Time: now, Method: "test", Outcome: master.OutcomeDone}
	}
	if err := e.master.CreateKey(e.ctx, master.Key{ID: expired.ID, Hash: expired.Hash[:], Holder: "test", Created: now.Add(-2 * time.Hour), Expires: now.Add(-time.Hour)}, entry(), master.LocalOperator()); err != nil {
		t.Fatal(err)
	}
	revoked, err := secret.MakeAPIKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := e.master.CreateKey(e.ctx, master.Key{ID: revoked.ID, Hash: revoked.Hash[:], Holder: "test", Created: now.Add(-time.Hour)}, entry(), master.LocalOperator()); err != nil {
		t.Fatal(err)
	}
	if _, err := e.master.RevokeKey(e.ctx, revoked.ID, now, entry(), master.LocalOperator()); err != nil {
		t.Fatal(err)
	}
	stranger, err := secret.MakeAPIKey() // a key of the right form that nobody holds
	if err != nil {
		t.Fatal(err)
	}
	return []badCredential{
		{"no key", nil, "no key"},
		{"malformed key", []string{"Bearer not-a-key"}, "malformed key"},
		{"unknown key id", []string{"Bearer " + stranger.Key()}, "unknown key"},
		{"wrong secret", []string{"Bearer " + wrong}, "wrong secret"},
		{"expired key", []string{"Bearer " + expired.Key()}, "key expired"},
		{"revoked key", []string{"Bearer " + revoked.Key()}, "key revoked"},
		{"another scheme", []string{"Basic dXNlcjpwYXNz"}, "malformed key"},
		{"two headers", []string{"Bearer " + good, "Bearer " + good}, "malformed key"},
	}
}

// contentCase is one Content-Type a request may carry, the body of an
// empty request in it, and the class of answer the server must give it:
// the protocol family and, for an enveloped protocol, the codec named in
// the answer's Content-Type.
type contentCase struct {
	name, contentType string
	body              []byte
	class             string
}

// contentCases are each RPC content type the server speaks, a streaming
// one, unsupported ones of an RPC family and none, text/plain, and none.
var contentCases = []contentCase{
	{"grpc", "application/grpc", make([]byte, 5), "grpc"},
	{"grpc+proto", "application/grpc+proto", make([]byte, 5), "grpc"},
	{"grpc+json", "application/grpc+json", []byte{0, 0, 0, 0, 2, '{', '}'}, "grpc+json"},
	{"grpc+unsupported", "application/grpc+xml", make([]byte, 5), "grpc"},
	{"grpc-web", "application/grpc-web+proto", make([]byte, 5), "grpc-web"},
	{"grpc-web+json", "application/grpc-web+json", []byte{0, 0, 0, 0, 2, '{', '}'}, "grpc-web+json"},
	{"connect streaming", "application/connect+proto", make([]byte, 5), "connect-stream"},
	{"connect streaming json", "application/connect+json; charset=utf-8", []byte{0, 0, 0, 0, 2, '{', '}'}, "connect-stream+json"},
	{"connect json", "application/json", []byte("{}"), "json"},
	{"connect proto", "application/proto", nil, "json"},
	{"grpc-web-text", "application/grpc-web-text", []byte("AAAAAAA="), "json"},
	{"unsupported", "application/xml", []byte("<x/>"), "json"},
	{"text", "text/plain", []byte("x"), "json"},
	{"none", "", nil, "json"},
}

// matrixPath is one path of the matrix, and the name its refused request
// must be audited under.
type matrixPath struct {
	name, path, audited string
}

// matrixPaths are the paths of the matrix: a served method whose
// requirement is fixed and one whose requirement reads the request, a
// method of a service the contract has and nothing serves, an unknown
// method of a served service, a name in no contract, a percent-encoded path
// to a served method, "/", the metrics, a path of 5000 bytes, a path with a
// control character, a double slash, and a trailing slash.
func matrixPaths(t *testing.T) []matrixPath {
	ns := notServedMethod(t)
	long := "/acme.v9.Nothing/" + strings.Repeat("x", 5000)
	return []matrixPath{
		{"served", servedProcedure, servedProcedure},
		{"served, reads the request", nopacsv1connect.SettingsServiceGetSettingProcedure, nopacsv1connect.SettingsServiceGetSettingProcedure},
		{"not served", ns, ns},
		{"unknown method", "/nopacs.v1.AccessService/NoSuchMethod", "/nopacs.v1.AccessService/NoSuchMethod"},
		{"no contract", "/acme.v9.Nothing/Do", "/acme.v9.Nothing/Do"},
		{"percent-encoded", "/nopacs.v1.AccessService%2FListKeys", "/"},
		{"root", "/", "/"},
		{"metrics", "/metrics", server.MetricsPattern},
		{"long", long, "/"},
		{"control character", "/acme.v9.Not%01thing/Do", "/"},
		{"double slash", "/" + servedProcedure, "/"},
		{"trailing slash", servedProcedure + "/", "/"},
		{"percent-encoded health check", "/grpc.health.v1.Health%2FCheck", "/"},
		{"health check, upper case", "/GRPC.HEALTH.V1.HEALTH/CHECK", "/GRPC.HEALTH.V1.HEALTH/CHECK"},
		{"health check, method's case", "/grpc.health.v1.Health/check", "/grpc.health.v1.Health/check"},
		{"health, another method", "/grpc.health.v1.Health/Nope", "/grpc.health.v1.Health/Nope"},
		{"health check, trailing slash", "/grpc.health.v1.Health/Check/", "/"},
		{"health check, a prefix", "/grpc.health.v1.Health/Chec", "/grpc.health.v1.Health/Chec"},
		{"health check, a suffix", "/grpc.health.v1.Health/CheckX", "/grpc.health.v1.Health/CheckX"},
		{"health check, inside", "/x/grpc.health.v1.Health/Check", "/"},
		{"health service", "/grpc.health.v1.Health/", "/"},
	}
}

// transport is one way of reaching the server.
type transport struct {
	name   string
	client *http.Client
	base   string
	// socket is set for the local socket, where a request without a key is
	// the local operator and so is not part of the matrix.
	socket bool
}

// matrixCell is one request of the matrix and what came back.
type matrixCell struct {
	transport, cred, path, method string
	content                       contentCase
	audited, reason               string
	got                           answer
}

// sendMatrix sends every request of the matrix, eight at a time.
func sendMatrix(t *testing.T, transports []transport, creds []badCredential, paths []matrixPath, methods []string) []*matrixCell {
	t.Helper()
	type job struct {
		cell *matrixCell
		tr   transport
		vals []string
	}
	var jobs []job
	var cells []*matrixCell
	for _, tr := range transports {
		for _, c := range creds {
			if tr.socket && len(c.values) == 0 {
				continue
			}
			for _, p := range paths {
				for _, m := range methods {
					for _, ct := range contentCases {
						cell := &matrixCell{transport: tr.name, cred: c.name, path: p.name, method: m, content: ct, audited: p.audited, reason: c.reason}
						cells = append(cells, cell)
						jobs = append(jobs, job{cell, tr, c.values})
					}
				}
			}
		}
	}
	work := make(chan job)
	var wg sync.WaitGroup
	var failed atomic.Value
	for range 8 {
		wg.Go(func() {
			for j := range work {
				a, err := rawCall(j.tr.client, j.cell.method, j.tr.base+pathOf(paths, j.cell.path), j.cell.content, j.vals)
				if err != nil {
					failed.Store(fmt.Sprintf("%s %s %s %s: %v", j.tr.name, j.cell.method, j.cell.path, j.cell.content.name, err))
					continue
				}
				j.cell.got = a
			}
		})
	}
	for _, j := range jobs {
		work <- j
	}
	close(work)
	wg.Wait()
	if f := failed.Load(); f != nil {
		t.Fatal(f)
	}
	return cells
}

// requestIDShape is the form of a request id: 16 characters of lowercase
// base32.
var requestIDShape = regexp.MustCompile(`^[a-z2-7]{16}$`)

// pathOf returns the path of the matrix path named name.
func pathOf(paths []matrixPath, name string) string {
	for _, p := range paths {
		if p.name == name {
			return p.path
		}
	}
	return ""
}

// rawCall sends one request and returns all of its answer but the date and
// the request id.
func rawCall(hc *http.Client, method, url string, ct contentCase, authorization []string) (answer, error) {
	req, err := http.NewRequest(method, url, bytes.NewReader(ct.body))
	if err != nil {
		return answer{}, err
	}
	if ct.contentType != "" {
		req.Header.Set("Content-Type", ct.contentType)
	}
	if strings.HasPrefix(ct.contentType, "application/grpc") && !strings.HasPrefix(ct.contentType, "application/grpc-web") {
		req.Header.Set("Te", "trailers")
	}
	for _, v := range authorization {
		req.Header.Add("Authorization", v)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return answer{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return answer{}, err
	}
	return answer{
		status: resp.StatusCode, header: headerText(resp.Header), trailer: headerText(resp.Trailer),
		body: body, id: resp.Header.Get(server.RequestIDHeader), protocol: ct.name,
	}, nil
}

// TestUnauthenticatedAnswerIsTheProtocols guards that a caller who is not
// authenticated learns nothing from the server but that it is not: across a
// generated matrix of every kind of bad credential, paths served, not
// served, malformed and hostile, every HTTP method, every content type and
// HTTP/1.1, HTTP/2 over TLS and h2c on the local socket, the whole answer
// (status, headers as a set, trailers, body) of each request depends only on
// the protocol family its Content-Type names (and, for an enveloped
// protocol, the codec named in the answer's Content-Type), the HTTP version,
// and whether it is a HEAD request, which has no body (RFC 9110 9.3.2). The
// same requests write exactly one audit entry each, refused, under the name
// the path is safely recorded by. The health service, the one public path,
// answers without a key and writes none.
func TestUnauthenticatedAnswerIsTheProtocols(t *testing.T) {
	// Stop writes the tens of thousands of refusals queued here and then
	// closes the master; on a loaded machine that passes the default close
	// deadline, which is not what this test is about (the deadline itself is
	// held by TestStopAbandonsWhatDoesNotClose).
	// The limit on failed calls is out of reach: the matrix sends tens of
	// thousands of refusals from one address, which would be slowed and then
	// blocked, and each answer is held to be the protocol's alone.
	e := newEnv(t, options{refusalQueue: 1 << 17, closeT: time.Minute,
		accessRules: rulesOf(func(r *server.AccessRules) { r.FailureLimit = 1 << 30 })})
	creds := badCredentials(t, e)
	paths := matrixPaths(t)
	methods := []string{http.MethodPost, http.MethodGet, http.MethodPut, http.MethodDelete, http.MethodOptions, http.MethodHead}
	transports := []transport{
		{"HTTP/1.1", e.tlsClient(false), e.netURL, false},
		{"HTTP/2", e.tlsClient(true), e.netURL, false},
		{"h2c socket", e.socketClient(true), "http://local", true},
	}
	cells := sendMatrix(t, transports, creds, paths, methods)

	groups := map[string]map[string][]string{}
	for _, c := range cells {
		// The request id is left out of the comparison below, since it
		// differs per request by design; here it must be in the answer's
		// first header block, well formed, and (further down) the id of the
		// request's one audit entry. In gRPC the status must not share that
		// block: a client that reads a header block holding grpc-status as
		// Trailers-Only (grpcio does) shows none of it as initial metadata,
		// and the caller would not see its request id there.
		if !requestIDShape.MatchString(c.got.id) {
			t.Fatalf("%s %s %s %s %s: request id %q", c.transport, c.cred, c.method, c.path, c.content.name, c.got.id)
		}
		if strings.HasPrefix(c.content.class, "grpc") && strings.Contains(c.got.header, "Grpc-Status:") {
			t.Fatalf("%s %s %s %s %s: grpc-status is in the header block with the request id:\n%s", c.transport, c.cred, c.method, c.path, c.content.name, c.got)
		}
		head := c.method == http.MethodHead
		key := fmt.Sprintf("%s, %s, HEAD %v", strings.TrimSuffix(c.transport, " socket"), c.content.class, head)
		if c.transport != "HTTP/1.1" {
			key = fmt.Sprintf("HTTP/2, %s, HEAD %v", c.content.class, head)
		}
		if groups[key] == nil {
			groups[key] = map[string][]string{}
		}
		label := fmt.Sprintf("%s %s %s %s %s", c.transport, c.cred, c.method, c.path, c.content.name)
		groups[key][c.got.String()] = append(groups[key][c.got.String()], label)
	}
	for key, answers := range groups {
		if len(answers) != 1 {
			var b strings.Builder
			for a, labels := range answers {
				fmt.Fprintf(&b, "--- %d requests, such as %s:\n%s", len(labels), labels[0], a)
			}
			t.Errorf("%s: %d different answers\n%s", key, len(answers), b.String())
		}
	}
	// Each group's one answer is the refusal of its protocol, in the
	// content type its class names.
	contentTypes := map[string]string{
		"grpc": "application/grpc", "grpc+json": "application/grpc+json",
		"grpc-web": "application/grpc-web+proto", "grpc-web+json": "application/grpc-web+json",
		"connect-stream": "application/connect+proto", "connect-stream+json": "application/connect+json",
		"json": "application/json",
	}
	for key, answers := range groups {
		class := strings.Split(key, ", ")[1]
		for a := range answers {
			if !strings.Contains(a, "Www-Authenticate: Bearer") || (!strings.Contains(a, "the request carries no valid key") && !strings.Contains(key, "HEAD true")) ||
				!strings.Contains(a, "Content-Type: "+contentTypes[class]+"\n") {
				t.Errorf("%s: not the refusal of its protocol:\n%s", key, a)
			}
		}
	}
	if len(groups) != 2*7*2 {
		t.Errorf("%d groups of answers, want %d: the matrix lost a dimension", len(groups), 2*7*2)
	}

	health := 0
	for _, tr := range transports[:2] {
		for _, m := range methods {
			a, err := rawCall(tr.client, m, tr.base+server.HealthCheckProcedure, contentCases[0], nil)
			if err != nil {
				t.Fatal(err)
			}
			if m == http.MethodPost && (a.status != http.StatusOK || strings.Contains(a.header+a.trailer, "Grpc-Status: 16")) {
				t.Errorf("%s: the health check was refused without a key: %s", tr.name, a)
			}
			cells = append(cells, &matrixCell{transport: tr.name, path: "health", method: m, got: a})
			health++
		}
	}

	var ids []string
	for _, c := range cells {
		if c.path != "health" {
			ids = append(ids, c.got.id)
		}
	}
	entries := allAudit(t, e, haveIDs(ids...))
	byID := map[string][]master.AuditEntry{}
	for _, en := range entries {
		byID[en.RequestID] = append(byID[en.RequestID], en)
	}
	for _, c := range cells {
		got := byID[c.got.id]
		if c.path == "health" {
			if len(got) != 0 {
				t.Errorf("%s %s health check: %d audit entries, want none", c.transport, c.method, len(got))
			}
			continue
		}
		if len(got) != 1 {
			t.Errorf("%s %s %s %s %s: %d audit entries, want 1", c.transport, c.cred, c.method, c.path, c.content.name, len(got))
			continue
		}
		if en := got[0]; en.RequestID != c.got.id || en.Outcome != master.OutcomeRefused || en.Method != c.audited || en.Reason != c.reason {
			t.Errorf("%s %s %s %s %s: entry %s %q %q, want refused %q %q", c.transport, c.cred, c.method, c.path, c.content.name, en.Outcome, en.Method, en.Reason, c.audited, c.reason)
		}
	}
	t.Logf("%d unauthenticated requests in %d groups, one answer each; %d health checks", len(cells)-health, len(groups), health)
}

// allAudit returns every entry of the master's audit log once ready holds
// for them, or ready is nil: it waits for the entries a test expects by
// event, not by order. An entry is handed to the audit writer when its
// request ends, which may be after the client has its whole answer (the
// front step's entry for an answer the RPC library flushed, the request
// path's entry after a handler that sent its answer), so one read right
// after the answer can come before the write. It reads until ready holds or
// ten seconds pass, then flushes and reads once more, so an entry written
// late beside the expected ones is counted too.
func allAudit(t *testing.T, e *env, ready func([]master.AuditEntry) bool) []master.AuditEntry {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		out := readAudit(t, e)
		if ready == nil || ready(out) {
			return readAudit(t, e)
		}
		if time.Now().After(deadline) {
			return out
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// haveIDs returns a readiness test for allAudit: every one of ids has an
// entry.
func haveIDs(ids ...string) func([]master.AuditEntry) bool {
	return func(es []master.AuditEntry) bool {
		seen := make(map[string]bool, len(es))
		for _, en := range es {
			seen[en.RequestID] = true
		}
		for _, id := range ids {
			if !seen[id] {
				return false
			}
		}
		return true
	}
}

// haveEntry returns a readiness test for allAudit: some entry matches.
func haveEntry(match func(master.AuditEntry) bool) func([]master.AuditEntry) bool {
	return func(es []master.AuditEntry) bool {
		for _, en := range es {
			if match(en) {
				return true
			}
		}
		return false
	}
}

// readAudit flushes the audit writer and reads the whole master log.
func readAudit(t *testing.T, e *env) []master.AuditEntry {
	t.Helper()
	if err := e.access.FlushAudit(e.ctx); err != nil {
		t.Fatal(err)
	}
	var out []master.AuditEntry
	page := master.Page{Limit: master.MaxPageLimit}
	for {
		es, next, err := e.master.ListAudit(e.ctx, master.AuditFilter{}, page)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, es...)
		if next == "" || len(es) == 0 {
			return out
		}
		page.Cursor = next
	}
}

// TestUnauthenticatedOddRequests guards the requests the matrix cannot
// make with an HTTP client, each sent raw over HTTP/1.1 without a key: an
// "OPTIONS *" (which net/http would otherwise answer itself), an Upgrade to
// h2c (which is not done: the request is answered as HTTP/1.1), a request
// that asks for 100-continue (which gets the refusal and no 100, so its body
// is never sent), and a preflight of the kind a browser sends. Each gets the
// one plain refusal, the same bytes, and the connection is closed.
func TestUnauthenticatedOddRequests(t *testing.T) {
	e := newEnv(t, options{})
	requests := map[string]string{
		"options star": "OPTIONS * HTTP/1.1\r\nHost: x\r\n\r\n",
		"upgrade h2c":  "GET /metrics HTTP/1.1\r\nHost: x\r\nConnection: Upgrade, HTTP2-Settings\r\nUpgrade: h2c\r\nHTTP2-Settings: AAMAAABkAAQCAAAAAAIAAAAA\r\n\r\n",
		"expect":       "POST " + servedProcedure + " HTTP/1.1\r\nHost: x\r\nContent-Type: text/plain\r\nContent-Length: 1000000\r\nExpect: 100-continue\r\n\r\n",
		"preflight":    "OPTIONS " + servedProcedure + " HTTP/1.1\r\nHost: x\r\nOrigin: https://example.org\r\nAccess-Control-Request-Method: POST\r\nAccess-Control-Request-Headers: authorization\r\n\r\n",
		"plain get":    "GET /x HTTP/1.1\r\nHost: x\r\n\r\n",
	}
	answers := map[string][]string{}
	for name, raw := range requests {
		conn := rawTLS(t, e)
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		if _, err := io.WriteString(conn, raw); err != nil {
			t.Fatal(err)
		}
		r := bufio.NewReader(conn)
		resp, err := http.ReadResponse(r, nil)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusContinue || !resp.Close {
			t.Errorf("%s: %d, Connection: close %v", name, resp.StatusCode, resp.Close)
		}
		a := answer{status: resp.StatusCode, header: headerText(resp.Header), body: body}
		answers[a.String()] = append(answers[a.String()], name)
	}
	if len(answers) != 1 {
		t.Fatalf("%d different answers: %v", len(answers), answers)
	}
	for a := range answers {
		if !strings.HasPrefix(a, "status 401") || strings.Contains(a, "Access-Control") {
			t.Fatalf("not the plain refusal:\n%s", a)
		}
	}
}

// TestStalledBodyRefusedAtOnce guards that a caller with no key cannot hold
// a request longer on one path than another: a request whose body never
// ends is refused at once on every path, a method whose requirement reads
// the request included, over HTTP/1.1 and HTTP/2, with a read bound long
// enough that waiting for the body would show.
func TestStalledBodyRefusedAtOnce(t *testing.T) {
	// Out of reach of the limit on failed calls, which would hold these
	// refusals back: the test is about refusing at once.
	e := newEnv(t, options{bodyRead: 5 * time.Second,
		accessRules: rulesOf(func(r *server.AccessRules) { r.FailureLimit = 1 << 30 })})
	for _, h2 := range []bool{false, true} {
		for _, p := range []string{nopacsv1connect.SettingsServiceGetSettingProcedure, servedProcedure, notServedMethod(t), "/acme.v9.Nothing/Do", "/"} {
			for _, ct := range []string{"application/grpc", "application/json", "application/connect+proto"} {
				pr, pw := io.Pipe()
				go func() { _, _ = pw.Write([]byte{0, 0, 0, 0, 9, 1}) }() // promises 9 bytes, sends 1
				req, _ := http.NewRequest(http.MethodPost, e.netURL+p, pr)
				req.Header.Set("Content-Type", ct)
				start := time.Now()
				resp, err := e.tlsClient(h2).Do(req)
				took := time.Since(start)
				_ = pw.Close()
				if err != nil {
					t.Fatalf("h2 %v %s %s: %v", h2, p, ct, err)
				}
				body, _ := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				refused := resp.StatusCode == http.StatusUnauthorized || resp.Trailer.Get("Grpc-Status") == "16" ||
					bytes.Contains(body, []byte(`"code":"unauthenticated"`))
				if !refused || took > time.Second {
					t.Errorf("h2 %v %s %s: %d after %v, want a refusal at once", h2, p, ct, resp.StatusCode, took)
				}
			}
		}
	}
}

// recordProcedure is a method only TestPercentEncodedNeverDispatched
// registers, whose handler records being called.
const recordProcedure = "/nopacs.test.Record/Call"

// TestPercentEncodedNeverDispatched guards that a path that is not exactly
// a route never reaches a handler, whoever asks: percent-encoded, with a
// double slash, a trailing slash or another case, in each protocol, the
// local operator's call is answered "no such method" and the handler is
// never called, while the exact path calls it once.
func TestPercentEncodedNeverDispatched(t *testing.T) {
	var calls atomic.Int64
	methods := servertest.Contract()
	methods[recordProcedure] = servertest.Entry{Fixed: access.Authenticated()}
	e := newEnv(t, options{noNetwork: true, methods: methods, register: func(s *connect.Server) {
		s.Register(connect.Method{
			Spec: connect.Spec{StreamType: connect.StreamTypeUnary, Procedure: recordProcedure},
			Handler: func(_ context.Context, _ connect.Spec, st connect.ServerStream) error {
				calls.Add(1)
				var in nopacsv1.WhoAmIRequest
				if err := st.Receive(&in); err != nil {
					return err
				}
				return st.Send(&nopacsv1.WhoAmIResponse{})
			},
		})
	}})
	near := []string{
		"/nopacs.test.Record%2FCall", "/nopacs.test.Record%2fCall", "/nopacs.test.Record/Cal%6C",
		"/%6Dagnetar.test.Record/Call", "//nopacs.test.Record/Call", "/nopacs.test.Record/Call/",
		"/nopacs.test.record/Call", "/nopacs.test.Record//Call",
	}
	for _, h2 := range []bool{false, true} {
		hc := e.socketClient(h2)
		for _, p := range near {
			for _, ct := range contentCases[:9] {
				a, err := rawCall(hc, http.MethodPost, "http://local"+p, ct, nil)
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(a.header+a.trailer+string(a.body), "no such method") {
					t.Errorf("h2 %v %s %s: not answered \"no such method\":\n%s", h2, p, ct.name, a)
				}
			}
		}
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("the handler was called %d times for a path that is not exactly its own", n)
	}
	a, err := rawCall(e.socketClient(true), http.MethodPost, "http://local"+recordProcedure, contentCases[0], nil)
	if err != nil || calls.Load() != 1 {
		t.Fatalf("the exact path did not call the handler: %v %v %s", err, calls.Load(), a)
	}
}

// TestAuthenticatedNotServed guards the answer to an authenticated caller
// (the local operator, and a key with no grant) who asks for something
// nothing serves: UNIMPLEMENTED "no such method" in its RPC protocol over
// HTTP/1.1 and HTTP/2, gRPC-Web included, never an HTTP 505; NOT_FOUND to a
// plain HTTP request; and exactly one audit entry each, failed, with the
// detail served=false, under the safe name of the path.
func TestAuthenticatedNotServed(t *testing.T) {
	e := newEnv(t, options{})
	key := e.makeKey(nopacsv1.Role_ROLE_UNSPECIFIED, nil)
	ns := notServedMethod(t)
	paths := map[string]string{ns: ns, "/nopacs.v1.AccessService/NoSuchMethod": "/nopacs.v1.AccessService/NoSuchMethod",
		"/acme.v9.Nothing/Do": "/acme.v9.Nothing/Do", "/nopacs.v1.AccessService%2FListKeys": "/", "/": "/", "//x": "/"}
	type sent struct {
		label, id, audited string
	}
	var all []sent
	for _, who := range []struct {
		name string
		hc   func(bool) *http.Client
		base string
		auth []string
	}{
		{"local operator", e.socketClient, "http://local", nil},
		{"key", e.tlsClient, e.netURL, []string{"Bearer " + key}},
	} {
		for _, h2 := range []bool{false, true} {
			for p, audited := range paths {
				for _, ct := range contentCases {
					a, err := rawCall(who.hc(h2), http.MethodPost, who.base+p, ct, who.auth)
					if err != nil {
						t.Fatal(err)
					}
					label := fmt.Sprintf("%s h2 %v %s %s", who.name, h2, p, ct.name)
					text := a.header + a.trailer + string(a.body)
					switch ct.class {
					case "json":
						rpc := ct.contentType == "application/json" || ct.contentType == "application/proto"
						if rpc && (a.status != http.StatusNotImplemented || !strings.Contains(text, `"code":"unimplemented","message":"no such method"`)) {
							t.Errorf("%s: %s", label, a)
						}
						if !rpc && (a.status != http.StatusNotFound || !strings.Contains(text, `"code":"not_found","message":"no such route"`)) {
							t.Errorf("%s: %s", label, a)
						}
					case "connect-stream", "connect-stream+json":
						if a.status != http.StatusOK || !strings.Contains(text, `{"error":{"code":"unimplemented","message":"no such method","details":[{"type":"google.rpc.ErrorInfo","value":"Cg1VTklNUExFTUVOVEVEEgZub3BhY3M"}]}}`) {
							t.Errorf("%s: %s", label, a)
						}
					case "grpc-web", "grpc-web+json":
						if a.status != http.StatusOK || !strings.Contains(string(a.body), "grpc-status: 12\r\ngrpc-message: no such method\r\n") {
							t.Errorf("%s: %s", label, a)
						}
					default:
						if a.status != http.StatusOK || !strings.Contains(a.trailer, "Grpc-Status: 12\n") || !strings.Contains(a.trailer, "Grpc-Message: no such method\n") {
							t.Errorf("%s: %s", label, a)
						}
					}
					if strings.HasPrefix(ct.class, "grpc") && strings.Contains(a.header, "Grpc-Status:") {
						t.Errorf("%s: grpc-status is in the header block with the request id: %s", label, a)
					}
					all = append(all, sent{label, a.id, audited})
				}
			}
		}
	}
	var ids []string
	for _, s := range all {
		ids = append(ids, s.id)
	}
	byID := map[string][]master.AuditEntry{}
	for _, en := range allAudit(t, e, haveIDs(ids...)) {
		byID[en.RequestID] = append(byID[en.RequestID], en)
	}
	for _, s := range all {
		got := byID[s.id]
		if len(got) != 1 {
			t.Errorf("%s: %d audit entries, want 1", s.label, len(got))
			continue
		}
		if en := got[0]; en.Outcome != master.OutcomeFailed || en.Details["served"] != "false" || en.Method != s.audited {
			t.Errorf("%s: entry %s %q %v, want failed %q served=false", s.label, en.Outcome, en.Method, en.Details, s.audited)
		}
	}
}

// TestAnsweredBeforeCheckAudited guards that a request the RPC library
// answers before the check, from an authenticated caller, still writes
// exactly one audit entry: failed, with the fixed reason and the HTTP
// status it was answered with. A wrong HTTP method, an unsupported content
// type and codec, an unknown compression and a malformed timeout header,
// in gRPC and in the Connect protocol, over HTTP/1.1 and HTTP/2, each to a
// served method; and a request the check takes up writes its own entry
// only.
func TestAnsweredBeforeCheckAudited(t *testing.T) {
	e := newEnv(t, options{noNetwork: true})
	type c struct {
		name, method string
		header       map[string]string
		body         []byte
	}
	cases := []c{
		{"http method", http.MethodPut, map[string]string{"Content-Type": "application/json"}, []byte("{}")},
		{"content type", http.MethodPost, map[string]string{"Content-Type": "text/plain"}, []byte("{}")},
		{"codec", http.MethodPost, map[string]string{"Content-Type": "application/grpc+xml", "Te": "trailers"}, make([]byte, 5)},
		{"stream shape", http.MethodPost, map[string]string{"Content-Type": "application/connect+proto"}, make([]byte, 5)},
		{"connect compression", http.MethodPost, map[string]string{"Content-Type": "application/json", "Content-Encoding": "br"}, []byte("{}")},
		{"grpc compression", http.MethodPost, map[string]string{"Content-Type": "application/grpc", "Grpc-Encoding": "br", "Te": "trailers"}, make([]byte, 5)},
		{"connect timeout", http.MethodPost, map[string]string{"Content-Type": "application/json", "Connect-Timeout-Ms": "soon"}, []byte("{}")},
		{"grpc timeout", http.MethodPost, map[string]string{"Content-Type": "application/grpc", "Grpc-Timeout": "xx", "Te": "trailers"}, make([]byte, 5)},
	}
	type sent struct{ label, id, status string }
	var all []sent
	for _, h2 := range []bool{false, true} {
		for _, k := range cases {
			req, _ := http.NewRequest(k.method, "http://local"+nopacsv1connect.AccessServiceWhoAmIProcedure, bytes.NewReader(k.body))
			for h, v := range k.header {
				req.Header.Set(h, v)
			}
			resp, err := e.socketClient(h2).Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			all = append(all, sent{fmt.Sprintf("h2 %v %s", h2, k.name), resp.Header.Get(server.RequestIDHeader), fmt.Sprint(resp.StatusCode)})
		}
	}
	ctx, info := withInfo(e.ctx)
	if _, err := e.local().access.WhoAmI(ctx, &nopacsv1.WhoAmIRequest{}); err != nil {
		t.Fatal(err)
	}
	okID := info.ResponseHeader().Get(server.RequestIDHeader)
	ids := []string{okID}
	for _, s := range all {
		ids = append(ids, s.id)
	}
	byID := map[string][]master.AuditEntry{}
	for _, en := range allAudit(t, e, haveIDs(ids...)) {
		byID[en.RequestID] = append(byID[en.RequestID], en)
	}
	for _, s := range all {
		got := byID[s.id]
		if len(got) != 1 {
			t.Errorf("%s: %d audit entries, want 1", s.label, len(got))
			continue
		}
		en := got[0]
		if en.Outcome != master.OutcomeFailed || !strings.HasPrefix(en.Reason, "answered before the check") ||
			en.Details["http_status"] != s.status || en.Method != nopacsv1connect.AccessServiceWhoAmIProcedure || en.LocalUser == "" {
			t.Errorf("%s: entry %+v, want failed, answered before the check, http_status %s", s.label, en, s.status)
		}
	}
	if n := len(byID[okID]); n != 1 {
		t.Errorf("an allowed WhoAmI wrote %d audit entries, want 1", n)
	}
}

// TestRefusalTiming reports the median time to refuse a request without a
// key for a served method and for a name nothing serves, which now share
// one code path, and fails only when they differ by more than a large
// margin, so a path that does more work for one of the two shows.
func TestRefusalTiming(t *testing.T) {
	if testing.Short() {
		t.Skip("timing needs many samples")
	}
	// A long close deadline, as in TestUnauthenticatedAnswerIsTheProtocols:
	// stop writes every refusal queued here first.
	e := newEnv(t, options{refusalQueue: 1 << 15, closeT: time.Minute})
	hc := e.tlsClient(true)
	paths := []string{servedProcedure, notServedMethod(t), "/acme.v9.Nothing/Do"}
	// Each sample is the mean of a batch of refusals: one refusal takes a
	// fraction of a millisecond, below the resolution of the clock on some
	// systems (Windows read most single refusals as 0s), while a batch is
	// long enough for every clock to measure.
	const batches, perBatch = 150, 20
	samples := make([][]time.Duration, len(paths))
	for i := range batches {
		for j := range paths {
			p := paths[(i+j)%len(paths)]
			k := slices.Index(paths, p)
			start := time.Now()
			for range perBatch {
				if _, err := rawCall(hc, http.MethodPost, e.netURL+p, contentCases[0], []string{"Bearer " + unknownKey}); err != nil {
					t.Fatal(err)
				}
			}
			samples[k] = append(samples[k], time.Since(start)/perBatch)
		}
	}
	medians := make([]time.Duration, len(paths))
	for i, s := range samples {
		sort.Slice(s, func(a, b int) bool { return s[a] < s[b] })
		medians[i] = s[len(s)/2]
		t.Logf("%-45s median %v over %d batches of %d", paths[i], medians[i], len(s), perBatch)
	}
	lo, hi := slices.Min(medians), slices.Max(medians)
	if hi > lo+lo/2 {
		t.Errorf("the medians differ by more than half: %v", medians)
	}
}

// keyReads is an access.Store that counts the reads of a key by its id.
type keyReads struct {
	access.Store
	n atomic.Int64
}

// Key counts the read and reads the key.
func (k *keyReads) Key(ctx context.Context, id string) (master.Key, error) {
	k.n.Add(1)
	return k.Store.Key(ctx, id)
}

// TestKeyReadOncePerRequest guards that judging the caller before routing
// does not cost a second read of the key: a request with a key reads it
// once, whether it is allowed (an RPC, the metrics), denied, or refused
// before it is routed; only a method whose requirement reads the request
// reads it again, at its check, once the message has arrived.
func TestKeyReadOncePerRequest(t *testing.T) {
	reads := &keyReads{}
	e := newEnv(t, options{store: func(s access.Store) access.Store { reads.Store = s; return reads }})
	admin := e.makeKey(nopacsv1.Role_ROLE_ADMIN, installation())
	noGrant := e.makeKey(nopacsv1.Role_ROLE_UNSPECIFIED, nil)
	stranger, err := secret.MakeAPIKey() // a key of the right form that nobody holds
	if err != nil {
		t.Fatal(err)
	}
	reads.n.Store(0)
	const n = 5
	for range n {
		if _, err := e.network("Bearer "+admin).access.WhoAmI(e.ctx, &nopacsv1.WhoAmIRequest{}); err != nil {
			t.Fatal(err)
		}
		if _, err := e.network("Bearer "+noGrant).access.GetKey(e.ctx, &nopacsv1.GetKeyRequest{}); connect.CodeOf(err) != connect.CodePermissionDenied {
			t.Fatal(err)
		}
		if _, err := e.network("Bearer "+stranger.Key()).access.GetKey(e.ctx, &nopacsv1.GetKeyRequest{}); connect.CodeOf(err) != connect.CodeUnauthenticated {
			t.Fatal(err)
		}
		req, _ := http.NewRequest(http.MethodGet, e.netURL+"/metrics", nil)
		req.Header.Set("Authorization", "Bearer "+admin)
		resp, err := e.tlsClient(true).Do(req)
		if err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("metrics: %v %v", resp, err)
		}
		_ = resp.Body.Close()
	}
	if got := reads.n.Load(); got != 4*n {
		t.Fatalf("%d requests read a key %d times, want once each", 4*n, got)
	}
	// A method whose requirement reads the request reads the key a second
	// time, at its check, after the message: the key may have been revoked
	// while the message arrived.
	reads.n.Store(0)
	for range n {
		_, err := e.network("Bearer "+admin).settings.GetSetting(e.ctx, &nopacsv1.GetSettingRequest{Name: "x", Scope: installation()})
		if connect.CodeOf(err) != connect.CodeUnimplemented {
			t.Fatal(err)
		}
	}
	if got := reads.n.Load(); got != 2*n {
		t.Fatalf("%d calls of a method that reads its request read a key %d times, want twice each", n, got)
	}
}

// TestRefusalRequestIDIsInitialMetadata guards that a refused caller, and an
// authenticated one told a method is not served, can read the request id
// where a client shows the answer's headers before its status: the response
// header (initial metadata), not only the trailers, for the generated
// client over gRPC, gRPC-Web and the Connect protocol, over HTTP/1.1 and
// HTTP/2. This client folds a Trailers-Only block into the response header,
// so it cannot see that form's flaw (grpcio shows such a block as trailing
// metadata only); the matrix holds the gRPC status out of the first header
// block for that.
func TestRefusalRequestIDIsInitialMetadata(t *testing.T) {
	e := newEnv(t, options{})
	for _, h2 := range []bool{false, true} {
		for _, proto := range []struct {
			name string
			opt  connecthttp.Option
		}{{"grpc", connecthttp.WithGRPC()}, {"grpc-web", connecthttp.WithGRPCWeb()}, {"connect", connecthttp.WithProtoJSON()}} {
			if proto.name == "grpc" && !h2 {
				continue // gRPC needs HTTP/2
			}
			for _, c := range []struct {
				who, base, auth, proc string
				hc                    *http.Client
				code                  connect.Code
			}{
				{"no key", e.netURL, "", servedProcedure, e.tlsClient(h2), connect.CodeUnauthenticated},
				{"malformed key", e.netURL, "garbage", servedProcedure, e.tlsClient(h2), connect.CodeUnauthenticated},
				{"local operator, not served", "http://local", "", "/acme.v9.Nothing/Do", e.socketClient(h2), connect.CodeUnimplemented},
			} {
				cl := connect.NewClient(connecthttp.NewTransport(c.hc, c.base, proto.opt), bearer(c.auth))
				ctx, info := withInfo(e.ctx)
				err := cl.CallUnary(ctx, connect.Spec{StreamType: connect.StreamTypeUnary, Procedure: c.proc}, &emptyMsg{}, &emptyMsg{})
				if connect.CodeOf(err) != c.code || !requestIDShape.MatchString(info.ResponseHeader().Get(server.RequestIDHeader)) {
					t.Errorf("h2 %v %s %s: %v, request id in the response header %q", h2, proto.name, c.who, err, info.ResponseHeader().Get(server.RequestIDHeader))
				}
			}
		}
	}
}
