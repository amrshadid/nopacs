package server_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"

	nopacsv1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/gen/nopacs/v1/nopacsv1connect"
	"github.com/amrshadid/nopacs/internal/access"
	"github.com/amrshadid/nopacs/internal/core/settings"
	"github.com/amrshadid/nopacs/internal/driver/sqlite"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/obs"
	"github.com/amrshadid/nopacs/internal/server"
	"github.com/amrshadid/nopacs/internal/testkit/servertest"
)

// syncBuffer is a bytes.Buffer safe for the server's goroutines to write
// and a test to read.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

// Write appends p.
func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// String returns what was written.
func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// options shape one test server.
type options struct {
	format    obs.Format
	level     slog.Level
	deadline  time.Duration
	maxDL     time.Duration
	bodyRead  time.Duration
	drain     time.Duration
	grace     time.Duration
	closeT    time.Duration
	delay     time.Duration
	auditWait time.Duration
	// refusalQueue replaces the access package's queue of refusals' audit
	// entries, for a test that sends more refusals at once than it holds.
	refusalQueue int
	// store wraps the master as the access package sees it, for a test
	// that counts what the check reads.
	store func(access.Store) access.Store
	// clock replaces the access package's clock, for a test that moves
	// time past a key's expiry.
	clock     func() time.Time
	register  func(*connect.Server)
	methods   servertest.Table
	noNetwork bool
	beforeRun func(*server.Server)
	// registry is given to the server as its tenant registry, for a test of
	// what Stop does with it; nil gives none.
	registry server.Registry
	// afterDrain is the server's step after the drain and the audit flush
	// (server.Config.AfterDrain); nil gives none.
	afterDrain func(context.Context) error
	// patterns replaces the pattern routes every test server has (the
	// stand-in download, testPatterns).
	patterns []server.PatternRoute
	// corsOrigins is the server's reading of the allowed CORS origins;
	// nil allows none.
	corsOrigins func(context.Context) (settings.Origins, error)
	// accessRules is the server's reading of the installation's access
	// settings; nil applies the defaults.
	accessRules func(context.Context) (server.AccessRules, error)
	// events receives the access package's events; nil discards them.
	events access.Events
	// longMethods are the server's calls without a deadline, and stall
	// replaces its write stall bound (zero keeps it).
	longMethods []string
	stall       time.Duration
}

// env is one running server over a fresh SQLite master, with stand-in
// handlers, the local socket and a TLS listener on loopback.
type env struct {
	t        testing.TB
	ctx      context.Context
	dir      string
	master   master.Store
	access   *access.Access
	standins *servertest.Standins
	srv      *server.Server
	log      *syncBuffer
	roots    *x509.CertPool
	netURL   string
	// clientsMu guards clients: a test may make clients from several
	// goroutines at once, and each is kept here to be closed at the end.
	clientsMu sync.Mutex
	clients   []*http.Client
	// stopErrOK is set by a test whose stop is meant to fail.
	stopErrOK   bool
	releaseOnce sync.Once
}

// shortTempDir returns a new private directory under the system's
// temporary directory, short enough for a socket path (t.TempDir names the
// test, which can push the path past the 103 bytes a socket allows).
func shortTempDir(tb testing.TB) string {
	tb.Helper()
	dir, err := os.MkdirTemp("", "mgs")
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // G302: a directory, private to its owner.
		tb.Fatal(err)
	}
	return dir
}

// newEnv starts a server for tb and stops it when tb ends.
func newEnv(tb testing.TB, o options) *env {
	tb.Helper()
	ctx := context.Background()
	e := &env{t: tb, ctx: ctx, dir: shortTempDir(tb), log: &syncBuffer{}}
	m, err := sqlite.OpenMaster(ctx, filepath.Join(e.dir, "master.db"))
	if err != nil {
		tb.Fatal(err)
	}
	if err := m.Migrate(ctx); err != nil {
		tb.Fatal(err)
	}
	e.master = m
	if o.format == "" {
		o.format = obs.JSON
	}
	log := obs.NewLogger(e.log, obs.LogOptions{Format: o.format, Level: o.level})
	var st access.Store = m
	if o.store != nil {
		st = o.store(m)
	}
	clock := time.Now
	if o.clock != nil {
		clock = o.clock
	}
	a, err := access.New(access.Config{Store: st, Clock: clock, NewID: server.NewID, Log: log, AuditInterval: o.auditWait, RefusalQueue: o.refusalQueue, Events: o.events})
	if err != nil {
		tb.Fatal(err)
	}
	e.access = a
	e.standins = &servertest.Standins{Access: a, Delay: o.delay, Release: make(chan struct{})}
	// Registered before the server's stop, so it runs after it: a "stuck"
	// handler the stop gave up on returns once released, before the leak
	// check.
	tb.Cleanup(e.release)
	register := e.standins.Register
	if o.register != nil {
		register = func(s *connect.Server) {
			e.standins.Register(s)
			o.register(s)
		}
	}
	methods := o.methods
	if methods == nil {
		methods = servertest.Contract()
	}
	lock, err := server.TakeLock(e.dir)
	if err != nil {
		tb.Fatal(err)
	}
	cfg := server.Config{
		DataDir: e.dir, Lock: lock, Access: a, Master: m, Log: log,
		Register: register, DefaultDeadline: o.deadline, Drain: o.drain,
		MaxDeadline: o.maxDL, BodyReadTimeout: o.bodyRead, Grace: o.grace, Close: o.closeT,
		Methods: methods, WithDecision: withDecision, Registry: o.registry, AfterDrain: o.afterDrain,
		PatternRoutes: o.patterns, LongMethods: o.longMethods, CORSOrigins: o.corsOrigins, AccessRules: o.accessRules,
	}
	if cfg.PatternRoutes == nil {
		cfg.PatternRoutes = testPatterns()
	}
	if !o.noNetwork {
		certFile, keyFile, roots := testCert(tb, e.dir)
		cfg.Listen, cfg.TLSCert, cfg.TLSKey = "127.0.0.1:0", certFile, keyFile
		e.roots = roots
	}
	srv, err := server.New(cfg)
	if err != nil {
		tb.Fatal(err)
	}
	if o.stall > 0 {
		srv.SetStallTimeout(o.stall)
	}
	if o.beforeRun != nil {
		o.beforeRun(srv)
	}
	if err := srv.Start(); err != nil {
		tb.Fatal(err)
	}
	e.srv = srv
	if a := srv.Addr(); a != nil {
		e.netURL = "https://" + a.String()
	}
	tb.Cleanup(func() {
		e.closeClients()
		if err := srv.Stop(); err != nil && !e.stopErrOK {
			tb.Errorf("stop: %v", err)
		}
	})
	return e
}

// testCert writes a self-signed certificate for 127.0.0.1 and its key into
// dir, and returns their paths and a pool that trusts the certificate.
func testCert(tb testing.TB, dir string) (certFile, keyFile string, roots *x509.CertPool) {
	tb.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		tb.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "nopacs test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		tb.Fatal(err)
	}
	kder, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		tb.Fatal(err)
	}
	certFile, keyFile = filepath.Join(dir, "cert.pem"), filepath.Join(dir, "cert-key.pem")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		tb.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder}), 0o600); err != nil {
		tb.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		tb.Fatal(err)
	}
	roots = x509.NewCertPool()
	roots.AddCert(cert)
	return certFile, keyFile, roots
}

// socketClient returns an HTTP client over the local socket: HTTP/2 without
// TLS when h2 is set (as gRPC clients speak), HTTP/1.1 otherwise.
func (e *env) socketClient(h2 bool) *http.Client {
	var p http.Protocols
	if h2 {
		p.SetUnencryptedHTTP2(true)
	} else {
		p.SetHTTP1(true)
	}
	path := server.SocketPath(e.dir)
	c := &http.Client{Transport: &http.Transport{
		Protocols: &p,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", path)
		},
	}}
	e.track(c)
	return c
}

// track keeps a client to be closed when the test ends. It may be called from
// several goroutines.
func (e *env) track(c *http.Client) {
	e.clientsMu.Lock()
	defer e.clientsMu.Unlock()
	e.clients = append(e.clients, c)
}

// closeClients closes the idle connections of every client the test made.
func (e *env) closeClients() {
	e.clientsMu.Lock()
	defer e.clientsMu.Unlock()
	for _, c := range e.clients {
		c.CloseIdleConnections()
	}
}

// tlsClient returns an HTTP client over the TLS listener: HTTP/2 when h2 is
// set, HTTP/1.1 otherwise.
func (e *env) tlsClient(h2 bool) *http.Client {
	var p http.Protocols
	if h2 {
		p.SetHTTP2(true)
	} else {
		p.SetHTTP1(true)
	}
	c := &http.Client{Transport: &http.Transport{
		Protocols:       &p,
		TLSClientConfig: &tls.Config{RootCAs: e.roots, MinVersion: tls.VersionTLS12},
	}}
	e.track(c)
	return c
}

// bearer is a client interceptor that sends a key, or a raw Authorization
// value, with every call.
func bearer(value string) connect.ClientInterceptor {
	return func(next connect.ClientFunc) connect.ClientFunc {
		return func(ctx context.Context, spec connect.Spec) (connect.ClientStream, error) {
			if value != "" {
				if info, ok := connect.CallInfoForClientContext(ctx); ok {
					info.RequestHeader().Set("Authorization", value)
				}
			}
			return next(ctx, spec)
		}
	}
}

// clients are the generated clients of the four services over one
// transport.
type clients struct {
	access   nopacsv1connect.AccessServiceClient
	tenants  nopacsv1connect.TenantServiceClient
	settings nopacsv1connect.SettingsServiceClient
	raw      *connect.Client
}

// newClients returns clients over hc to base, speaking gRPC when grpc is
// set (the Connect protocol with JSON otherwise), sending authorization.
func newClients(hc *http.Client, base string, grpc bool, authorization string) clients {
	opts := []connecthttp.Option{connecthttp.WithReadMaxBytes(16 << 20)}
	if grpc {
		opts = append(opts, connecthttp.WithGRPC())
	} else {
		opts = append(opts, connecthttp.WithProtoJSON())
	}
	c := connect.NewClient(connecthttp.NewTransport(hc, base, opts...), bearer(authorization))
	return clients{
		access:   nopacsv1connect.NewAccessServiceClient(c),
		tenants:  nopacsv1connect.NewTenantServiceClient(c),
		settings: nopacsv1connect.NewSettingsServiceClient(c),
		raw:      c,
	}
}

// local returns gRPC clients over the local socket, as the local operator.
func (e *env) local() clients { return newClients(e.socketClient(true), "http://local", true, "") }

// network returns gRPC clients over TLS sending authorization.
func (e *env) network(authorization string) clients {
	return newClients(e.tlsClient(true), e.netURL, true, authorization)
}

// makeKey makes a key as the local operator and, when role is set, grants
// it that role on scope; it returns the whole key.
func (e *env) makeKey(role nopacsv1.Role, scope *nopacsv1.Scope) string {
	e.t.Helper()
	res, err := e.local().access.CreateKey(e.ctx, &nopacsv1.CreateKeyRequest{Holder: "test", Expiry: &nopacsv1.CreateKeyRequest_NeverExpire{NeverExpire: true}})
	if err != nil {
		e.t.Fatal(err)
	}
	if role != nopacsv1.Role_ROLE_UNSPECIFIED {
		if _, err := e.local().access.SetGrant(e.ctx, &nopacsv1.SetGrantRequest{KeyId: res.GetKey().GetId(), Scope: scope, Role: role}); err != nil {
			e.t.Fatal(err)
		}
	}
	return res.GetSecret()
}

// installation is the installation scope.
func installation() *nopacsv1.Scope {
	return &nopacsv1.Scope{Kind: &nopacsv1.Scope_Installation{Installation: &nopacsv1.Installation{}}}
}

// codeOf returns an error's API code and message.
func codeOf(err error) (connect.Code, string) {
	var ce *connect.Error
	if errors.As(err, &ce) {
		return ce.Code(), ce.Message()
	}
	if err == nil {
		return 0, ""
	}
	return connect.CodeUnknown, err.Error()
}

// withInfo returns a context whose CallInfo the client fills, so a test
// can read the response headers.
func withInfo(ctx context.Context) (context.Context, *connect.CallInfo) {
	return connect.NewClientContext(ctx)
}

// decisionKey is the context key withDecision uses, standing in for
// package internal/api's accessor.
type decisionKey struct{}

// withDecision is the server.WithDecision the test servers use.
func withDecision(ctx context.Context, d access.Decision) context.Context {
	return context.WithValue(ctx, decisionKey{}, d)
}

// decisionFrom reads what withDecision put in ctx.
func decisionFrom(ctx context.Context) (access.Decision, bool) {
	d, ok := ctx.Value(decisionKey{}).(access.Decision)
	return d, ok
}

// release ends the stand-ins' "stuck" requests, once.
func (e *env) release() { e.releaseOnce.Do(func() { close(e.standins.Release) }) }

// contains reports whether s holds sub; a short name for log searches.
func contains(s, sub string) bool { return strings.Contains(s, sub) }
