package server_test

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/amrshadid/nopacs/internal/master"
)

// servedProcedure is a method the stand-ins serve whose requirement does
// not read the request, so a refusal of it depends on the key alone.
const servedProcedure = "/nopacs.v1.AccessService/ListKeys"

// notServedProcedures are a method of the contract that no handler serves
// and a name in no contract: both must be indistinguishable from a served
// method to a caller without a valid key.
var notServedProcedures = []string{"/nopacs.v1.StorageService/GetStorage", "/acme.v9.Nothing/Do"}

// rpcProtocol is one way of making a call: its content type and the body of
// an empty request in it.
type rpcProtocol struct {
	name, contentType string
	body              []byte
	header            map[string]string
}

// rpcProtocols are the protocols the server speaks: gRPC, gRPC-Web, and the
// Connect protocol with JSON and with protobuf. An empty message in the
// enveloped protocols is a five-byte frame of flags and length.
var rpcProtocols = []rpcProtocol{
	{"grpc", "application/grpc", make([]byte, 5), map[string]string{"Te": "trailers"}},
	{"grpc-web", "application/grpc-web+proto", make([]byte, 5), nil},
	{"connect-json", "application/json", []byte("{}"), nil},
	{"connect-proto", "application/proto", nil, nil},
}

// answer is everything a client receives for one call, but the date and
// the request id, which differ from one answer to the next by design.
type answer struct {
	status   int
	header   string
	trailer  string
	body     []byte
	id       string
	protocol string
}

// String writes the answer for a failure message and for the comparison
// file.
func (a answer) String() string {
	return fmt.Sprintf("status %d\nheaders:\n%strailers:\n%sbody %q\n", a.status, a.header, a.trailer, a.body)
}

// headerText writes h sorted, one line per value, without the headers
// that differ between any two answers.
func headerText(h http.Header) string {
	var lines []string
	for k, vs := range h {
		if k == "Date" || k == "Nopacs-Request-Id" {
			continue
		}
		for _, v := range vs {
			lines = append(lines, k+": "+v)
		}
	}
	sort.Strings(lines)
	var b strings.Builder
	for _, l := range lines {
		b.WriteString(l + "\n")
	}
	return b.String()
}

// call makes one raw call over hc and returns what came back.
func call(t *testing.T, hc *http.Client, base, path string, p rpcProtocol, authorization string) answer {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, base+path, bytes.NewReader(p.body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", p.contentType)
	for k, v := range p.header {
		req.Header.Set(k, v)
	}
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", p.name, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return answer{
		status: resp.StatusCode, header: headerText(resp.Header), trailer: headerText(resp.Trailer),
		body: body, id: resp.Header.Get("Nopacs-Request-Id"), protocol: p.name,
	}
}

// TestNotServedRefusedAsServed guards that a caller with no key or a bad
// key cannot tell a method the server serves from one it does not: in each
// protocol, the status, the headers, the trailers and the body of the
// refusal are the same for a served method, for a method of the contract
// no handler serves, and for a name in no contract. With
// NOPACS_SERVED_ANSWERS set to a file name, the served method's answers
// are written there, so the answers of two builds can be compared.
func TestNotServedRefusedAsServed(t *testing.T) {
	e := newEnv(t, options{})
	hc := e.tlsClient(true)
	var dump strings.Builder
	for _, p := range rpcProtocols {
		for _, auth := range []string{"", "Bearer not-a-key", "Bearer " + unknownKey} {
			served := call(t, hc, e.netURL, servedProcedure, p, auth)
			fmt.Fprintf(&dump, "== %s, authorization %q\n%s", p.name, auth, served)
			if served.status == http.StatusOK && p.name != "grpc" && p.name != "grpc-web" {
				t.Fatalf("%s %q: the served method was not refused: %s", p.name, auth, served)
			}
			for _, path := range notServedProcedures {
				got := call(t, hc, e.netURL, path, p, auth)
				if got.id == "" {
					t.Errorf("%s %s %q: no request id", p.name, path, auth)
				}
				if got.String() != served.String() {
					t.Errorf("%s %s, authorization %q: answered differently from a served method\nserved:\n%s\nnot served:\n%s", p.name, path, auth, served, got)
				}
			}
		}
	}
	if !strings.Contains(dump.String(), "the request carries no valid key") {
		t.Fatalf("no refusal was seen:\n%s", dump.String())
	}
	if f := os.Getenv("NOPACS_SERVED_ANSWERS"); f != "" {
		if err := os.WriteFile(f, []byte(dump.String()), 0o600); err != nil { //nolint:gosec // G703: a file the person running the test names.
			t.Fatal(err)
		}
	}
}

// TestNotServedAudited guards the audit entries of calls to procedures no
// handler serves, all in the installation's log: a refused one is recorded
// as refused under the procedure asked for; an allowed one, which the
// contract also has write an entry, as failed, with the detail that the
// method is not served; and a path that is not the name of a procedure
// (control characters, 5000 bytes, an identifier that starts with a digit or
// is empty, not the shape of one) only under the fixed name "/", and never
// whole in the log.
func TestNotServedAudited(t *testing.T) {
	e := newEnv(t, options{})
	grpc := rpcProtocols[0]
	long := "/acme.v9.Nothing/" + strings.Repeat("x", 5000)
	type want struct {
		id, method string
		outcome    master.Outcome
		local      bool
	}
	var wants []want
	for _, path := range notServedProcedures {
		a := call(t, e.tlsClient(true), e.netURL, path, grpc, "")
		wants = append(wants, want{a.id, path, master.OutcomeRefused, false})
		a = call(t, e.socketClient(true), "http://local", path, grpc, "")
		if !strings.Contains(a.trailer+a.header, "no such method") {
			t.Errorf("%s for the local operator: %s", path, a)
		}
		wants = append(wants, want{a.id, path, master.OutcomeFailed, true})
	}
	for _, path := range []string{"/acme.v9.Not%01thing/Do", "/acme.v9.Nothing/Do%0Aforged", long, "/just-a-path", "/acme.v9.Nothing/",
		"/acme.9v.Nothing/Do", "/acme.v9.Nothing/9Do", "/acme..Nothing/Do"} {
		a := call(t, e.socketClient(true), "http://local", path, grpc, "")
		wants = append(wants, want{a.id, "/", master.OutcomeFailed, true})
		a = call(t, e.tlsClient(true), e.netURL, path, grpc, "Bearer not-a-key")
		wants = append(wants, want{a.id, "/", master.OutcomeRefused, false})
	}
	var ids []string
	for _, w := range wants {
		ids = append(ids, w.id)
	}
	entries := allAudit(t, e, haveIDs(ids...))
	for _, w := range wants {
		i := slices.IndexFunc(entries, func(en master.AuditEntry) bool { return en.RequestID == w.id })
		if w.id == "" || i < 0 {
			t.Errorf("%q (%s): no audit entry for request %q", w.method, w.outcome, w.id)
			continue
		}
		en := entries[i]
		if en.Method != w.method || en.Outcome != w.outcome || (en.LocalUser != "") != w.local {
			t.Errorf("request %s: method %q, outcome %v, local user %q; want %q, %v, local %v", w.id, en.Method, en.Outcome, en.LocalUser, w.method, w.outcome, w.local)
		}
		if w.outcome == master.OutcomeFailed && en.Details["served"] != "false" {
			t.Errorf("request %s: details %v, want served=false", w.id, en.Details)
		}
	}
	logText := e.log.String()
	if strings.Contains(logText, strings.Repeat("x", 300)) || strings.Contains(logText, "forged") || strings.Contains(logText, "Not\\u0001thing") {
		t.Fatal("a hostile path reached the log")
	}
}
