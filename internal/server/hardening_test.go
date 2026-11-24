package server_test

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/server"
	"github.com/amrshadid/nopacs/internal/testkit/servertest"
)

// sendOldHello sends addr a TLS ClientHello that offers TLS 1.0 and 1.1
// only (the record and the hello written by hand, with ECDHE suites a TLS
// 1.1 server could choose), and returns the type of the first record the
// server answers with and, for an alert, its description.
func sendOldHello(t *testing.T, addr string) (recordType, alert byte) {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))

	var hello []byte
	hello = append(hello, 0x03, 0x02) // client_version: TLS 1.1
	random := make([]byte, 32)
	_, _ = rand.Read(random)
	hello = append(hello, random...)
	hello = append(hello, 0) // no session id
	suites := []uint16{0xc009, 0xc00a, 0xc013, 0xc014, 0x002f, 0x0035}
	hello = binary.BigEndian.AppendUint16(hello, u16(2*len(suites)))
	for _, s := range suites {
		hello = binary.BigEndian.AppendUint16(hello, s)
	}
	hello = append(hello, 1, 0) // compression: null only
	ext := []byte{
		0x00, 0x0a, 0x00, 0x06, 0x00, 0x04, 0x00, 0x17, 0x00, 0x1d, // supported_groups: P-256, X25519
		0x00, 0x0b, 0x00, 0x02, 0x01, 0x00, // ec_point_formats: uncompressed
	}
	hello = binary.BigEndian.AppendUint16(hello, u16(len(ext)))
	hello = append(hello, ext...)

	n := u16(len(hello))
	hs := binary.BigEndian.AppendUint16([]byte{0x01, 0}, n)
	hs = append(hs, hello...)
	record := []byte{0x16, 0x03, 0x01}
	record = binary.BigEndian.AppendUint16(record, u16(len(hs)))
	record = append(record, hs...)
	if _, err := c.Write(record); err != nil {
		t.Fatal(err)
	}
	head := make([]byte, 5)
	if _, err := io.ReadFull(c, head); err != nil {
		t.Fatalf("no answer to the TLS 1.1 hello: %v", err)
	}
	if head[0] == 0x15 {
		body := make([]byte, 2)
		if _, err := io.ReadFull(c, body); err == nil {
			return head[0], body[1]
		}
	}
	return head[0], 0
}

// u16 is n as the two-byte length of a TLS record or field; every length
// here is a few hundred bytes.
func u16(n int) uint16 {
	if n < 0 || n > 0xffff {
		panic("a TLS length out of range")
	}
	return uint16(n) //nolint:gosec // G115: checked just above.
}

// rawTLS opens an HTTP/1.1 connection over TLS to the env's listener.
func rawTLS(t *testing.T, e *env) *tls.Conn {
	t.Helper()
	c, err := tls.Dial("tcp", strings.TrimPrefix(e.netURL, "https://"), &tls.Config{RootCAs: e.roots, MinVersion: tls.VersionTLS12, NextProtos: []string{"http/1.1"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// stalledRequest writes the head of a Connect JSON request for procedure
// that promises a 100-byte body and sends one byte of it, and returns a
// reader of the answer.
func stalledRequest(t *testing.T, c net.Conn, procedure, authorization string) *bufio.Reader {
	t.Helper()
	head := "POST " + procedure + " HTTP/1.1\r\nHost: x\r\nContent-Type: application/json\r\nContent-Length: 100\r\n"
	if authorization != "" {
		head += "Authorization: " + authorization + "\r\n"
	}
	if _, err := io.WriteString(c, head+"\r\n{"); err != nil {
		t.Fatal(err)
	}
	return bufio.NewReader(c)
}

// TestMaxDeadline guards that every call is cut at the maximum, even when its
// client asks for a longer deadline, so no client can hold a request for as
// long as it likes; a client's shorter deadline is kept.
func TestMaxDeadline(t *testing.T) {
	e := newEnv(t, options{noNetwork: true, maxDL: 400 * time.Millisecond})
	cl := e.local()
	ctx, cancel := context.WithTimeout(e.ctx, time.Minute)
	defer cancel()
	start := time.Now()
	result := make(chan error, 1)
	go func() {
		_, err := cl.tenants.ListTenants(ctx, &nopacsv1.ListTenantsRequest{PageToken: "block"})
		result <- err
	}()
	var err error
	select {
	case err = <-result:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("a call asking for a minute was not cut at the maximum; it is still running after 5s")
	}
	took := time.Since(start)
	if code, _ := codeOf(err); code != connect.CodeDeadlineExceeded || took < 350*time.Millisecond || took > 3*time.Second {
		t.Fatalf("a call asking for a minute: %v after %v, want DEADLINE_EXCEEDED at the 400ms maximum", err, took)
	}
	ctx2, cancel2 := context.WithTimeout(e.ctx, 150*time.Millisecond)
	defer cancel2()
	start = time.Now()
	_, _ = cl.tenants.ListTenants(ctx2, &nopacsv1.ListTenantsRequest{PageToken: "block"})
	if took := time.Since(start); took > 350*time.Millisecond {
		t.Fatalf("a client's shorter deadline was not kept: %v", took)
	}
}

// TestClientDeadlineHeaders guards that each protocol's deadline header is
// the client's deadline, so the default is not applied over it: the
// Connect protocol's Connect-Timeout-Ms (with no grpc-timeout) and gRPC's
// grpc-timeout (with no Connect-Timeout-Ms) each run past the default.
func TestClientDeadlineHeaders(t *testing.T) {
	e := newEnv(t, options{noNetwork: true, deadline: 300 * time.Millisecond})
	req, _ := http.NewRequest(http.MethodPost, "http://local"+nopacsv1connect.TenantServiceListTenantsProcedure, strings.NewReader(`{"pageToken":"block"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Connect-Timeout-Ms", "900")
	start := time.Now()
	resp, err := e.socketClient(false).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if took := time.Since(start); took < 800*time.Millisecond {
		t.Fatalf("Connect-Timeout-Ms 900 was cut at %v by the default", took)
	}
	ctx, cancel := context.WithTimeout(e.ctx, 900*time.Millisecond)
	defer cancel()
	start = time.Now()
	_, _ = e.local().tenants.ListTenants(ctx, &nopacsv1.ListTenantsRequest{PageToken: "block"})
	if took := time.Since(start); took < 800*time.Millisecond {
		t.Fatalf("grpc-timeout 900ms was cut at %v by the default", took)
	}
}

// TestBodyReadBound guards against a caller that trickles its body to hold a
// request open before it is checked: a method whose requirement reads the
// request waits for its body only as long as the read bound. A caller with no
// key that trickles a body is refused UNAUTHENTICATED when the bound runs
// out; a caller who passes the fallback check is told its request was
// invalid.
func TestBodyReadBound(t *testing.T) {
	e := newEnv(t, options{bodyRead: 300 * time.Millisecond})
	reader := e.makeKey(nopacsv1.Role_ROLE_READER, installation())
	for _, c := range []struct {
		auth   string
		status int
		word   string
	}{
		{"", http.StatusUnauthorized, "unauthenticated"},
		{"Bearer " + reader, http.StatusBadRequest, "did not arrive in time"},
	} {
		conn := rawTLS(t, e)
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		r := stalledRequest(t, conn, nopacsv1connect.SettingsServiceGetSettingProcedure, c.auth)
		start := time.Now()
		resp, err := http.ReadResponse(r, nil)
		if err != nil {
			t.Fatalf("no answer to a stalled body (key %v): %v", c.auth != "", err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if took := time.Since(start); resp.StatusCode != c.status || !strings.Contains(string(body), c.word) || took > 3*time.Second {
			t.Fatalf("a stalled body (key %v): %d %s after %v, want %d %q", c.auth != "", resp.StatusCode, body, took, c.status, c.word)
		}
	}
}

// TestUnauthenticatedHoldsNothing guards against callers with no key using up
// the server's connections and files until the local operator is locked out:
// callers with no key open many connections that try each way to stay (a
// stalled body on a method that reads its request, Watch, a long deadline on
// a method that does not read), and once the read bound and the maximum have
// passed none of them holds a request in flight, while the local operator is
// answered.
func TestUnauthenticatedHoldsNothing(t *testing.T) {
	e := newEnv(t, options{bodyRead: 300 * time.Millisecond, maxDL: 600 * time.Millisecond})
	var wg sync.WaitGroup
	for range 20 {
		conn := rawTLS(t, e)
		_ = stalledRequest(t, conn, nopacsv1connect.SettingsServiceGetSettingProcedure, "")
		wg.Go(func() {
			cl := newClients(e.tlsClient(true), e.netURL, true, "")
			ctx, cancel := context.WithTimeout(e.ctx, time.Hour)
			defer cancel()
			st, err := cl.raw.CallServerStream(ctx, connect.Spec{StreamType: connect.StreamTypeServer, Procedure: server.HealthWatchProcedure}, &nopacsv1.WhoAmIRequest{})
			if err == nil {
				var m healthStatus
				_ = st.Receive(&m)
				_ = st.Close()
			}
			_, _ = cl.tenants.ListTenants(ctx, &nopacsv1.ListTenantsRequest{PageToken: "block"})
		})
	}
	wg.Wait()
	waitFor(t, func() bool { return strings.Contains(metricsText(e), "nopacs_requests_in_flight 0\n") })
	if _, err := e.local().access.WhoAmI(e.ctx, &nopacsv1.WhoAmIRequest{}); err != nil {
		t.Fatalf("the local operator was not answered: %v", err)
	}
}

// TestUnauthenticatedConnectionClosed guards that an HTTP/1.1 connection
// whose request was never authenticated (a refusal, the public health
// check) is closed after its answer, so a caller with no key cannot keep
// idle connections; an authenticated caller's connection is kept.
func TestUnauthenticatedConnectionClosed(t *testing.T) {
	e := newEnv(t, options{})
	admin := e.makeKey(nopacsv1.Role_ROLE_ADMIN, installation())
	for _, c := range []struct {
		path, auth string
		closed     bool
	}{
		{nopacsv1connect.AccessServiceWhoAmIProcedure, "", true},
		{server.HealthCheckProcedure, "", true},
		{nopacsv1connect.AccessServiceWhoAmIProcedure, "Bearer junk", true},
		{nopacsv1connect.AccessServiceWhoAmIProcedure, "Bearer " + admin, false},
	} {
		conn := rawTLS(t, e)
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		head := "POST " + c.path + " HTTP/1.1\r\nHost: x\r\nContent-Type: application/json\r\nContent-Length: 2\r\n"
		if c.auth != "" {
			head += "Authorization: " + c.auth + "\r\n"
		}
		if _, err := io.WriteString(conn, head+"\r\n{}"); err != nil {
			t.Fatal(err)
		}
		r := bufio.NewReader(conn)
		resp, err := http.ReadResponse(r, nil)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		// The server closes a connection it marked: a further read ends.
		_ = conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		_, rerr := r.ReadByte()
		gotClosed := errors.Is(rerr, io.EOF)
		if resp.Close != c.closed || gotClosed != c.closed {
			t.Fatalf("%s (key %v): %d, Connection: close %v, closed by the server %v; want %v", c.path, c.auth != "", resp.StatusCode, resp.Close, gotClosed, c.closed)
		}
	}
}

// TestStopAbandonsStuck guards that a handler that ignores its context does
// not hold Stop, which would otherwise keep the server from ever stopping.
// After the drain and the grace Stop gives up on it, closes the rest,
// releases the lock, and says how many it abandoned.
func TestStopAbandonsStuck(t *testing.T) {
	e := newEnv(t, options{noNetwork: true, drain: 200 * time.Millisecond, grace: 200 * time.Millisecond, closeT: time.Second})
	e.stopErrOK = true
	cl := e.local()
	go func() { _, _ = cl.tenants.ListTenants(e.ctx, &nopacsv1.ListTenantsRequest{PageToken: "stuck"}) }()
	waitFor(t, func() bool { return strings.Contains(metricsText(e), "nopacs_requests_in_flight 1\n") })
	start := time.Now()
	stopped := make(chan error, 1)
	go func() { stopped <- e.srv.Stop() }()
	var err error
	select {
	case err = <-stopped:
	case <-time.After(5 * time.Second):
		e.release()
		t.Fatalf("stop did not return with a handler that ignores its context (it returned %v once the handler was released)", <-stopped)
	}
	took := time.Since(start)
	if err == nil || !strings.Contains(err.Error(), "1 requests did not end") {
		t.Fatalf("stop with a stuck handler: %v", err)
	}
	if took > 3*time.Second {
		t.Fatalf("stop took %v", took)
	}
	l, err := server.TakeLock(e.dir)
	if err != nil {
		t.Fatalf("the lock was not released: %v", err)
	}
	_ = l.Release()
}

// TestLateRegistrationNotServed guards that no method reaches a handler
// without its requirement: a method registered on the server's connect.Server
// after New, under a service already mounted, is checked and answered
// UNIMPLEMENTED; its handler is never called.
func TestLateRegistrationNotServed(t *testing.T) {
	var rpc *connect.Server
	ran := false
	e := newEnv(t, options{noNetwork: true, register: func(s *connect.Server) { rpc = s }, beforeRun: func(*server.Server) {
		rpc.Register(connect.Method{
			Spec: connect.Spec{StreamType: connect.StreamTypeUnary, Procedure: "/" + nopacsv1connect.AccessServiceName + "/Late"},
			Handler: func(context.Context, connect.Spec, connect.ServerStream) error {
				ran = true
				return nil
			},
		})
	}})
	err := e.local().raw.CallUnary(e.ctx, connect.Spec{StreamType: connect.StreamTypeUnary, Procedure: "/" + nopacsv1connect.AccessServiceName + "/Late"}, &emptyMsg{}, &emptyMsg{})
	if code, _ := codeOf(err); code != connect.CodeUnimplemented || ran {
		t.Fatalf("a late registration: %v, handler ran: %v", err, ran)
	}
}

// TestPanicValueRedacted guards that a panic cannot carry a secret into the
// log: a panic's value reaches the log only as its type, a scrubbed short
// text, or a redacted message. A panic with an address holding a password,
// with a request message holding it, and with a value of another type holding
// it, leave no password in the log.
func TestPanicValueRedacted(t *testing.T) {
	e := newEnv(t, options{noNetwork: true})
	cl := e.local()
	for _, token := range []string{"panic-secret", "panic-proto", "panic-struct"} {
		if _, err := cl.tenants.ListTenants(e.ctx, &nopacsv1.ListTenantsRequest{PageToken: token}); err == nil {
			t.Fatalf("%s did not fail", token)
		}
	}
	text := e.log.String()
	if strings.Contains(text, "pw-in-a-panic") {
		t.Fatalf("a panic put a password in the log:\n%s", text)
	}
	for _, want := range []string{`"panic_type":"string"`, `"panic_type":"*nopacsv1.CreateTenantRequest"`, `"panic_type":"servertest.panicHolder"`, "postgres://[hidden]@db.example"} {
		if !strings.Contains(text, want) {
			t.Fatalf("the panic log lacks %s:\n%s", want, text)
		}
	}
}

// TestAuditWrittenAtStop guards that the audit entries queued when Stop
// begins are written: with an audit interval of an hour nothing is written
// before Stop, so every refusal's entry found in the master afterwards was
// written by the access package's close, which must come before the
// master's.
func TestAuditWrittenAtStop(t *testing.T) {
	const refusals = 50
	e := newEnv(t, options{noNetwork: true, auditWait: time.Hour})
	bad := newClients(e.socketClient(true), "http://local", true, "Bearer "+unknownKey)
	for range refusals {
		_, _ = bad.access.WhoAmI(e.ctx, &nopacsv1.WhoAmIRequest{})
	}
	if err := e.srv.Stop(); err != nil {
		t.Fatal(err)
	}
	m, err := sqlite.OpenMaster(context.Background(), filepath.Join(e.dir, "master.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = m.Close() }()
	entries, _, err := m.ListAudit(context.Background(), master.AuditFilter{Outcome: master.OutcomeRefused}, master.Page{Limit: master.MaxPageLimit})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != refusals {
		t.Fatalf("%d refusals queued at stop, %d written", refusals, len(entries))
	}
}

// orderRegistry is a tenant registry that records its Close in order.
type orderRegistry struct{ order *[]string }

func (r orderRegistry) Close(context.Context) error {
	*r.order = append(*r.order, "registry")
	return nil
}

// TestAfterDrainRunsAfterTheAuditFlush guards the step after the drain
// (the audit's last seal): it runs once, after the access package has
// written the audit entries it held (with an audit interval of an hour,
// none is written before Stop, so the hook finds them only if the flush
// came first), and before the registry and the master close.
func TestAfterDrainRunsAfterTheAuditFlush(t *testing.T) {
	const refusals = 20
	var order []string
	var dir string
	seen := -1
	e := newEnv(t, options{noNetwork: true, auditWait: time.Hour, registry: orderRegistry{order: &order},
		afterDrain: func(ctx context.Context) error {
			order = append(order, "after-drain")
			m, err := sqlite.OpenMaster(ctx, filepath.Join(dir, "master.db"))
			if err != nil {
				return err
			}
			defer func() { _ = m.Close() }()
			entries, _, err := m.ListAudit(ctx, master.AuditFilter{Outcome: master.OutcomeRefused}, master.Page{Limit: master.MaxPageLimit})
			seen = len(entries)
			return err
		}})
	dir = e.dir
	bad := newClients(e.socketClient(true), "http://local", true, "Bearer "+unknownKey)
	for range refusals {
		_, _ = bad.access.WhoAmI(e.ctx, &nopacsv1.WhoAmIRequest{})
	}
	if err := e.srv.Stop(); err != nil {
		t.Fatal(err)
	}
	if strings.Join(order, ",") != "after-drain,registry" {
		t.Fatalf("stop order %v, want the step after the drain once, then the registry", order)
	}
	if seen != refusals {
		t.Fatalf("the step after the drain saw %d refusals' entries, want the %d the audit writer held", seen, refusals)
	}
}

// stuckRegistry is a tenant registry whose Close never returns, whatever
// its context says, until the test releases it.
type stuckRegistry struct{ release chan struct{} }

func (r stuckRegistry) Close(context.Context) error {
	<-r.release
	return nil
}

// TestStopAbandonsWhatDoesNotClose guards the close deadline: a part that
// does not return from Close within it (here the tenant registry, which
// ignores its context) is given up on, Stop says so, the parts after it
// are still closed, the lock on the data directory is released, and the
// whole of Stop is bounded by the deadline rather than by the part.
func TestStopAbandonsWhatDoesNotClose(t *testing.T) {
	reg := stuckRegistry{release: make(chan struct{})}
	defer close(reg.release)
	const closeT = 300 * time.Millisecond
	e := newEnv(t, options{noNetwork: true, drain: 100 * time.Millisecond, grace: 100 * time.Millisecond, closeT: closeT, registry: reg})
	e.stopErrOK = true
	began := time.Now()
	err := e.srv.Stop()
	took := time.Since(began)
	if err == nil || !strings.Contains(err.Error(), "closing the tenant registry") ||
		!strings.Contains(err.Error(), "did not return within its close deadline and was abandoned") {
		t.Fatalf("Stop with a registry that does not close: %v", err)
	}
	// Drain, grace and three close deadlines with their quarter of slack,
	// and room for a loaded machine; the stuck part alone would never end.
	if bound := 10 * time.Second; took > bound {
		t.Fatalf("Stop took %v with a part that does not close, past %v", took, bound)
	}
	lock, err := server.TakeLock(e.dir)
	if err != nil {
		t.Fatalf("the lock was not released after the part was given up on: %v", err)
	}
	_ = lock.Release()
}

// TestRequestDuringStop guards what a request meets while Stop drains: any
// request is UNAVAILABLE, except the health service's Check at its exact
// path (not percent-encoded, not in another case, not with a trailing
// slash), which answers NOT_SERVING; and when Stop is done the lock is
// released.
func TestRequestDuringStop(t *testing.T) {
	e := newEnv(t, options{noNetwork: true, delay: 800 * time.Millisecond, drain: 5 * time.Second})
	cl := e.local()
	go func() { _, _ = cl.tenants.ListTenants(e.ctx, &nopacsv1.ListTenantsRequest{PageToken: "wait"}) }()
	waitFor(t, func() bool { return strings.Contains(metricsText(e), "nopacs_requests_in_flight 1\n") })
	stopped := make(chan error, 1)
	go func() { stopped <- e.srv.Stop() }()
	call := func(path string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, path, strings.NewReader("{}"))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		e.srv.Handler().ServeHTTP(w, r)
		return w
	}
	waitFor(t, func() bool {
		return call(nopacsv1connect.AccessServiceWhoAmIProcedure).Code == http.StatusServiceUnavailable
	})
	w := call(nopacsv1connect.AccessServiceWhoAmIProcedure)
	if !strings.Contains(w.Body.String(), "unavailable") || w.Header().Get(server.RequestIDHeader) == "" {
		t.Fatalf("a request during stop: %d %s", w.Code, w.Body.String())
	}
	if h := call(server.HealthCheckProcedure); h.Code != http.StatusOK || !strings.Contains(h.Body.String(), "NOT_SERVING") {
		t.Fatalf("health during stop: %d %s", h.Code, h.Body.String())
	}
	// Only the health check's exact path is let in: not another spelling of
	// it, which the server does not serve as the health check.
	for _, p := range []string{"/grpc.health.v1.Health%2FCheck", "/GRPC.HEALTH.V1.HEALTH/CHECK", "/grpc.health.v1.Health/Check/", server.HealthWatchProcedure} {
		if h := call(p); h.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s during stop: %d %s, want the stopping refusal", p, h.Code, h.Body.String())
		}
	}
	if err := <-stopped; err != nil {
		t.Fatal(err)
	}
	l, err := server.TakeLock(e.dir)
	if err != nil {
		t.Fatalf("the lock was not released at stop: %v", err)
	}
	_ = l.Release()
}

// TestStreamFirstMessageOnce guards the replay of the message the check
// read: on a client stream whose requirement reads the request, the handler
// gets every message once and the first not twice.
func TestStreamFirstMessageOnce(t *testing.T) {
	const proc = "/nopacs.test.Stream/Collect"
	methods := servertest.Contract()
	methods[proc] = servertest.Entry{
		NewMsg: func() proto.Message { return &nopacsv1.GetTenantRequest{} },
		Need:   func(proto.Message) access.Requirement { return access.Authenticated() },
	}
	e := newEnv(t, options{noNetwork: true, methods: methods, register: func(s *connect.Server) {
		s.Register(connect.Method{
			Spec: connect.Spec{StreamType: connect.StreamTypeClient, Procedure: proc},
			Handler: func(_ context.Context, _ connect.Spec, st connect.ServerStream) error {
				var names []string
				for {
					var m nopacsv1.GetTenantRequest
					if err := st.Receive(&m); err != nil {
						if errors.Is(err, io.EOF) {
							break
						}
						return err
					}
					names = append(names, m.GetName())
				}
				return st.Send(&nopacsv1.GetTenantResponse{Tenant: &nopacsv1.Tenant{Name: strings.Join(names, ",")}})
			},
		})
	}})
	st, err := e.local().raw.CallClientStream(e.ctx, connect.Spec{StreamType: connect.StreamTypeClient, Procedure: proc})
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"m1", "m2", "m3"} {
		if err := st.Send(&nopacsv1.GetTenantRequest{Name: n}); err != nil {
			t.Fatal(err)
		}
	}
	_ = st.CloseSend()
	var out nopacsv1.GetTenantResponse
	if err := st.Receive(&out); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	if got := out.GetTenant().GetName(); got != "m1,m2,m3" {
		t.Fatalf("the handler received %q", got)
	}
}

// TestWwwAuthenticate guards RFC 9110 15.5.2 on both kinds of route: a 401
// names the Bearer scheme.
func TestWwwAuthenticate(t *testing.T) {
	e := newEnv(t, options{})
	for _, r := range []struct{ method, path string }{
		{http.MethodPost, nopacsv1connect.AccessServiceWhoAmIProcedure},
		{http.MethodGet, "/metrics"},
	} {
		req, _ := http.NewRequest(r.method, e.netURL+r.path, strings.NewReader("{}"))
		req.Header.Set("Content-Type", "application/json")
		resp, err := e.tlsClient(false).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized || resp.Header.Get("Www-Authenticate") != "Bearer" {
			t.Fatalf("%s %s: %d, Www-Authenticate %q", r.method, r.path, resp.StatusCode, resp.Header.Get("Www-Authenticate"))
		}
	}
}

// TestCatchAllNever200 guards that a path no route serves is never
// answered 200, even for the local operator.
func TestCatchAllNever200(t *testing.T) {
	e := newEnv(t, options{noNetwork: true})
	hc := e.socketClient(false)
	for _, p := range []string{"/", "/x", "/nopacs.v1.AccessService", "/a/b/c", "/metrics/x", "/grpc.health.v1.Health"} {
		for _, m := range []string{http.MethodGet, http.MethodPost} {
			req, _ := http.NewRequest(m, "http://local"+p, strings.NewReader("{}"))
			resp, err := hc.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				t.Fatalf("%s %s answered 200", m, p)
			}
		}
	}
}

// TestUnknownMethodLabels guards the metrics' series: a method nobody
// registered is counted as "unknown", whether its caller is allowed or
// refused, and its path never becomes a label, so callers cannot make series
// without bound; and its path is cut in the log.
func TestUnknownMethodLabels(t *testing.T) {
	e := newEnv(t, options{noNetwork: true})
	long := strings.Repeat("x", 5000)
	refused := newClients(e.socketClient(true), "http://local", true, "Bearer "+unknownKey)
	for _, p := range []string{"/nopacs.v1.AccessService/NoSuchOne", "/nopacs.v1.AccessService/" + long, "/no.such.Service/Call"} {
		_ = e.local().raw.CallUnary(e.ctx, connect.Spec{StreamType: connect.StreamTypeUnary, Procedure: p}, &emptyMsg{}, &emptyMsg{})
		_ = refused.raw.CallUnary(e.ctx, connect.Spec{StreamType: connect.StreamTypeUnary, Procedure: p}, &emptyMsg{}, &emptyMsg{})
	}
	text := metricsText(e)
	if strings.Contains(text, "NoSuchOne") || strings.Contains(text, "xxxx") || strings.Contains(text, "no.such.Service") {
		t.Fatalf("a request's path became a metric label:\n%s", text)
	}
	if !strings.Contains(text, `method="unknown"`) {
		t.Fatalf("the unknown method was not counted as unknown:\n%s", text)
	}
	if strings.Contains(e.log.String(), strings.Repeat("x", 300)) {
		t.Fatal("a 5000-byte path was written whole in the log")
	}
}

// TestTransportAnswersLogged guards that no answer escapes the log, so an
// operator can trace every one: answers the RPC library gives before the
// request path (a wrong content type, a wrong HTTP method, an unknown
// compression, a malformed timeout header) each carry a request id that is in
// the log, and each is counted under the method "transport".
func TestTransportAnswersLogged(t *testing.T) {
	e := newEnv(t, options{noNetwork: true})
	hc := e.socketClient(false)
	proc := nopacsv1connect.AccessServiceWhoAmIProcedure
	for _, c := range []struct {
		name, method string
		header       map[string]string
	}{
		{"content type", http.MethodPost, map[string]string{"Content-Type": "text/plain"}},
		{"http method", http.MethodPut, map[string]string{"Content-Type": "application/json"}},
		{"compression", http.MethodPost, map[string]string{"Content-Type": "application/json", "Content-Encoding": "br"}},
		{"timeout header", http.MethodPost, map[string]string{"Content-Type": "application/json", "Connect-Timeout-Ms": "soon"}},
	} {
		req, _ := http.NewRequest(c.method, "http://local"+proc, strings.NewReader("{}"))
		for k, v := range c.header {
			req.Header.Set(k, v)
		}
		resp, err := hc.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		id := resp.Header.Get(server.RequestIDHeader)
		if resp.StatusCode == http.StatusOK || id == "" || !strings.Contains(e.log.String(), `"request_id":"`+id+`"`) {
			t.Fatalf("%s: %d, request id %q not in the log:\n%s", c.name, resp.StatusCode, id, e.log.String())
		}
		want := fmt.Sprintf(`nopacs_requests_total{method="transport",code="http_%d"}`, resp.StatusCode)
		if !strings.Contains(metricsText(e), want) {
			t.Fatalf("%s: not counted as %s", c.name, want)
		}
	}
}

// TestNothingLoggedBeforeCheck guards that nothing from a request is logged
// before the check has allowed it, even at debug level: a refused request's
// message is not in the log, and the same message from the local operator
// is, once allowed.
func TestNothingLoggedBeforeCheck(t *testing.T) {
	e := newEnv(t, options{level: -4})
	const marker = "marker-before-check"
	req := &nopacsv1.GetSettingRequest{Name: marker, Scope: installation()}
	_, _ = e.network("").settings.GetSetting(e.ctx, req)
	if strings.Contains(e.log.String(), marker) {
		t.Fatal("a refused request's message was logged")
	}
	_, _ = e.local().settings.GetSetting(e.ctx, req)
	if !strings.Contains(e.log.String(), marker) {
		t.Fatal("an allowed request's message was not logged at debug level, so the first search proves nothing")
	}
}

// TestJSONSizeLimit guards the send limit per encoding: an answer under 4
// MiB in protobuf and over it in JSON is sent in protobuf and refused in
// JSON.
func TestJSONSizeLimit(t *testing.T) {
	e := newEnv(t, options{noNetwork: true})
	req := &nopacsv1.ListTenantsRequest{PageToken: "huge-json"}
	if _, err := e.local().tenants.ListTenants(e.ctx, req); err != nil {
		t.Fatalf("in protobuf: %v", err)
	}
	cl := newClients(e.socketClient(false), "http://local", false, "")
	_, err := cl.tenants.ListTenants(e.ctx, req)
	if code, _ := codeOf(err); code != connect.CodeResourceExhausted {
		t.Fatalf("in JSON: %v", err)
	}
}

// TestScrapeNotAudited guards that an allowed scrape of /metrics writes no
// audit entry and a refused one does.
func TestScrapeNotAudited(t *testing.T) {
	e := newEnv(t, options{})
	get := func(hc *http.Client, base string) {
		resp, err := hc.Get(base + "/metrics")
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	}
	count := func() int {
		if err := e.access.FlushAudit(e.ctx); err != nil {
			t.Fatal(err)
		}
		entries, _, err := e.master.ListAudit(e.ctx, master.AuditFilter{}, master.Page{Limit: master.MaxPageLimit})
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, en := range entries {
			if en.Method == server.MetricsPattern {
				n++
			}
		}
		return n
	}
	for range 3 {
		get(e.socketClient(false), "http://local")
	}
	if n := count(); n != 0 {
		t.Fatalf("allowed scrapes wrote %d audit entries", n)
	}
	get(e.tlsClient(false), e.netURL)
	if n := count(); n != 1 {
		t.Fatalf("a refused scrape wrote %d audit entries, want 1", n)
	}
}

// TestListenErrorHasCause guards that a listener that cannot open says why.
func TestListenErrorHasCause(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = busy.Close() }()
	a, err := access.New(access.Config{Store: nilStore{}, Clock: time.Now, NewID: server.NewID})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Close(context.Background()) }()
	srv, err := server.New(server.Config{NoSocket: true, Access: a, Register: func(*connect.Server) {}, Listen: busy.Addr().String()})
	if err != nil {
		t.Fatal(err)
	}
	err = srv.Start()
	if err == nil {
		_ = srv.Stop()
		t.Fatal("a busy address was listened on")
	}
	if msg := err.Error(); !strings.Contains(msg, "listen tcp "+busy.Addr().String()) {
		t.Fatalf("the error does not give the cause: %v", msg)
	}
}
