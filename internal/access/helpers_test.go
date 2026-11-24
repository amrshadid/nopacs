package access_test

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/access"
	"github.com/amrshadid/nopacs/internal/driver/sqlite"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/secret"
)

// t0 is the time the tests start at, with microseconds, as the master keeps
// them.
var t0 = time.Date(2026, 11, 18, 10, 0, 0, 123456000, time.UTC)

// clock is a clock a test moves by hand, so expiry is tested without
// sleeping.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

// Now returns the clock's time.
func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Set moves the clock to t.
func (c *clock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
}

// ids returns an id source that counts, so each test knows the ids it will
// read back and newer entries sort after older ones.
func ids(prefix string) func() string {
	var n atomic.Uint64
	return func() string { return fmt.Sprintf("%s-%08d", prefix, n.Add(1)) }
}

// openSQLite opens a fresh, migrated SQLite master in a temporary file,
// closed when tb ends.
func openSQLite(tb testing.TB) master.Store {
	tb.Helper()
	ctx := context.Background()
	m, err := sqlite.OpenMaster(ctx, filepath.Join(tb.TempDir(), "master.db"))
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { _ = m.Close() })
	if err := m.Migrate(ctx); err != nil {
		tb.Fatal(err)
	}
	return m
}

// counting wraps a Store and records the name of every call made on the
// request's path, in order, so a test can show two refusals read the same
// things in the same order, and the entries every write was given.
// MarkKeyUsed and AppendAudit are counted apart: they run on the package's
// workers, at moments no test controls.
type counting struct {
	access.Store
	mu      sync.Mutex
	calls   []string
	entries []master.AuditEntry
	marks   atomic.Int64
	appends atomic.Int64
	// tenantArgs is the tenant name of every RolesFor call.
	tenantArgs []string
	// markedKeys is the key id of every MarkKeyUsed call.
	markedKeys []string
	// failAudit and failMarks make AppendAudit and MarkKeyUsed fail, so a
	// test can make the package log.
	failAudit atomic.Bool
	failMarks atomic.Bool
	// failCreateKey, when set, is what CreateKey returns without calling the
	// store; attempted is the entry the last CreateKey was given.
	failCreateKey error
	attempted     master.AuditEntry
	// keyDownAfterRevoke makes every Key fail as Unavailable once a
	// RevokeKey has succeeded.
	keyDownAfterRevoke atomic.Bool
	revoked            atomic.Bool
}

func (c *counting) note(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, name)
}

// reset forgets the calls and entries recorded so far.
func (c *counting) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls, c.entries = nil, nil
}

// snapshot returns the calls and entries recorded since the last reset.
func (c *counting) snapshot() ([]string, []master.AuditEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.calls...), append([]master.AuditEntry(nil), c.entries...)
}

// written records a change's audit entry when the change, and so its
// entry, was written: a change that fails writes neither.
func (c *counting) written(e master.AuditEntry, err error) {
	if err != nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = append(c.entries, e)
}

func (c *counting) Key(ctx context.Context, id string) (master.Key, error) {
	c.note("Key")
	if c.keyDownAfterRevoke.Load() && c.revoked.Load() {
		return master.Key{}, fault.New(fault.Unavailable, "test: the master stopped answering")
	}
	return c.Store.Key(ctx, id)
}

func (c *counting) RolesFor(ctx context.Context, keyID, tenantName string) (master.Roles, error) {
	c.mu.Lock()
	c.calls = append(c.calls, "RolesFor")
	c.tenantArgs = append(c.tenantArgs, tenantName)
	c.mu.Unlock()
	return c.Store.RolesFor(ctx, keyID, tenantName)
}

func (c *counting) GrantsByKey(ctx context.Context, keyID string, p master.Page) ([]master.Grant, string, error) {
	c.note("GrantsByKey")
	return c.Store.GrantsByKey(ctx, keyID, p)
}

func (c *counting) GrantsByScope(ctx context.Context, s master.Scope, p master.Page) ([]master.Grant, string, error) {
	c.note("GrantsByScope")
	return c.Store.GrantsByScope(ctx, s, p)
}

func (c *counting) TenantByName(ctx context.Context, name string) (master.Tenant, error) {
	c.note("TenantByName")
	return c.Store.TenantByName(ctx, name)
}

func (c *counting) MarkKeyUsed(ctx context.Context, id string, at time.Time, address string) (bool, error) {
	c.marks.Add(1)
	c.mu.Lock()
	c.markedKeys = append(c.markedKeys, id)
	c.mu.Unlock()
	if c.failMarks.Load() {
		return false, fault.New(fault.Unavailable, "test: marks fail")
	}
	return c.Store.MarkKeyUsed(ctx, id, at, address)
}

func (c *counting) AppendAudit(ctx context.Context, entries ...master.AuditEntry) error {
	c.appends.Add(1)
	c.mu.Lock()
	c.entries = append(c.entries, entries...)
	c.mu.Unlock()
	if c.failAudit.Load() {
		return fault.New(fault.Unavailable, "test: audit fails")
	}
	return c.Store.AppendAudit(ctx, entries...)
}

func (c *counting) CheckActor(ctx context.Context, by master.Actor) error {
	c.note("CheckActor")
	return c.Store.CheckActor(ctx, by)
}

func (c *counting) CreateKey(ctx context.Context, k master.Key, e master.AuditEntry, by master.Actor) error {
	c.note("CreateKey")
	c.mu.Lock()
	c.attempted = e
	fail := c.failCreateKey
	c.mu.Unlock()
	if fail != nil {
		return fail
	}
	err := c.Store.CreateKey(ctx, k, e, by)
	c.written(e, err)
	return err
}

func (c *counting) RevokeKey(ctx context.Context, id string, at time.Time, e master.AuditEntry, by master.Actor) (time.Time, error) {
	c.note("RevokeKey")
	out, err := c.Store.RevokeKey(ctx, id, at, e, by)
	c.written(e, err)
	if err == nil {
		c.revoked.Store(true)
	}
	return out, err
}

func (c *counting) SetGrant(ctx context.Context, g master.Grant, e master.AuditEntry, by master.Actor) (master.Role, error) {
	c.note("SetGrant")
	out, err := c.Store.SetGrant(ctx, g, e, by)
	c.written(e, err)
	return out, err
}

func (c *counting) RemoveGrant(ctx context.Context, keyID string, s master.Scope, e master.AuditEntry, by master.Actor) (master.Role, error) {
	c.note("RemoveGrant")
	out, err := c.Store.RemoveGrant(ctx, keyID, s, e, by)
	c.written(e, err)
	return out, err
}

// env is one test's installation: a real SQLite master behind a counting
// wrapper, an Access over it, a clock and a log the test can search.
type env struct {
	t      testing.TB
	ctx    context.Context
	master master.Store
	store  *counting
	clock  *clock
	access *access.Access
	log    *bytes.Buffer
	logMu  *sync.Mutex
	setup  func() string
	// The two tenants every env has.
	lab, clinic master.Tenant
}

// lockedWriter serializes writes to a buffer the worker and the test share.
type lockedWriter struct {
	mu *sync.Mutex
	b  *bytes.Buffer
}

func (w lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

// newEnv makes an env with the tenants "lab" and "clinic". Each option
// changes the Config before New, such as to give it an event sink.
func newEnv(t testing.TB, options ...func(*access.Config)) *env {
	t.Helper()
	m := openSQLite(t)
	e := &env{
		t: t, ctx: context.Background(), master: m, store: &counting{Store: m},
		clock: &clock{now: t0}, log: &bytes.Buffer{}, logMu: &sync.Mutex{}, setup: ids("setup"),
	}
	cfg := access.Config{
		Store: e.store, Clock: e.clock.Now, NewID: ids("audit"),
		Log: slog.New(slog.NewTextHandler(lockedWriter{e.logMu, e.log}, nil)),
	}
	for _, o := range options {
		o(&cfg)
	}
	a, err := access.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	e.access = a
	t.Cleanup(func() { _ = a.Close(context.Background()) })
	e.lab = e.tenant("t-lab", "lab")
	e.clinic = e.tenant("t-clinic", "clinic")
	return e
}

// flush waits until the audit writer has written every entry handed to it.
func (e *env) flush() {
	e.t.Helper()
	if err := e.access.FlushAudit(e.ctx); err != nil {
		e.t.Fatal(err)
	}
}

// reset flushes the audit writer, so no entry of an earlier request is
// counted as the next one's, and forgets what the store recorded.
func (e *env) reset() {
	e.t.Helper()
	e.flush()
	e.store.reset()
}

// recorded flushes the audit writer and returns the calls and the audit
// entries recorded since the last reset.
func (e *env) recorded() ([]string, []master.AuditEntry) {
	e.flush()
	return e.store.snapshot()
}

// decide makes the Decision a server hands a handler: the check of cred for
// req, with facts naming method. A refusal fails the test.
func (e *env) decide(cred access.Credential, req access.Requirement, method string) access.Decision {
	e.t.Helper()
	d, err := e.access.Check(e.ctx, cred, req, facts(method))
	if err != nil {
		e.t.Fatalf("check for %s: %v", method, err)
	}
	return d
}

// admin is the Decision of the local operator as the installation's admin,
// for method.
func (e *env) admin(method string) access.Decision {
	e.t.Helper()
	return e.decide(local, access.OnInstallation(master.RoleAdmin), method)
}

// setupEntry is the audit entry of a change the test makes directly in the
// master, outside the package under test.
func (e *env) setupEntry() master.AuditEntry {
	return master.AuditEntry{ID: e.setup(), Time: t0, LocalUser: "0", Method: "Setup", Outcome: master.OutcomeDone}
}

// tenant registers a tenant directly in the master.
func (e *env) tenant(id, name string) master.Tenant {
	e.t.Helper()
	tn := master.Tenant{ID: id, Name: name, Driver: "sqlite", SealedAddress: []byte{1}, State: master.StateReady, Created: t0}
	if err := e.master.CreateTenant(e.ctx, tn, e.setupEntry(), master.LocalOperator()); err != nil {
		e.t.Fatal(err)
	}
	return tn
}

// testKey is a key made by package secret and stored in the master, with
// the whole key the test presents.
type testKey struct {
	id   string
	full string
}

// key makes a key with an expiry (zero for never) and grants, directly in
// the master.
func (e *env) key(expires time.Time, grants ...master.Grant) testKey {
	e.t.Helper()
	made, err := secret.MakeAPIKey()
	if err != nil {
		e.t.Fatal(err)
	}
	k := master.Key{ID: made.ID, Hash: made.Hash[:], Holder: "test", Created: t0.Add(-48 * time.Hour), Expires: expires}
	if err := e.master.CreateKey(e.ctx, k, e.setupEntry(), master.LocalOperator()); err != nil {
		e.t.Fatal(err)
	}
	for _, g := range grants {
		g.KeyID = made.ID
		if _, err := e.master.SetGrant(e.ctx, g, e.setupEntry(), master.LocalOperator()); err != nil {
			e.t.Fatal(err)
		}
	}
	return testKey{id: made.ID, full: made.Key()}
}

// on is a grant of role on scope, for env.key, which fills in the key.
func on(scope master.Scope, role master.Role) master.Grant {
	return master.Grant{Scope: scope, Role: role}
}

// facts are the request facts the tests pass.
func facts(method string) access.Request {
	return access.Request{Method: method, RemoteAddress: "192.0.2.7:51000", RequestID: "req-1"}
}

// wantErr fails unless err has kind and, when message is not empty, exactly
// that message.
func wantErr(t testing.TB, what string, err error, kind fault.Kind, message string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: no error, want %s", what, kind)
	}
	if got := fault.KindOf(err); got != kind {
		t.Fatalf("%s: kind %s (%v), want %s", what, got, err, kind)
	}
	if message != "" && fault.Message(err) != message {
		t.Fatalf("%s: message %q, want %q", what, fault.Message(err), message)
	}
}

// local is the local operator's credential.
var local = access.LocalOperator("501")
