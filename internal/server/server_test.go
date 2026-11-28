package server_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	"google.golang.org/protobuf/proto"

	nopacsv1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/gen/nopacs/v1/nopacsv1connect"
	"github.com/amrshadid/nopacs/internal/access"
	"github.com/amrshadid/nopacs/internal/driver/sqlite"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/obs"
	"github.com/amrshadid/nopacs/internal/server"
	"github.com/amrshadid/nopacs/internal/testkit/servertest"
)

// unknownKey is a key of the right shape that no installation holds.
const unknownKey = "nopacs_111111111111_1111111111111111111111111111111111111111111"

// TestRefusalsAreTheSame guards that refusals tell a caller nothing, and the
// order of checks: no key, a malformed key, an unknown key and a key with no
// grant get the fixed message and code, the same over gRPC and over Connect
// JSON; the handler never runs for any of them; the local operator and a key
// with the grant succeed. Each refusal carries a request id.
func TestRefusalsAreTheSame(t *testing.T) {
	e := newEnv(t, options{})
	noGrant := e.makeKey(nopacsv1.Role_ROLE_UNSPECIFIED, nil)
	admin := e.makeKey(nopacsv1.Role_ROLE_ADMIN, installation())
	before := e.standins.Ran.Load()

	cases := []struct {
		name, auth string
		code       connect.Code
		msg        string
	}{
		{"no key", "", connect.CodeUnauthenticated, access.MessageUnauthenticated},
		{"malformed key", "Bearer not-a-key", connect.CodeUnauthenticated, access.MessageUnauthenticated},
		{"not bearer", "Basic dXNlcjpwYXNz", connect.CodeUnauthenticated, access.MessageUnauthenticated},
		{"unknown key", "Bearer " + unknownKey, connect.CodeUnauthenticated, access.MessageUnauthenticated},
		{"no grant", "Bearer " + noGrant, connect.CodePermissionDenied, access.MessageDenied},
	}
	for _, grpc := range []bool{true, false} {
		for _, c := range cases {
			cl := newClients(e.tlsClient(grpc), e.netURL, grpc, c.auth)
			ctx, info := withInfo(e.ctx)
			_, err := cl.access.ListKeys(ctx, &nopacsv1.ListKeysRequest{})
			code, msg := codeOf(err)
			if code != c.code || msg != c.msg {
				t.Errorf("grpc=%v %s: got %v %q, want %v %q", grpc, c.name, code, msg, c.code, c.msg)
			}
			if info.ResponseHeader().Get(server.RequestIDHeader) == "" {
				t.Errorf("grpc=%v %s: no request id on the refusal", grpc, c.name)
			}
		}
	}
	if ran := e.standins.Ran.Load() - before; ran != 0 {
		t.Fatalf("a refused request ran a handler %d times", ran)
	}

	if _, err := e.local().access.ListKeys(e.ctx, &nopacsv1.ListKeysRequest{}); err != nil {
		t.Fatalf("the local operator was refused: %v", err)
	}
	if _, err := e.network("Bearer "+admin).access.ListKeys(e.ctx, &nopacsv1.ListKeysRequest{}); err != nil {
		t.Fatalf("an admin key was refused: %v", err)
	}
	// WhoAmI needs only a good key, so the key with no grant may call it.
	if _, err := e.network("Bearer "+noGrant).access.WhoAmI(e.ctx, &nopacsv1.WhoAmIRequest{}); err != nil {
		t.Fatalf("WhoAmI refused a good key: %v", err)
	}
}

// TestKeyOnTheSocket guards that a key sent on the local socket is the
// credential, so a refusal looks the same there, and that without one the
// caller is the local operator.
func TestKeyOnTheSocket(t *testing.T) {
	e := newEnv(t, options{noNetwork: true})
	_, err := newClients(e.socketClient(true), "http://local", true, "Bearer "+unknownKey).access.WhoAmI(e.ctx, &nopacsv1.WhoAmIRequest{})
	if code, _ := codeOf(err); code != connect.CodeUnauthenticated {
		t.Fatalf("an unknown key on the socket: %v", err)
	}
	res, err := e.local().access.WhoAmI(e.ctx, &nopacsv1.WhoAmIRequest{})
	if err != nil || res.GetPrincipal().GetLocalOperator().GetUserId() == "" {
		t.Fatalf("no local operator: %v %v", res, err)
	}
}

// TestScopeRequirementReadsMessage guards the rules that depend on the
// request: a key with reader on one tenant passes GetSetting for that
// tenant (and reaches the handler, which answers UNIMPLEMENTED) and is
// refused for another, and a malformed message from a caller without a key
// is answered UNAUTHENTICATED, not INVALID_ARGUMENT.
func TestScopeRequirementReadsMessage(t *testing.T) {
	e := newEnv(t, options{})
	ctx := e.ctx
	for id, name := range map[string]string{"t1": "lab", "t2": "clinic"} {
		if err := e.master.CreateTenant(ctx, master.Tenant{ID: id, Name: name, Driver: "sqlite", SealedAddress: []byte{1}, State: master.StateReady, Created: time.Now().UTC().Truncate(time.Microsecond)},
			master.AuditEntry{ID: server.NewID(), Time: time.Now(), Method: "test", Outcome: master.OutcomeDone}, master.LocalOperator()); err != nil {
			t.Fatal(err)
		}
	}
	reader := e.makeKey(nopacsv1.Role_ROLE_READER, &nopacsv1.Scope{Kind: &nopacsv1.Scope_Tenant{Tenant: "lab"}})
	cl := e.network("Bearer " + reader)
	_, err := cl.settings.GetSetting(ctx, &nopacsv1.GetSettingRequest{Name: "x", Scope: &nopacsv1.Scope{Kind: &nopacsv1.Scope_Tenant{Tenant: "lab"}}})
	if code, _ := codeOf(err); code != connect.CodeUnimplemented {
		t.Fatalf("reader on lab asking about lab: %v", err)
	}
	for _, other := range []string{"clinic", "nosuch"} {
		_, err = cl.settings.GetSetting(ctx, &nopacsv1.GetSettingRequest{Name: "x", Scope: &nopacsv1.Scope{Kind: &nopacsv1.Scope_Tenant{Tenant: other}}})
		if code, msg := codeOf(err); code != connect.CodePermissionDenied || msg != access.MessageDenied {
			t.Fatalf("reader on lab asking about %s: %v", other, err)
		}
	}
	// A body that is not JSON, without a key: the credential is checked
	// before the message is found malformed.
	req, _ := http.NewRequest(http.MethodPost, e.netURL+nopacsv1connect.SettingsServiceGetSettingProcedure, strings.NewReader("{not json"))
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.tlsClient(false).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized || !contains(string(body), "unauthenticated") {
		t.Fatalf("a malformed message without a key: %d %s", resp.StatusCode, body)
	}
	// The same body with the reader's key: it holds no role on the
	// installation, the fallback, so it is refused before it is told the
	// message is malformed.
	req, _ = http.NewRequest(http.MethodPost, e.netURL+nopacsv1connect.SettingsServiceGetSettingProcedure, strings.NewReader("{not json"))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+reader)
	resp, err = e.tlsClient(false).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("a malformed message from a reader of one tenant: %d", resp.StatusCode)
	}
}

// TestRequestID guards the request id: made by the server, returned on
// success, on a refusal, on a handler's panic, on the RPC library's own
// error and on a path no route serves; a client's own header of that name
// is ignored; no two ids are the same.
func TestRequestID(t *testing.T) {
	e := newEnv(t, options{})
	seen := map[string]bool{}
	hc := e.tlsClient(false)
	do := func(method, path, body, auth string) string {
		req, _ := http.NewRequest(method, e.netURL+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(server.RequestIDHeader, "chosen-by-client")
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		resp, err := hc.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		id := resp.Header.Get(server.RequestIDHeader)
		if id == "" || id == "chosen-by-client" || seen[id] {
			t.Fatalf("%s %s: request id %q", method, path, id)
		}
		seen[id] = true
		return id
	}
	admin := "Bearer " + e.makeKey(nopacsv1.Role_ROLE_ADMIN, installation())
	do("POST", nopacsv1connect.AccessServiceWhoAmIProcedure, "{}", admin)                         // success
	do("POST", nopacsv1connect.AccessServiceWhoAmIProcedure, "{}", "")                            // refusal
	do("POST", nopacsv1connect.TenantServiceListTenantsProcedure, `{"pageToken":"panic"}`, admin) // panic
	do("POST", nopacsv1connect.TenantServiceListTenantsProcedure, "{", admin)                     // the library's own error
	do("GET", "/no/such/path", "", "")                                                            // no route
	do("GET", "/metrics", "", "")                                                                 // plain route, refused
	if !contains(e.log.String(), "chosen-by-client") {
		return
	}
	t.Fatal("a client's request id reached the log")
}

// TestMessageLimit guards the size limit: a message of 4 MiB and one byte is
// refused with RESOURCE_EXHAUSTED, a message of exactly 4 MiB is read (and
// then refused by the handler as invalid, which shows it got through), and an
// answer over the limit is not sent.
func TestMessageLimit(t *testing.T) {
	e := newEnv(t, options{noNetwork: true})
	cl := e.local()
	// The encoded size of CreateKeyRequest{holder: h} is 1 (tag) + 4 (the
	// length as a varint, for a length near 4 MiB) + len(h).
	over := strings.Repeat("h", server.MaxMessageBytes+1-5)
	exact := strings.Repeat("h", server.MaxMessageBytes-5)
	if n := proto.Size(&nopacsv1.CreateKeyRequest{Holder: over}); n != server.MaxMessageBytes+1 {
		t.Fatalf("the over-limit message is %d bytes", n)
	}
	_, err := cl.access.CreateKey(e.ctx, &nopacsv1.CreateKeyRequest{Holder: over})
	if code, _ := codeOf(err); code != connect.CodeResourceExhausted {
		t.Fatalf("4 MiB + 1: %v", err)
	}
	_, err = cl.access.CreateKey(e.ctx, &nopacsv1.CreateKeyRequest{Holder: exact})
	if code, _ := codeOf(err); code != connect.CodeInvalidArgument {
		t.Fatalf("exactly 4 MiB: %v", err)
	}
	_, err = cl.tenants.ListTenants(e.ctx, &nopacsv1.ListTenantsRequest{PageToken: "huge"})
	if code, _ := codeOf(err); code != connect.CodeResourceExhausted {
		t.Fatalf("an answer over the limit: %v", err)
	}
}

// TestDefaultDeadline guards the default deadline: a call with no deadline is
// cut at the default (shortened here), and a deadline the client sends is
// kept.
func TestDefaultDeadline(t *testing.T) {
	e := newEnv(t, options{noNetwork: true, deadline: 300 * time.Millisecond})
	cl := e.local()
	start := time.Now()
	// The call carries no deadline of its own (one would be sent to the
	// server), so a watchdog fails the test if the server never cuts it.
	result := make(chan error, 1)
	go func() {
		_, err := cl.tenants.ListTenants(e.ctx, &nopacsv1.ListTenantsRequest{PageToken: "block"})
		result <- err
	}()
	var err error
	select {
	case err = <-result:
	case <-time.After(5 * time.Second):
		t.Fatal("a call with no deadline was not cut at the default; it is still running after 5s")
	}
	took := time.Since(start)
	if code, _ := codeOf(err); code != connect.CodeDeadlineExceeded {
		t.Fatalf("a call with no deadline: %v", err)
	}
	if took < 250*time.Millisecond || took > 5*time.Second {
		t.Fatalf("cut after %v, not at the 300ms default", took)
	}
	ctx, cancel := context.WithTimeout(e.ctx, 900*time.Millisecond)
	defer cancel()
	start = time.Now()
	_, err = cl.tenants.ListTenants(ctx, &nopacsv1.ListTenantsRequest{PageToken: "block"})
	if took := time.Since(start); took < 800*time.Millisecond {
		t.Fatalf("the client's longer deadline was cut at %v: %v", took, err)
	}
}

// TestPanicRecovered guards recovery from a panic: the caller gets INTERNAL
// with no detail, the log has the request id and a stack, and the server goes
// on serving.
func TestPanicRecovered(t *testing.T) {
	e := newEnv(t, options{noNetwork: true})
	cl := e.local()
	ctx, info := withInfo(e.ctx)
	_, err := cl.tenants.ListTenants(ctx, &nopacsv1.ListTenantsRequest{PageToken: "panic"})
	code, msg := codeOf(err)
	if code != connect.CodeInternal || msg != "internal error" {
		t.Fatalf("a panic answered %v %q", code, msg)
	}
	id := info.ResponseHeader().Get(server.RequestIDHeader)
	logText := e.log.String()
	if id == "" || !contains(logText, `"request_id":"`+id+`"`) || !contains(logText, "panic in a handler") || !contains(logText, "goroutine") {
		t.Fatalf("the panic's log line lacks the id %q or the stack:\n%s", id, logText)
	}
	if contains(msg, "on purpose") {
		t.Fatal("the panic's value reached the caller")
	}
	if _, err := cl.access.WhoAmI(e.ctx, &nopacsv1.WhoAmIRequest{}); err != nil {
		t.Fatalf("the server stopped serving after a panic: %v", err)
	}
}

// TestRedactedLogs guards the redaction of secrets end to end, since the
// protobuf library itself prints a debug_redact field: with the server's log
// at debug level, in JSON and in text, the database address sent to
// CreateTenant and the secret answered by CreateKey, and the Authorization
// header, are not in the log, while the requests are.
func TestRedactedLogs(t *testing.T) {
	const address = "postgres://owner:Pa55-in-the-log@db.example/lab" //nolint:gosec // G101: a planted test secret, searched for in the log.
	for _, format := range []obs.Format{obs.JSON, obs.Text} {
		t.Run(string(format), func(t *testing.T) {
			e := newEnv(t, options{format: format, level: -4})
			admin := e.makeKey(nopacsv1.Role_ROLE_ADMIN, installation())
			cl := e.network("Bearer " + admin)
			if _, err := cl.tenants.CreateTenant(e.ctx, &nopacsv1.CreateTenantRequest{
				Name: "lab", DatabaseType: nopacsv1.DatabaseType_DATABASE_TYPE_POSTGRES, DatabaseAddress: address,
			}); err != nil {
				t.Fatal(err)
			}
			res, err := cl.access.CreateKey(e.ctx, &nopacsv1.CreateKeyRequest{Holder: "robot"})
			if err != nil {
				t.Fatal(err)
			}
			logText := e.log.String()
			_, adminSecret, _ := strings.Cut(strings.TrimPrefix(admin, "nopacs_"), "_")
			_, newSecret, _ := strings.Cut(strings.TrimPrefix(res.GetSecret(), "nopacs_"), "_")
			for name, s := range map[string]string{"address": address, "password": "Pa55-in-the-log", "new key": res.GetSecret(),
				"new secret": newSecret, "admin key": admin, "admin secret": adminSecret} {
				if contains(logText, s) {
					t.Fatalf("the log holds the %s", name)
				}
			}
			for _, want := range []string{"message received", "message sent", "CreateTenant"} {
				if !contains(logText, want) {
					t.Fatalf("the debug log lacks %q, so the search proves nothing:\n%s", want, logText)
				}
			}
			// A message line says only that a message went by and how large it
			// was: no "message" attribute, whatever it would hold.
			for _, line := range strings.Split(logText, "\n") {
				if !contains(line, "message received") && !contains(line, "message sent") {
					continue
				}
				if !contains(line, "bytes") || contains(line, `"message":`) || contains(line, "message=") || contains(line, "message.") {
					t.Fatalf("a message line is not size only: %s", line)
				}
			}
			// No body is logged at any level: not the request's field, not the
			// answer's.
			for _, body := range []string{"robot", "lab", "db.example"} {
				if contains(logText, `"`+body+`"`) || contains(logText, `holder=`+body) {
					t.Fatalf("the debug log holds the body value %q:\n%s", body, logText)
				}
			}
		})
	}
}

// TestMetrics guards /metrics: the local operator reads it; over the network
// it needs a key with reader on the installation; and its text has every
// family the server exports, each listed below.
func TestMetrics(t *testing.T) {
	e := newEnv(t, options{})
	get := func(hc *http.Client, base, auth string) (int, string) {
		req, _ := http.NewRequest(http.MethodGet, base+"/metrics", nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		resp, err := hc.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if _, err := e.local().access.WhoAmI(e.ctx, &nopacsv1.WhoAmIRequest{}); err != nil {
		t.Fatal(err)
	}
	status, text := get(e.socketClient(false), "http://local", "")
	if status != http.StatusOK {
		t.Fatalf("local: %d %s", status, text)
	}
	for _, want := range []string{"nopacs_requests_total", "nopacs_request_duration_seconds_bucket", "nopacs_audit_entries_written_total",
		"nopacs_audit_entries_failed_total", "nopacs_audit_entries_dropped_refusals_total", "nopacs_audit_entries_dropped_records_total",
		"nopacs_audit_entries_dropped_writes_total", "nopacs_audit_entries_rerouted_total", "nopacs_audit_entries_notices_total",
		"nopacs_key_last_use_marks_written_total", "nopacs_key_last_use_marks_dropped_total", "nopacs_key_last_use_marks_failed_total",
		"nopacs_requests_in_flight 1"} {
		if !contains(text, want) {
			t.Errorf("the metrics lack %q", want)
		}
	}
	if status, _ := get(e.tlsClient(false), e.netURL, ""); status != http.StatusUnauthorized {
		t.Fatalf("network without a key: %d", status)
	}
	noGrant := e.makeKey(nopacsv1.Role_ROLE_UNSPECIFIED, nil)
	if status, _ := get(e.tlsClient(false), e.netURL, "Bearer "+noGrant); status != http.StatusForbidden {
		t.Fatalf("network with a key without a grant: %d", status)
	}
	reader := e.makeKey(nopacsv1.Role_ROLE_READER, installation())
	if status, _ := get(e.tlsClient(false), e.netURL, "Bearer "+reader); status != http.StatusOK {
		t.Fatalf("network with a reader key: %d", status)
	}
}

// TestHealth guards the health service: it answers without a key, over the
// network, by gRPC and by JSON, unary and streaming.
func TestHealth(t *testing.T) {
	e := newEnv(t, options{})
	req, _ := http.NewRequest(http.MethodPost, e.netURL+server.HealthCheckProcedure, strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.tlsClient(false).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(b) != `{"status":"SERVING"}` {
		t.Fatalf("health without a key: %d %s", resp.StatusCode, b)
	}
	// Watch is not served: a public stream that never ends would let any
	// caller hold connections without a key. It answers UNIMPLEMENTED at
	// once, and holds nothing.
	ctx, cancel := context.WithTimeout(e.ctx, 2*time.Second)
	defer cancel()
	cl := newClients(e.tlsClient(true), e.netURL, true, "")
	start := time.Now()
	stream, err := cl.raw.CallServerStream(ctx, connect.Spec{StreamType: connect.StreamTypeServer, Procedure: server.HealthWatchProcedure}, &nopacsv1.WhoAmIRequest{})
	if err == nil {
		defer func() { _ = stream.Close() }()
		var first healthStatus
		err = stream.Receive(&first)
	}
	if code, _ := codeOf(err); code != connect.CodeUnimplemented || time.Since(start) > time.Second {
		t.Fatalf("watch answered %v after %v, want UNIMPLEMENTED at once", err, time.Since(start))
	}
}

// healthStatus reads a HealthCheckResponse without its generated type: an
// empty message accepts any bytes, which is all the Watch test needs.
type healthStatus = nopacsv1.WhoAmIResponse

// TestStreamingRequiresKey guards the interceptor's streaming path for a
// method that needs a key: a server-streaming method registered on the
// server's connect.Server is refused without a key and served with one.
func TestStreamingRequiresKey(t *testing.T) {
	const proc = "/nopacs.test.Stream/Count"
	methods := servertest.Contract()
	methods[proc] = servertest.Entry{Fixed: access.Authenticated()}
	ran, withDecisionSeen := 0, false
	e := newEnv(t, options{methods: methods, register: func(s *connect.Server) {
		s.Register(connect.Method{
			Spec: connect.Spec{StreamType: connect.StreamTypeServer, Procedure: proc},
			Handler: func(ctx context.Context, _ connect.Spec, st connect.ServerStream) error {
				ran++
				_, withDecisionSeen = decisionFrom(ctx)
				var in nopacsv1.WhoAmIRequest
				if err := st.Receive(&in); err != nil {
					return err
				}
				for range 3 {
					if err := st.Send(&nopacsv1.WhoAmIResponse{}); err != nil {
						return err
					}
				}
				return nil
			},
		})
	}})
	count := func(auth string) (int, error) {
		cl := newClients(e.tlsClient(true), e.netURL, true, auth)
		st, err := cl.raw.CallServerStream(e.ctx, connect.Spec{StreamType: connect.StreamTypeServer, Procedure: proc}, &nopacsv1.WhoAmIRequest{})
		if err != nil {
			return 0, err
		}
		defer func() { _ = st.Close() }()
		n := 0
		for {
			var m nopacsv1.WhoAmIResponse
			if err := st.Receive(&m); err != nil {
				if errors.Is(err, io.EOF) {
					return n, nil
				}
				return n, err
			}
			n++
		}
	}
	if _, err := count(""); func() connect.Code { c, _ := codeOf(err); return c }() != connect.CodeUnauthenticated {
		t.Fatalf("a stream without a key: %v", err)
	}
	if ran != 0 {
		t.Fatal("the stream's handler ran for a refused caller")
	}
	key := e.makeKey(nopacsv1.Role_ROLE_UNSPECIFIED, nil)
	if n, err := count("Bearer " + key); err != nil || n != 3 {
		t.Fatalf("a stream with a key: %d messages, %v", n, err)
	}
	if !withDecisionSeen {
		t.Fatal("the handler's context did not carry the Decision put there by WithDecision")
	}
}

// TestNewRefusesUncheckedMethod guards that a method registered without an
// entry in the table is refused at New, never served.
func TestNewRefusesUncheckedMethod(t *testing.T) {
	a, err := access.New(access.Config{Store: nilStore{}, Clock: time.Now, NewID: server.NewID})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Close(context.Background()) }()
	methods := servertest.Contract()
	delete(methods, nopacsv1connect.AccessServiceWhoAmIProcedure)
	_, err = server.New(server.Config{NoSocket: true, Access: a, Methods: methods, Register: func(s *connect.Server) {
		nopacsv1connect.RegisterAccessServiceHandler(s, nopacsv1connect.UnimplementedAccessServiceHandler{})
	}})
	if err == nil || !contains(err.Error(), "WhoAmI") {
		t.Fatalf("a method without a rule was accepted: %v", err)
	}
}

// nilStore is an access.Store that is never called, for servers that serve
// nothing.
type nilStore struct{ access.Store }

// TestListenRules guards the network listener's rules: without a
// certificate only a loopback address written as an address; a certificate
// needs its key and an address; a certificate that does not load is
// refused with the reason.
func TestListenRules(t *testing.T) {
	a, err := access.New(access.Config{Store: nilStore{}, Clock: time.Now, NewID: server.NewID})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Close(context.Background()) }()
	reg := func(*connect.Server) {}
	for _, c := range []struct {
		cfg server.Config
		ok  bool
	}{
		{server.Config{Listen: "127.0.0.1:0"}, true},
		{server.Config{Listen: "[::1]:0"}, true},
		{server.Config{Listen: "0.0.0.0:8443"}, false},
		{server.Config{Listen: ":8443"}, false},
		{server.Config{Listen: "localhost:8443"}, false},
		{server.Config{Listen: "192.168.1.10:8443"}, false},
		{server.Config{Listen: "nonsense"}, false},
		{server.Config{TLSCert: "c.pem", TLSKey: "k.pem"}, false},
		{server.Config{Listen: "0.0.0.0:8443", TLSCert: "c.pem"}, false},
		{server.Config{Listen: "0.0.0.0:8443", TLSCert: "/no/such.pem", TLSKey: "/no/such-key.pem"}, false},
	} {
		c.cfg.NoSocket, c.cfg.Access, c.cfg.Register = true, a, reg
		_, err := server.New(c.cfg)
		if (err == nil) != c.ok {
			t.Errorf("%+v: %v", c.cfg.Listen, err)
		}
	}
}

// TestLock guards one server per data directory within one process: a second
// lock on a data directory is refused, naming the holder's process, and once
// released the directory can be locked again. (The process-level test with
// signal 9 is in package servecmd.)
func TestLock(t *testing.T) {
	dir := shortTempDir(t)
	l, err := server.TakeLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	_, err = server.TakeLock(dir)
	if fault.KindOf(err) != fault.Conflict || !contains(err.Error(), "process "+strconv.Itoa(os.Getpid())) {
		t.Fatalf("a second lock: %v", err)
	}
	if err := l.Release(); err != nil {
		t.Fatal(err)
	}
	l2, err := server.TakeLock(dir)
	if err != nil {
		t.Fatalf("relocking after release: %v", err)
	}
	_ = l2.Release()
	_ = l.Release()
}

// TestSocketPrivate guards the local socket's guards on this system: the
// directory is mode 0700 and the socket 0600 (Unix), a directory others can
// enter is refused at start, and a peer who is not the server's user is
// closed before any byte is read.
func TestSocketPrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("modes do not apply; the access list is set by EnsurePrivateDir")
	}
	e := newEnv(t, options{noNetwork: true})
	run, _ := os.Stat(server.RunDir(e.dir))
	sock, _ := os.Lstat(server.SocketPath(e.dir))
	if run.Mode().Perm() != 0o700 || sock.Mode().Perm() != 0o600 {
		t.Fatalf("run/ is %v and the socket %v", run.Mode().Perm(), sock.Mode().Perm())
	}

	// A peer who is not the server's user: the server is told to expect
	// another user, so this test's own connection is that stranger.
	e2 := newEnv(t, options{noNetwork: true, beforeRun: func(s *server.Server) { s.SetSocketUser("4242424242") }})
	_, err := e2.local().access.WhoAmI(e2.ctx, &nopacsv1.WhoAmIRequest{})
	if err == nil {
		t.Fatal("a peer who is not the server's user was served")
	}
	if !contains(e2.log.String(), "from another user was closed") {
		t.Fatalf("the refused peer was not logged:\n%s", e2.log.String())
	}

	// A run directory others may enter is refused at start.
	dir := shortTempDir(t)
	if err := os.Mkdir(server.RunDir(dir), 0o755); err != nil { //nolint:gosec // G301: an open directory is what the test shows refused.
		t.Fatal(err)
	}
	if err := os.Chmod(server.RunDir(dir), 0o755); err != nil { //nolint:gosec // G302: as above.
		t.Fatal(err)
	}
	if _, err := server.TakeLock(dir); err == nil || !contains(err.Error(), "chmod 700") {
		t.Fatalf("a run directory of mode 0755 was accepted: %v", err)
	}
}

// TestStaleSocket guards the removal of a socket left by a dead process:
// a socket file nothing answers on is removed and the server starts; one a
// live process answers on is left alone and the start refused; a file that
// is not a socket is never removed.
func TestStaleSocket(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("socket files are not left behind the same way on Windows")
	}
	dir := shortTempDir(t)
	if err := server.EnsurePrivateDir(server.RunDir(dir)); err != nil {
		t.Fatal(err)
	}
	path := server.SocketPath(dir)
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	_ = ln.Close()
	if _, err := os.Lstat(path); err != nil {
		t.Fatal("the stale socket was not left in place")
	}
	e := startedIn(t, dir)
	if _, err := e.local().access.WhoAmI(e.ctx, &nopacsv1.WhoAmIRequest{}); err != nil {
		t.Fatalf("the server did not start over a stale socket: %v", err)
	}

	// A live socket another process (here, another listener) answers on.
	dir2 := shortTempDir(t)
	if err := server.EnsurePrivateDir(server.RunDir(dir2)); err != nil {
		t.Fatal(err)
	}
	live, err := net.Listen("unix", server.SocketPath(dir2))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = live.Close() }()
	go func() {
		for {
			c, err := live.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	if err := startIn(t, dir2); err == nil || !contains(err.Error(), "another process answers") {
		t.Fatalf("a live socket was taken over: %v", err)
	}

	// A regular file in the socket's place.
	dir3 := shortTempDir(t)
	if err := server.EnsurePrivateDir(server.RunDir(dir3)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(server.SocketPath(dir3), []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := startIn(t, dir3); err == nil || !contains(err.Error(), "not a socket") {
		t.Fatalf("a regular file in the socket's place: %v", err)
	}
	if b, _ := os.ReadFile(server.SocketPath(dir3)); string(b) != "keep me" {
		t.Fatal("a regular file was removed")
	}
}

// startedIn starts a server over a fresh master in a directory the test
// prepared.
func startedIn(t *testing.T, dir string) *env {
	t.Helper()
	srv, a, err := build(t, dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Stop() })
	e := &env{t: t, ctx: context.Background(), dir: dir, srv: srv, access: a, log: &syncBuffer{}}
	t.Cleanup(func() {
		e.closeClients()
	})
	return e
}

// startIn builds and starts a server in dir and returns Start's error,
// stopping it again when it started.
func startIn(t *testing.T, dir string) error {
	t.Helper()
	srv, _, err := build(t, dir)
	if err != nil {
		return err
	}
	err = srv.Start()
	_ = srv.Stop()
	return err
}

// build makes a server over a fresh master in dir, without starting it.
func build(t *testing.T, dir string) (*server.Server, *access.Access, error) {
	t.Helper()
	lock, err := server.TakeLock(dir)
	if err != nil {
		return nil, nil, err
	}
	m, err := sqlite.OpenMaster(context.Background(), filepath.Join(dir, "master.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	a, err := access.New(access.Config{Store: m, Clock: time.Now, NewID: server.NewID})
	if err != nil {
		t.Fatal(err)
	}
	h := &servertest.Standins{Access: a}
	srv, err := server.New(server.Config{DataDir: dir, Lock: lock, Access: a, Master: m, Register: h.Register, Methods: servertest.Contract()})
	return srv, a, err
}

// TestStopDrains guards a graceful stop in process: with requests in flight
// and refusals queued for the audit writer, Stop lets every request in flight
// finish, refuses new ones, writes every audit entry queued, and returns
// without error within the drain deadline.
func TestStopDrains(t *testing.T) {
	const inFlight, refusals = 40, 200
	e := newEnv(t, options{noNetwork: true, delay: 500 * time.Millisecond, drain: 10 * time.Second})
	cl := e.local()
	var wg sync.WaitGroup
	errs := make(chan error, inFlight)
	for range inFlight {
		wg.Go(func() {
			_, err := cl.tenants.ListTenants(e.ctx, &nopacsv1.ListTenantsRequest{PageToken: "wait"})
			errs <- err
		})
	}
	// Refusals: each queues one audit entry for the writer.
	bad := newClients(e.socketClient(true), "http://local", true, "Bearer "+unknownKey)
	for range refusals {
		if _, err := bad.access.WhoAmI(e.ctx, &nopacsv1.WhoAmIRequest{}); err == nil {
			t.Fatal("an unknown key was served")
		}
	}
	waitFor(t, func() bool {
		return e.srv.Metrics() != nil && strings.Contains(metricsText(e), "nopacs_requests_in_flight 40")
	})
	start := time.Now()
	if err := e.srv.Stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}
	took := time.Since(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("a request in flight did not finish: %v", err)
		}
	}
	if got := e.standins.Finished.Load(); got != inFlight {
		t.Fatalf("%d of %d requests in flight finished", got, inFlight)
	}
	if took > 5*time.Second {
		t.Fatalf("stop took %v", took)
	}
	stats := e.access.AuditStats()
	if stats.Dropped() != 0 || stats.Failed != 0 {
		t.Fatalf("audit entries lost at stop: %+v", stats)
	}
	if stats.Written < refusals {
		t.Fatalf("only %d audit entries written for %d refusals", stats.Written, refusals)
	}
	// A request after Stop finds nothing listening.
	if _, err := cl.access.WhoAmI(e.ctx, &nopacsv1.WhoAmIRequest{}); err == nil {
		t.Fatal("a request after stop was served")
	}
}

// metricsText is the server's metrics as text.
func metricsText(e *env) string {
	var b bytes.Buffer
	_ = e.srv.Metrics().WriteText(&b)
	return b.String()
}

// waitFor polls cond for up to five seconds.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestStopCancelsAfterDrain guards that Stop does not hang on a request
// that will not finish: past the drain deadline its context is canceled
// and Stop returns.
func TestStopCancelsAfterDrain(t *testing.T) {
	e := newEnv(t, options{noNetwork: true, drain: 300 * time.Millisecond, deadline: time.Minute})
	cl := e.local()
	done := make(chan error, 1)
	go func() {
		_, err := cl.tenants.ListTenants(e.ctx, &nopacsv1.ListTenantsRequest{PageToken: "block"})
		done <- err
	}()
	waitFor(t, func() bool { return strings.Contains(metricsText(e), "nopacs_requests_in_flight 1") })
	start := time.Now()
	_ = e.srv.Stop()
	if took := time.Since(start); took > 3*time.Second {
		t.Fatalf("stop hung for %v", took)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("the blocked request succeeded instead of being ended")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the blocked request was not ended")
	}
}

// TestTLSMinimum guards TLS 1.2 as the floor, decided by the server: a
// ClientHello that offers at most TLS 1.1, written byte by byte (Go's own
// client refuses to offer it at all, so it would never reach the server),
// gets the server's protocol_version alert, not a ServerHello; and a client
// that offers 1.2 is served.
func TestTLSMinimum(t *testing.T) {
	e := newEnv(t, options{})
	addr := strings.TrimPrefix(e.netURL, "https://")
	typ, alert := sendOldHello(t, addr)
	if typ != 0x15 || alert != 70 {
		t.Fatalf("a TLS 1.1 ClientHello got record type %#x (alert %d), want an alert, protocol_version (70)", typ, alert)
	}
	ok, err := tls.Dial("tcp", addr, &tls.Config{RootCAs: e.roots, MinVersion: tls.VersionTLS12, MaxVersion: tls.VersionTLS12}) //nolint:gosec // G402: TLS 1.2 exactly is the point of the test.
	if err != nil {
		t.Fatalf("a TLS 1.2 client was refused: %v", err)
	}
	_ = ok.Close()
}

// factsTable is the contract's table, giving every request the audit
// object "the-object", as internal/api's table gives each request its own.
type factsTable struct{ servertest.Table }

// Requirement adds the object to the contract table's answer.
func (f factsTable) Requirement(procedure string, msg proto.Message) (access.Requirement, access.Request) {
	req, facts := f.Table.Requirement(procedure, msg)
	facts.Object = "the-object"
	return req, facts
}

// TestTableFactsReachAudit guards that the audit facts the table returns
// for a request reach the entry the check writes, beside the method and the
// request id the server fills: here the denial of a good key with no grant.
// A key that is no good is refused before the request is routed, so its
// entry has the method and the request id and nothing of the table, which
// is never asked for a caller who is not authenticated.
func TestTableFactsReachAudit(t *testing.T) {
	dir := shortTempDir(t)
	lock, err := server.TakeLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	m, err := sqlite.OpenMaster(context.Background(), filepath.Join(dir, "master.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	a, err := access.New(access.Config{Store: m, Clock: time.Now, NewID: server.NewID})
	if err != nil {
		t.Fatal(err)
	}
	h := &servertest.Standins{Access: a}
	srv, err := server.New(server.Config{DataDir: dir, Lock: lock, Access: a, Master: m, Register: h.Register, Methods: factsTable{servertest.Contract()}})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Stop() })
	e2 := &env{t: t, ctx: context.Background(), dir: dir}
	t.Cleanup(func() {
		for _, c := range e2.clients {
			c.CloseIdleConnections()
		}
	})
	noGrant := e2.makeKey(nopacsv1.Role_ROLE_UNSPECIFIED, nil)
	ctx, info := withInfo(context.Background())
	_, err = newClients(e2.socketClient(true), "http://local", true, "Bearer "+noGrant).access.ListKeys(ctx, &nopacsv1.ListKeysRequest{})
	if code, _ := codeOf(err); code != connect.CodePermissionDenied {
		t.Fatalf("a key with no grant: %v", err)
	}
	uctx, uinfo := withInfo(context.Background())
	_, err = newClients(e2.socketClient(true), "http://local", true, "Bearer "+unknownKey).access.ListKeys(uctx, &nopacsv1.ListKeysRequest{})
	if code, _ := codeOf(err); code != connect.CodeUnauthenticated {
		t.Fatalf("an unknown key: %v", err)
	}
	if err := a.FlushAudit(context.Background()); err != nil {
		t.Fatal(err)
	}
	entries, _, err := m.ListAudit(context.Background(), master.AuditFilter{Outcome: master.OutcomeRefused}, master.Page{Limit: 10})
	if err != nil || len(entries) != 2 {
		t.Fatalf("%d refusal entries, %v", len(entries), err)
	}
	byID := map[string]master.AuditEntry{}
	for _, en := range entries {
		byID[en.RequestID] = en
	}
	en := byID[info.ResponseHeader().Get(server.RequestIDHeader)]
	if en.Object != "the-object" || en.Method != nopacsv1connect.AccessServiceListKeysProcedure || en.Reason != "no sufficient grant" {
		t.Fatalf("the denial's entry is %+v", en)
	}
	en = byID[uinfo.ResponseHeader().Get(server.RequestIDHeader)]
	if en.Object != "" || en.Method != nopacsv1connect.AccessServiceListKeysProcedure || en.Outcome != master.OutcomeRefused {
		t.Fatalf("the unknown key's entry is %+v", en)
	}
}

// TestHandlerConnectErrorsPassThrough guards that an error a handler builds
// with connect.NewError reaches the caller with its own code and message,
// for every code: only an error with a fault kind is mapped through
// codes.ToConnect, and mapping a connect error again would turn it into
// INTERNAL.
func TestHandlerConnectErrorsPassThrough(t *testing.T) {
	const proc = "/nopacs.test.Errors/Fail"
	methods := servertest.Contract()
	methods[proc] = servertest.Entry{Fixed: access.Authenticated()}
	var answer connect.Code
	e := newEnv(t, options{noNetwork: true, methods: methods, register: func(s *connect.Server) {
		s.Register(connect.Method{
			Spec: connect.Spec{StreamType: connect.StreamTypeUnary, Procedure: proc},
			Handler: func(_ context.Context, _ connect.Spec, st connect.ServerStream) error {
				var in nopacsv1.WhoAmIRequest
				if err := st.Receive(&in); err != nil {
					return err
				}
				return connect.NewError(answer, "handler says "+answer.String())
			},
		})
	}})
	cl := e.local()
	for c := connect.CodeCanceled; c <= connect.CodeUnauthenticated; c++ {
		answer = c
		err := cl.raw.CallUnary(e.ctx, connect.Spec{StreamType: connect.StreamTypeUnary, Procedure: proc}, &nopacsv1.WhoAmIRequest{}, &nopacsv1.WhoAmIResponse{})
		code, msg := codeOf(err)
		if code != c || msg != "handler says "+c.String() {
			t.Errorf("a handler's %v reached the caller as %v %q", c, code, msg)
		}
	}
}

// TestBearerScheme guards how the key is taken from the Authorization
// header (RFC 6750 2.1, access.APIKey's contract): the scheme is compared
// without regard to case and the spaces around the key are dropped, so only
// the key reaches the check; a header in any other form is passed on whole,
// which the check refuses as a malformed key.
func TestBearerScheme(t *testing.T) {
	key := access.APIKey(unknownKey)
	for _, h := range []string{
		"Bearer " + unknownKey,
		"bearer " + unknownKey,
		"BEARER " + unknownKey,
		"BeArEr " + unknownKey,
		"Bearer    " + unknownKey,
		"  Bearer " + unknownKey + "  ",
	} {
		if server.CredentialOf(h) != key {
			t.Errorf("%q did not give the key alone", h)
		}
	}
	for _, h := range []string{"Basic " + unknownKey, unknownKey, "Bearer", "Bearer "} {
		if got := server.CredentialOf(h); got == key || got == access.APIKey("") {
			t.Errorf("%q gave %v, not a malformed key", h, got)
		}
	}
	if server.CredentialOf("") != access.APIKey("") {
		t.Error("no header is not a request with no key")
	}
}
