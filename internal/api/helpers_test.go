package api_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	connect "connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"google.golang.org/protobuf/proto"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/gen/nopacs/v1/nopacsv1connect"
	"github.com/amrshadid/nopacs/internal/access"
	"github.com/amrshadid/nopacs/internal/api"
	"github.com/amrshadid/nopacs/internal/api/codes"
	"github.com/amrshadid/nopacs/internal/core/settings"
	"github.com/amrshadid/nopacs/internal/correct"
	"github.com/amrshadid/nopacs/internal/driver"
	"github.com/amrshadid/nopacs/internal/events"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/place"
	"github.com/amrshadid/nopacs/internal/receive"
	"github.com/amrshadid/nopacs/internal/registry"
	"github.com/amrshadid/nopacs/internal/secret"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// TestMain runs the package's tests, then fails if goroutines are left
// running that were not before: this package starts none, and every
// goroutine of the packages below it has an owner that a test closes.
func TestMain(m *testing.M) {
	before := runtime.NumGoroutine()
	code := m.Run()
	if code == 0 {
		deadline := time.Now().Add(5 * time.Second)
		for runtime.NumGoroutine() > before && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if after := runtime.NumGoroutine(); after > before {
			buf := make([]byte, 1<<20)
			n := runtime.Stack(buf, true)
			fmt.Fprintf(os.Stderr, "goroutine leak: %d before the tests, %d after\n%s\n", before, after, buf[:n])
			code = 1
		}
	}
	os.Exit(code)
}

// t0 is the start of the test clock.
var t0 = time.Date(2026, 11, 18, 9, 30, 0, 0, time.UTC)

// localHeader is the request header by which a test client says it is the
// local operator, with that user id. The real server learns this from its
// local socket; the stand-in reads a header.
const localHeader = "X-Test-Local-User"

// pgAddress is a made-up address for a tenant of the test's Postgres
// driver, which keeps the tenant in a SQLite file and opens nothing at it.
const pgAddress = "postgres://u:p@h:5432/d" //nolint:gosec // G101: made up; it reaches no database.

// syncBuffer is a buffer the logger writes to from several goroutines.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

// Write appends p.
func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

// String returns everything written.
func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// fakePostgres is a registry driver registered as "postgres": it takes an
// address, as a Postgres driver would, and keeps the tenant's data in a
// SQLite file. A tenant set down fails its open and its pings as a database
// that does not answer. Its error keeps the registry's driver contract (the
// message, fault.Message, is safe for the caller) while its cause repeats the
// address, as a database library's error might, so the tests show that the
// cause reaches nothing a caller or the logs see.
type fakePostgres struct {
	// secrets holds each S3 storage's sealed secret as its tenant's store
	// was given it, by "<tenant>/<storage>", so the store can hand it back
	// (pgStore.StorageSecret).
	secrets sync.Map
	dir     string
	mu      sync.Mutex
	down    map[string]bool
	// logDown names the tenants whose audit log refuses entries while the
	// rest of their database answers.
	logDown map[string]bool
}

// setLogDown makes the tenant named name's audit log refuse entries or not.
func (f *fakePostgres) setLogDown(name string, down bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.logDown == nil {
		f.logDown = map[string]bool{}
	}
	f.logDown[name] = down
}

// isLogDown reports whether the tenant named name's audit log refuses
// entries.
func (f *fakePostgres) isLogDown(name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.logDown[name]
}

// setDown makes the tenant named name's database answer or not.
func (f *fakePostgres) setDown(name string, down bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down == nil {
		f.down = map[string]bool{}
	}
	f.down[name] = down
}

// isDown reports whether the tenant named name's database does not answer.
func (f *fakePostgres) isDown(name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.down[name]
}

// noAnswer is the error of a database that does not answer.
func noAnswer(address string) error {
	return fault.Wrap(fault.Unavailable, errors.New("dial "+address+": connection refused"), "connection refused")
}

// pgStore is a tenant's store whose pings fail while its tenant is down. It
// keeps the reception pipeline's data as every driver does.
type pgStore struct {
	tenant.Pipeline
	f       *fakePostgres
	name    string
	address string
}

// AppendAudit fails as a database that does not answer while the tenant's
// log is set down.
func (s pgStore) AppendAudit(ctx context.Context, entries ...master.AuditEntry) error {
	if s.f.isLogDown(s.name) {
		return noAnswer(s.address)
	}
	return s.Pipeline.AppendAudit(ctx, entries...)
}

// Ping fails while the tenant is down.
func (s pgStore) Ping(ctx context.Context) error {
	if s.f.isDown(s.name) {
		return noAnswer(s.address)
	}
	return s.Pipeline.Ping(ctx)
}

// Location returns a location with no user, password or host of the
// address.
func (f *fakePostgres) Location(name, address string) (string, error) {
	if address == "" {
		return "", fault.New(fault.Invalid, "a Postgres tenant needs an address")
	}
	if strings.Contains(address, "location=refused") {
		// An address this driver does not take at all, refused before
		// anything is written.
		return "", fault.New(fault.Invalid, "the driver does not take this address")
	}
	return "pg.location.test:5432/" + name, nil
}

// Open opens the tenant's file, or fails while the tenant is down.
func (f *fakePostgres) Open(ctx context.Context, id, name, address string) (tenant.Store, error) {
	if f.isDown(name) {
		return nil, noAnswer(address)
	}
	if strings.Contains(address, "location=refused") {
		return nil, fault.New(fault.Invalid, "the database refuses this address")
	}
	if strings.Contains(address, "sslmode=refused") {
		// An address this database will never take, as a remote address
		// with no protection would be.
		return nil, fault.New(fault.Invalid, "the database refuses this address")
	}
	st, err := driver.SQLiteTenants{DataDir: f.dir}.Open(ctx, id, name, "")
	if err != nil {
		return nil, err
	}
	p, ok := st.(tenant.Pipeline)
	if !ok {
		_ = st.Close()
		return nil, errors.New("the SQLite driver's store keeps no pipeline")
	}
	return pgStore{Pipeline: p, f: f, name: name, address: address}, nil
}

// faultyMaster is the master as the registry and the handlers see it, with
// two operations that a test can make fail on their own while the rest of
// the master, its audit log among it, keeps working.
type faultyMaster struct {
	master.Store
	failSetSetting   atomic.Bool
	failRemoveTenant atomic.Bool
}

// SetSetting fails as a master that does not answer while failSetSetting is
// set.
func (m *faultyMaster) SetSetting(ctx context.Context, name, value string, audit master.AuditEntry, by master.Actor) error {
	if m.failSetSetting.Load() {
		return fault.New(fault.Unavailable, "the master does not answer (a test)")
	}
	return m.Store.SetSetting(ctx, name, value, audit, by)
}

// RemoveTenant fails as a master that does not answer while
// failRemoveTenant is set.
func (m *faultyMaster) RemoveTenant(ctx context.Context, id string, audit master.AuditEntry, by master.Actor) error {
	if m.failRemoveTenant.Load() {
		return fault.New(fault.Unavailable, "the master does not answer (a test)")
	}
	return m.Store.RemoveTenant(ctx, id, audit, by)
}

// env is one test's installation: a SQLite master and SQLite tenants in a
// temporary directory, a real key file, the real access package and
// registry, the handlers, and an HTTP server whose only middleware is the
// stand-in for the server's path.
type env struct {
	// probedHosts are the addresses the services probed for a free port.
	probedHosts struct {
		sync.Mutex
		hosts []string
	}
	t        testing.TB
	dir      string
	data     string
	master   master.Store
	keys     *secret.Keys
	access   *access.Access
	registry *registry.Registry
	pg       *fakePostgres
	log      *syncBuffer
	url      string
	client   *http.Client
	seq      atomic.Int64
	ticks    atomic.Int64
	offset   atomic.Int64
	requests atomic.Int64
	// decide, when false, makes the stand-in skip putting the Decision in
	// the context, as a server that forgot would.
	decide atomic.Bool
	// known is the list of settings the handlers are given: the server's,
	// with the test's own added.
	known *settings.Table
	// badTenantID, when true, makes the handlers' id source give a tenant id
	// the master refuses: a fault of the server.
	badTenantID atomic.Bool
	// faulty is the master the registry and the handlers are given.
	faulty *faultyMaster
	// afterCheck, when set, runs once in the stand-in after the check
	// allowed a request and before its handler, as something else the
	// server does meanwhile would.
	afterCheck atomic.Pointer[func()]
	// parts is what the handlers are given in place of the reception
	// pipeline and the receivers' supervisor: it records what they are
	// asked, and answers as a test sets it.
	parts *testParts
	// handlers are the services under test, for the seams of export_test.go.
	handlers api.Handlers
	// engines are the tenants' corrections engines the handlers are given.
	engines *correct.Engines
	// pipe is lab1's running reception pipeline, once a test runs one,
	// which a replace makes its file through.
	pipe atomic.Pointer[receive.Pipeline]
	// handoffs is what the handlers ask to drain a tenant and check its
	// owner records: it answers as a test sets it.
	handoffs testHandoffs
}

// newEnv returns a running installation with no tenant and no key, whose
// registry checks every tenant each 20 ms, so a test sees a database go down
// at once.
func newEnv(t testing.TB) *env { return newEnvChecking(t, 20*time.Millisecond) }

// newEnvChecking is newEnv with the registry checking every tenant, and
// reconciling with the master's list, each interval.
func newEnvChecking(t testing.TB, interval time.Duration) *env {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // G302: a directory needs its execute bit.
		t.Fatal(err)
	}
	e := &env{t: t, dir: dir, data: filepath.Join(dir, "data"), log: &syncBuffer{}, known: testSettings(t), parts: &testParts{}}
	e.decide.Store(true)
	if err := os.Mkdir(e.data, 0o700); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(dir, "key")
	if err := secret.CreateKeyFile(keyPath); err != nil {
		t.Fatal(err)
	}
	keys, err := secret.LoadKeyFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	e.keys = keys
	m, err := driver.OpenMaster(ctx, driver.SQLiteScheme+filepath.Join(e.data, "master.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	e.master = m
	e.faulty = &faultyMaster{Store: m}
	logger := slog.New(slog.NewTextHandler(e.log, &slog.HandlerOptions{Level: slog.LevelDebug}))
	e.access, err = access.New(access.Config{Store: m, Clock: e.now, NewID: e.newID, AuditInterval: 5 * time.Millisecond, AuditBackoff: time.Millisecond, Log: logger})
	if err != nil {
		t.Fatal(err)
	}
	e.pg = &fakePostgres{dir: filepath.Join(dir, "pg")}
	e.registry, err = registry.New(registry.Config{
		Master: e.faulty, Keys: keys,
		Drivers: map[string]registry.Driver{master.SQLiteDriver: driver.SQLiteTenants{DataDir: e.data}, master.PostgresDriver: e.pg},
		Now:     e.now,
		// Generous deadlines: a real SQLite open, or a ping of one, on a
		// loaded runner can take seconds, and no test here is about them.
		Interval: interval, OpenTimeout: 30 * time.Second, PingTimeout: 10 * time.Second, MaxOpening: 4,
		Log: logger, NewID: e.newID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.registry.Start(ctx); err != nil {
		t.Fatal(err)
	}
	tenantID := func() string {
		if e.badTenantID.Load() {
			return "not an id"
		}
		return e.newID()
	}
	feeds, err := events.New(events.Config{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = feeds.Close() })
	e.engines = newTestEngines(t, e)
	h, err := api.New(api.Config{Access: e.access, Registry: e.registry, Master: e.faulty, NewID: tenantID, Log: logger, Settings: e.known, Keys: e.keys, Now: e.now,
		Incoming: e.parts, Headers: e.parts, HeldFiles: e.parts, Feeds: feeds, Installation: testInstallation,
		Corrections: e.engines, ReplaceSources: testReplaceSources{e}, Handoffs: &e.handoffs})
	if err != nil {
		t.Fatal(err)
	}
	e.handlers = h
	api.SetPortProbe(h, func(host string, port int) bool {
		e.probedHosts.Lock()
		e.probedHosts.hosts = append(e.probedHosts.hosts, host)
		e.probedHosts.Unlock()
		return place.SystemProbe(host, port)
	})
	srv := connect.NewServer(e.standIn)
	h.Register(srv)
	mux := http.NewServeMux()
	// As the server does for its long procedures: ReadFiles is sent
	// uncompressed whatever the client accepts.
	connecthttp.Mount(mux, srv, connecthttp.WithConditionalOptions(func(spec connect.Spec) []connecthttp.Option {
		if spec.Procedure != nopacsv1connect.DataServiceReadFilesProcedure {
			return nil
		}
		return []connecthttp.Option{connecthttp.WithCompressMinBytes(math.MaxInt)}
	}))
	hs := httptest.NewServer(mux)
	e.url, e.client = hs.URL, hs.Client()
	t.Cleanup(func() {
		hs.Close()
		cctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = e.registry.Close(cctx)
		_ = e.access.Close(cctx)
		_ = m.Close()
	})
	return e
}

// now is the test clock: t0, a microsecond later at each reading, so audit
// entries have distinct times and list in the order they were made.
func (e *env) now() time.Time {
	return t0.Add(time.Duration(e.offset.Load()) + time.Duration(e.ticks.Add(1))*time.Microsecond)
}

// advance moves the test clock forward by d.
func (e *env) advance(d time.Duration) { e.offset.Add(int64(d)) }

// The settings tests register, beside the server's own: the server ships
// none a tenant may override, and the tests need some. They are never in the
// binary.
const (
	// testRetention is a period a tenant may override, 365d by default.
	testRetention = "test.retention"
	// testText takes any text, the empty one included, and a tenant may
	// override it.
	testText = "test.text"
	// testAllotted is set on a tenant by the installation's admin only. The
	// method table does not know it (it reads the server's own list), so a
	// tenant's admin passes the check, and only the handler's own rule
	// refuses the change.
	testAllotted = "test.allotted"
)

// testSettings returns the server's list of settings with the test's two.
func testSettings(t testing.TB) *settings.Table {
	t.Helper()
	period := func(v string) error {
		_, err := settings.ParseDuration(v)
		return err
	}
	defs := append(settings.Builtin(),
		settings.Definition{Name: testRetention, Default: "365d", TenantOverride: true, Validate: period},
		settings.Definition{Name: testText, Default: "", TenantOverride: true, Validate: func(string) error { return nil }},
		settings.Definition{Name: testAllotted, Default: "", TenantOverride: true, TenantSetByInstallation: true, Validate: func(string) error { return nil }},
	)
	tab, err := settings.New(defs...)
	if err != nil {
		t.Fatal(err)
	}
	return tab
}

// downTenant creates a tenant of the test's Postgres driver named name and
// then makes its database stop answering, and waits until the registry and
// the master both hold it unreachable. The clock is moved past the minute
// the registry waits between writes of a state to the master.
func (e *env) downTenant(name string) {
	e.t.Helper()
	o := call(e.as(local).tenants.CreateTenant, &v1.CreateTenantRequest{Name: name, DatabaseType: v1.DatabaseType_DATABASE_TYPE_POSTGRES, DatabaseAddress: pgAddress})
	if o.err != nil {
		e.t.Fatalf("creating %s: %v", name, o.err)
	}
	e.pg.setDown(name, true)
	e.advance(2 * registry.StateChangeInterval)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		st, _ := e.registry.State(name)
		row, err := e.master.TenantByName(context.Background(), name)
		if st.State == master.StateUnreachable && err == nil && row.State == master.StateUnreachable {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	e.t.Fatalf("%s did not become unreachable", name)
}

// newID returns a fresh id.
func (e *env) newID() string { return fmt.Sprintf("id-%07d", e.seq.Add(1)) }

// replay is the request stream after the stand-in has read its message:
// the handler receives that message.
type replay struct {
	connect.ServerStream
	msg  proto.Message
	read bool
}

// Receive gives the message the stand-in read, once.
func (r *replay) Receive(dst any) error {
	if r.read {
		return io.EOF
	}
	r.read = true
	m, ok := dst.(proto.Message)
	if !ok {
		return errors.New("replay: not a message")
	}
	proto.Merge(m, r.msg)
	return nil
}

// standIn is the test's stand-in for the server's path, and the only
// middleware: it sets the request id, reads the credential, asks
// api.Methods for the requirement and the audit facts, calls the real
// access.Check, puts the Decision in the context with api.WithDecision, and
// answers a refusal with its code. It reads the request before the check
// only when the requirement depends on it, as the server does.
func (e *env) standIn(next connect.ServerFunc) connect.ServerFunc {
	return func(ctx context.Context, spec connect.Spec, stream connect.ServerStream) error {
		info, _ := connect.CallInfoForServerContext(ctx)
		id := fmt.Sprintf("req-%06d", e.requests.Add(1))
		info.ResponseHeader().Set(api.RequestIDHeader, id)

		var methods api.Methods
		msg, known := methods.RequestFor(spec.Procedure)
		if !known {
			return connect.NewError(connect.CodeUnimplemented, "unknown method")
		}
		var read proto.Message
		if msg != nil {
			if err := stream.Receive(msg); err != nil {
				return err
			}
			read = msg
		}
		need, facts := methods.Requirement(spec.Procedure, read)
		facts.Method, facts.RequestID, facts.RemoteAddress = spec.Procedure, id, "127.0.0.1:1"

		cred := access.APIKey(strings.TrimPrefix(info.RequestHeader().Get("Authorization"), "Bearer "))
		if u := info.RequestHeader().Get(localHeader); u != "" {
			cred = access.LocalOperator(u)
		}
		d, err := e.access.Check(ctx, cred, need, facts)
		if err != nil {
			return codes.ToConnect(err)
		}
		if hook := e.afterCheck.Swap(nil); hook != nil {
			(*hook)()
		}
		if e.decide.Load() {
			ctx = api.WithDecision(ctx, d)
		}
		if msg != nil {
			stream = &replay{ServerStream: stream, msg: msg}
		}
		return next(ctx, spec, stream)
	}
}

// caller is who a test client calls as: a presented key, the local
// operator, or nobody.
type caller struct {
	key   string
	local string
}

// local is the local operator.
var local = caller{local: "501"}

// nobody is a caller with no key.
var nobody = caller{}

// clients is the generated Connect clients of the services, for one caller.
type clients struct {
	tenants    nopacsv1connect.TenantServiceClient
	access     nopacsv1connect.AccessServiceClient
	settings   nopacsv1connect.SettingsServiceClient
	audit      nopacsv1connect.AuditServiceClient
	storages   nopacsv1connect.StorageServiceClient
	receivers  nopacsv1connect.ReceiverServiceClient
	receptions nopacsv1connect.ReceptionServiceClient
	quarantine nopacsv1connect.QuarantineServiceClient
	monitor    nopacsv1connect.MonitorServiceClient
	metadata   nopacsv1connect.MetadataServiceClient
	catalog    nopacsv1connect.CatalogServiceClient
	data       nopacsv1connect.DataServiceClient
	senders    nopacsv1connect.SenderServiceClient
	jobs       nopacsv1connect.JobServiceClient
	profiles   nopacsv1connect.ProfileServiceClient
	deid       nopacsv1connect.DeidServiceClient
	auditSeal  nopacsv1connect.AuditSealServiceClient
	// corrections is CorrectionService's client.
	corrections nopacsv1connect.CorrectionServiceClient
	// backups is BackupService's client.
	backups nopacsv1connect.BackupServiceClient
}

// as returns clients that call as c.
func (e *env) as(c caller) clients {
	set := func(next connect.ClientFunc) connect.ClientFunc {
		return func(ctx context.Context, spec connect.Spec) (connect.ClientStream, error) {
			if info, ok := connect.CallInfoForClientContext(ctx); ok {
				if c.key != "" {
					info.RequestHeader().Set("Authorization", "Bearer "+c.key)
				}
				if c.local != "" {
					info.RequestHeader().Set(localHeader, c.local)
				}
			}
			return next(ctx, spec)
		}
	}
	cl := connect.NewClient(connecthttp.NewTransport(e.client, e.url), set)
	return clients{
		tenants:    nopacsv1connect.NewTenantServiceClient(cl),
		access:     nopacsv1connect.NewAccessServiceClient(cl),
		settings:   nopacsv1connect.NewSettingsServiceClient(cl),
		audit:      nopacsv1connect.NewAuditServiceClient(cl),
		storages:   nopacsv1connect.NewStorageServiceClient(cl),
		receivers:  nopacsv1connect.NewReceiverServiceClient(cl),
		receptions: nopacsv1connect.NewReceptionServiceClient(cl),
		quarantine: nopacsv1connect.NewQuarantineServiceClient(cl),
		monitor:    nopacsv1connect.NewMonitorServiceClient(cl),
		metadata:   nopacsv1connect.NewMetadataServiceClient(cl),
		catalog:    nopacsv1connect.NewCatalogServiceClient(cl),
		data:       nopacsv1connect.NewDataServiceClient(cl),
		senders:    nopacsv1connect.NewSenderServiceClient(cl),
		jobs:       nopacsv1connect.NewJobServiceClient(cl),
		profiles:   nopacsv1connect.NewProfileServiceClient(cl),
		deid:       nopacsv1connect.NewDeidServiceClient(cl),
		auditSeal:  nopacsv1connect.NewAuditSealServiceClient(cl),
		// The correction service's.
		corrections: nopacsv1connect.NewCorrectionServiceClient(cl),
		backups:     nopacsv1connect.NewBackupServiceClient(cl),
	}
}

// outcome is what one call answered: its response, its error, and the
// request id the server returned.
type outcome struct {
	res       proto.Message
	err       error
	requestID string
}

// code returns the call's code, or 0 for success.
func (o outcome) code() connect.Code {
	if o.err == nil {
		return 0
	}
	return connect.CodeOf(o.err)
}

// message returns the error's message, or "".
func (o outcome) message() string {
	var ce *connect.Error
	if errors.As(o.err, &ce) {
		return ce.Message()
	}
	return ""
}

// call makes one call with a client context, so the response's request id
// is read.
func call[Req, Res proto.Message](f func(context.Context, Req) (Res, error), req Req) outcome {
	ctx, info := connect.NewClientContext(context.Background())
	res, err := f(ctx, req)
	o := outcome{err: err, requestID: info.ResponseHeader().Get(api.RequestIDHeader)}
	if err == nil {
		o.res = res
	}
	return o
}

// watchOnce opens a stream with req and returns its first message or its
// error as an outcome, then ends the stream: what a table needs of Watch.
func watchOnce(c clients, req *v1.WatchRequest) outcome {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ctx, info := connect.NewClientContext(ctx)
	s, err := c.monitor.Watch(ctx, req)
	if err != nil {
		return outcome{err: err, requestID: info.ResponseHeader().Get(api.RequestIDHeader)}
	}
	defer func() { _ = s.Close() }()
	msg, err := s.Receive()
	o := outcome{err: err, requestID: info.ResponseHeader().Get(api.RequestIDHeader)}
	if err == nil {
		o.res = msg
	}
	return o
}

// key makes a key in the master with the grants given and returns it as a
// caller, with its id.
func (e *env) key(grants ...master.Grant) (caller, string) {
	e.t.Helper()
	ctx := context.Background()
	made, err := secret.MakeAPIKey()
	if err != nil {
		e.t.Fatal(err)
	}
	k := master.Key{ID: made.ID, Hash: made.Hash[:], Holder: "test", Created: t0}
	if err := e.master.CreateKey(ctx, k, e.setupEntry(), master.LocalOperator()); err != nil {
		e.t.Fatal(err)
	}
	for _, g := range grants {
		g.KeyID = k.ID
		if _, err := e.master.SetGrant(ctx, g, e.setupEntry(), master.LocalOperator()); err != nil {
			e.t.Fatal(err)
		}
	}
	return caller{key: made.Key()}, k.ID
}

// setupEntry is the audit entry of a change a test makes directly.
func (e *env) setupEntry() master.AuditEntry {
	return master.AuditEntry{ID: e.newID(), Time: e.now(), Method: "test.setup", Outcome: master.OutcomeDone}
}

// onInstallation is a grant of role on the installation.
func onInstallation(role master.Role) master.Grant {
	return master.Grant{Scope: master.Installation, Role: role}
}

// onTenant is a grant of role on the tenant named name.
func (e *env) onTenant(name string, role master.Role) master.Grant {
	e.t.Helper()
	t, err := e.master.TenantByName(context.Background(), name)
	if err != nil {
		e.t.Fatal(err)
	}
	return master.Grant{Scope: master.TenantScope(t.ID), Role: role}
}

// tenant creates a SQLite tenant over the API, as the local operator, and
// fails the test unless it is ready.
func (e *env) tenant(name string) {
	e.t.Helper()
	o := call(e.as(local).tenants.CreateTenant, &v1.CreateTenantRequest{Name: name, DatabaseType: v1.DatabaseType_DATABASE_TYPE_SQLITE})
	if o.err != nil {
		e.t.Fatalf("creating tenant %s: %v", name, o.err)
	}
	if s := o.res.(*v1.CreateTenantResponse).GetTenant().GetState(); s != v1.TenantState_TENANT_STATE_READY {
		e.t.Fatalf("tenant %s is %v", name, s)
	}
}

// flush waits until the access package's audit writer has written what it
// holds.
func (e *env) flush() {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := e.access.FlushAudit(ctx); err != nil {
		e.t.Fatal(err)
	}
}

// masterLog returns every entry of the installation's log, after a flush.
func (e *env) masterLog() []master.AuditEntry {
	e.t.Helper()
	e.flush()
	var out []master.AuditEntry
	page := master.Page{Limit: master.MaxPageLimit}
	for {
		entries, next, err := e.master.ListAudit(context.Background(), master.AuditFilter{}, page)
		if err != nil {
			e.t.Fatal(err)
		}
		out = append(out, entries...)
		if next == "" {
			return out
		}
		page.Cursor = next
	}
}

// tenantLog returns every entry of a tenant's log, after a flush of the
// audit writer.
func (e *env) tenantLog(name string) []master.AuditEntry {
	e.t.Helper()
	e.flush()
	st, err := e.registry.Reader(name)
	if err != nil {
		e.t.Fatal(err)
	}
	var out []master.AuditEntry
	page := master.Page{Limit: master.MaxPageLimit}
	for {
		entries, next, err := st.ListAudit(context.Background(), master.AuditFilter{}, page)
		if err != nil {
			e.t.Fatal(err)
		}
		out = append(out, entries...)
		if next == "" {
			return out
		}
		page.Cursor = next
	}
}

// entriesOf returns the entries of a log with this request id.
func entriesOf(log []master.AuditEntry, requestID string) []master.AuditEntry {
	var out []master.AuditEntry
	for _, en := range log {
		if en.RequestID == requestID {
			out = append(out, en)
		}
	}
	return out
}

// installation is the installation scope as a request names it.
func installation() *v1.Scope {
	return &v1.Scope{Kind: &v1.Scope_Installation{Installation: &v1.Installation{}}}
}

// tenantScope is a tenant scope as a request names it.
func tenantScope(name string) *v1.Scope {
	return &v1.Scope{Kind: &v1.Scope_Tenant{Tenant: name}}
}

// probed returns the addresses probed for a free port so far.
func (e *env) probed() []string {
	e.probedHosts.Lock()
	defer e.probedHosts.Unlock()
	return slices.Clone(e.probedHosts.hosts)
}
