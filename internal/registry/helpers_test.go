package registry

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/driver"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/secret"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// TestMain runs the package's tests, then fails if goroutines are left
// running that were not running before: every goroutine the registry starts
// must have an owner that waits for it, or a stop could leave work running
// against a closed store. database/sql's own goroutines end when a pool
// closes, so a store left open shows here too.
func TestMain(m *testing.M) {
	before := runtime.NumGoroutine()
	code := m.Run()
	if code == 0 {
		deadline := time.Now().Add(30 * time.Second)
		for runtime.NumGoroutine() > before && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if after := runtime.NumGoroutine(); after > before {
			buf := make([]byte, 1<<20)
			n := runtime.Stack(buf, true)
			fmt.Fprintf(os.Stderr, "goroutine leak: %d before the tests, %d after\n%s\n", before, after, buf[:n])
			code = 1
		} else {
			fmt.Printf("goroutines: %d before the tests, %d after\n", before, after)
		}
	}
	os.Exit(code)
}

// t0 is the fake clock's start.
var t0 = time.Date(2026, 11, 18, 9, 30, 0, 0, time.UTC)

// clock is a clock a test moves by hand.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

// newClock returns a clock at t0.
func newClock() *clock { return &clock{t: t0} }

// Now returns the clock's time.
func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

// Add moves the clock forward by d.
func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// privateDir returns a new directory only the current user may use, as the
// key file's directory must be.
func privateDir(t testing.TB) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // G302: a directory needs its execute bit.
		t.Fatal(err)
	}
	return dir
}

// newKeys makes a new key file and loads it.
func newKeys(t testing.TB) *secret.Keys {
	t.Helper()
	path := filepath.Join(privateDir(t), "key")
	if err := secret.CreateKeyFile(path); err != nil {
		t.Fatal(err)
	}
	k, err := secret.LoadKeyFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// newMaster opens a migrated SQLite master in a new directory.
func newMaster(t testing.TB) master.Store {
	t.Helper()
	ctx := context.Background()
	m, err := driver.OpenMaster(ctx, driver.SQLiteScheme+filepath.Join(t.TempDir(), "master.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	if err := m.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	return m
}

// env is one test's installation: a master, a key file, a data directory,
// the SQLite driver and the test driver, a clock and a log.
type env struct {
	t      testing.TB
	master master.Store
	keys   *secret.Keys
	data   string
	flaky  *flaky
	clock  *clock
	logBuf *syncBuffer
	seq    atomic.Int64
}

// newEnv returns an installation with a SQLite master.
func newEnv(t testing.TB) *env {
	t.Helper()
	data := privateDir(t)
	return &env{
		t: t, master: newMaster(t), keys: newKeys(t), data: data,
		flaky: newFlaky(t), clock: newClock(), logBuf: &syncBuffer{},
	}
}

// config returns a registry configuration for the installation, with the
// SQLite driver as "sqlite" and the test driver as "flaky".
func (e *env) config() Config {
	return Config{
		Master:   e.master,
		Keys:     e.keys,
		Drivers:  map[string]Driver{driver.SQLiteDriver: driver.SQLiteTenants{DataDir: e.data}, flakyDriver: e.flaky},
		Now:      e.clock.Now,
		Interval: time.Hour, // tests call checkAll themselves unless they test the timer
		// Generous, above the server's own 10 seconds: a real SQLite open on
		// a loaded runner can take seconds, and no test that uses these is
		// about the deadline. A test that is sets its own, and makes the
		// deadline pass with the test driver's hang or stuck modes, never
		// with a real database being slow.
		OpenTimeout: 30 * time.Second,
		PingTimeout: 10 * time.Second,
		MaxOpening:  8,
		Log:         slog.New(slog.NewTextHandler(e.logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
}

// newRegistry makes a registry with cfg, closed when the test ends.
func (e *env) newRegistry(cfg Config) *Registry {
	e.t.Helper()
	r, err := New(cfg)
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { _ = r.Close(context.Background()) })
	return r
}

// audit returns a new audit entry for a request by the local operator.
func (e *env) audit(method string) master.AuditEntry {
	id := fmt.Sprintf("req-%06d", e.seq.Add(1))
	return master.AuditEntry{
		ID: id, Time: e.clock.Now(), LocalUser: "501",
		Method: method, Outcome: master.OutcomeDone, RequestID: "request-" + id,
	}
}

// create creates a tenant through r, failing the test on an error.
func (e *env) create(r *Registry, id, name, drv, address string) master.Tenant {
	e.t.Helper()
	tn, _, err := r.Create(context.Background(), NewTenant{ID: id, Name: name, Driver: drv, Address: address}, e.audit("CreateTenant"), master.LocalOperator())
	if err != nil {
		e.t.Fatalf("Create(%s): %v", name, err)
	}
	return tn
}

// masterTenant reads a tenant's row from the master.
func (e *env) masterTenant(name string) master.Tenant {
	e.t.Helper()
	tn, err := e.master.TenantByName(context.Background(), name)
	if err != nil {
		e.t.Fatalf("TenantByName(%s): %v", name, err)
	}
	return tn
}

// systemEntries returns the registry's audit entries for the tenant named
// name, oldest first.
func (e *env) systemEntries(name string) []master.AuditEntry {
	e.t.Helper()
	var out []master.AuditEntry
	page := master.Page{Limit: master.MaxPageLimit}
	for {
		entries, next, err := e.master.ListAudit(context.Background(), master.AuditFilter{TenantName: name}, page)
		if err != nil {
			e.t.Fatal(err)
		}
		for _, a := range entries {
			if a.Method == SystemMethod {
				out = append([]master.AuditEntry{a}, out...)
			}
		}
		if next == "" {
			return out
		}
		page.Cursor = next
	}
}

// syncBuffer is a bytes.Buffer safe for the concurrent writes of a logger.
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

// String returns what was written.
func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// flakyDriver is the name of the test driver's database type.
const flakyDriver = "flaky"

// password is the known password in every test driver address; the tests
// search every reason, error, log line and audit entry for it, in every form
// a scrubber of a driver's text was shown to miss (passwordForms).
const password = "Hunter2-NotARealPassword-91f3" //nolint:gosec // G101: made up, so its absence can be searched for.

// flakyHost is the host in every test driver address, also kept out of
// reasons.
const flakyHost = "db.flaky.example"

// flakyAddress returns the address of a test driver tenant.
func flakyAddress(name string) string {
	return "flaky://admin:" + password + "@" + flakyHost + ":5432/" + name
}

// passwordForms are the password as a driver's text may carry it: plain,
// quoted, escaped as libpq escapes a keyword value, URL-escaped, and inside
// a URL that does not parse.
var passwordForms = []string{
	password,
	"'" + password + "'",
	`password='Hunter2\'NotARealPassword'`,
	"Hunter2%2DNotARealPassword%2D91f3",
	"postgres://admin:" + password + "@[db.flaky.example",
}

// mode is how the test driver behaves for one tenant.
type mode int

const (
	// up opens and pings normally.
	up mode = iota
	// down fails to open and to ping, as Unavailable, with a cause that
	// holds the address and the password in every form of passwordForms, as
	// a careless driver's might.
	down
	// hang blocks an open or a ping until its context ends.
	hang
	// stuck blocks an open or a ping until the test releases it, whatever
	// its context does: a driver that ignores its deadline.
	stuck
	// late opens normally, but only once the test releases it, whatever
	// its context does: a store that arrives after its deadline.
	late
	// broken fails to migrate, as Internal, with the address in the cause.
	broken
	// refused fails to open as Invalid, as a database that refuses its
	// address or its file does.
	refused
	// leaky fails to open with a plain error, not a fault, whose text holds
	// the password.
	leaky
	// panicky panics in an open or a ping, with the password in the panic's
	// value.
	panicky
)

// flaky is the test driver. A tenant on it is a real SQLite database in its
// own directory, wrapped so that its open and its ping can be made to fail,
// hang, panic and recover. It is registered only in this package's tests;
// the real SQLite driver is not changed for them.
type flaky struct {
	sqlite  driver.SQLiteTenants
	mu      sync.Mutex
	modes   map[string]mode
	byName  map[string]int // opens begun, by tenant name
	release chan struct{}

	opens, closes atomic.Int64 // stores opened and closed
	opening       atomic.Int64 // opens running now
	maxOpening    atomic.Int64 // the most that ran at once
}

// newFlaky returns a test driver whose databases are under a new directory.
func newFlaky(t testing.TB) *flaky {
	return &flaky{sqlite: driver.SQLiteTenants{DataDir: t.TempDir()}, modes: map[string]mode{},
		byName: map[string]int{}, release: make(chan struct{})}
}

// set sets the mode of the tenant named name.
func (f *flaky) set(name string, m mode) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.modes[name] = m
}

// opensOf returns how many opens of the tenant named name have begun.
func (f *flaky) opensOf(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.byName[name]
}

// unstick releases every stuck or late open and ping, once.
func (f *flaky) unstick() { close(f.release) }

// behave acts out the tenant's mode for an open (open true) or a ping.
func (f *flaky) behave(ctx context.Context, name string, open bool) error {
	f.mu.Lock()
	m := f.modes[name]
	f.mu.Unlock()
	switch m {
	case down:
		return fault.Wrap(fault.Unavailable,
			fmt.Errorf("dial %s: connection refused (%s)", flakyAddress(name), strings.Join(passwordForms, " ")),
			"the database does not answer")
	case hang:
		<-ctx.Done()
		return fault.Wrap(fault.Unavailable, ctx.Err(), "the database does not answer")
	case stuck:
		<-f.release
		return fault.New(fault.Unavailable, "the database does not answer")
	case late:
		<-f.release
	case refused:
		if open {
			return fault.Wrap(fault.Invalid, errors.New("bad address "+flakyAddress(name)), "the database refuses this address")
		}
	case leaky:
		if open {
			return errors.New("open " + flakyAddress(name) + ": password " + password + " rejected")
		}
	case panicky:
		panic("driver bug at " + flakyAddress(name))
	}
	return nil
}

// Open implements Driver.
func (f *flaky) Open(ctx context.Context, id, name, address string) (tenant.Store, error) {
	n := f.opening.Add(1)
	defer f.opening.Add(-1)
	for {
		m := f.maxOpening.Load()
		if n <= m || f.maxOpening.CompareAndSwap(m, n) {
			break
		}
	}
	f.mu.Lock()
	f.byName[name]++
	f.mu.Unlock()
	if err := f.behave(ctx, name, true); err != nil {
		return nil, err
	}
	s, err := f.sqlite.Open(context.Background(), id, name, "")
	if err != nil {
		return nil, err
	}
	f.opens.Add(1)
	return &flakyStore{Store: s, f: f, name: name}, nil
}

// Location implements Driver: host:port/database, with no user and no
// password, as a Postgres driver answers. An address of another scheme is
// refused with an error that, carelessly, repeats it.
func (f *flaky) Location(name, address string) (string, error) {
	u, err := url.Parse(address)
	if err != nil || u.Scheme != flakyDriver {
		return "", fault.New(fault.Invalid, "not a flaky address: "+address)
	}
	return u.Host + u.Path, nil
}

// flakyStore is a test driver tenant's store.
type flakyStore struct {
	tenant.Store
	f      *flaky
	name   string
	closed atomic.Bool
}

// Ping fails, hangs or answers as the tenant's mode says.
func (s *flakyStore) Ping(ctx context.Context) error {
	if err := s.f.behave(ctx, s.name, false); err != nil {
		return err
	}
	return s.Store.Ping(s.f.ctxFor(ctx, s.name))
}

// ctxFor returns ctx, or for a late tenant a context that never ends: a
// driver that ignores its deadline finishes its ping and migration after it,
// so the store it returns arrives whole, late.
func (f *flaky) ctxFor(ctx context.Context, name string) context.Context {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.modes[name] == late {
		return context.WithoutCancel(ctx)
	}
	return ctx
}

// Migrate fails as the tenant's mode says when it is broken.
func (s *flakyStore) Migrate(ctx context.Context) error {
	s.f.mu.Lock()
	m := s.f.modes[s.name]
	s.f.mu.Unlock()
	if m == broken {
		return fault.Wrap(fault.Internal, errors.New("migration 0002 failed at "+flakyAddress(s.name)), "the database cannot be migrated")
	}
	return s.Store.Migrate(s.f.ctxFor(ctx, s.name))
}

// Close closes the database once and counts it.
func (s *flakyStore) Close() error {
	if !s.closed.Swap(true) {
		s.f.closes.Add(1)
	}
	return s.Store.Close()
}

// countingMaster counts every call made to the master it wraps, to show
// which paths reach the master and which do not.
type countingMaster struct {
	master.Store
	calls atomic.Int64
}

// count adds one call.
func (c *countingMaster) count() { c.calls.Add(1) }

// Each method below counts, then calls the wrapped master.

func (c *countingMaster) Migrate(ctx context.Context) error { c.count(); return c.Store.Migrate(ctx) }
func (c *countingMaster) Version(ctx context.Context) (int, error) {
	c.count()
	return c.Store.Version(ctx)
}
func (c *countingMaster) Ping(ctx context.Context) error { c.count(); return c.Store.Ping(ctx) }
func (c *countingMaster) Close() error                   { c.count(); return c.Store.Close() }
func (c *countingMaster) CreateTenant(ctx context.Context, t master.Tenant, a master.AuditEntry, by master.Actor) error {
	c.count()
	return c.Store.CreateTenant(ctx, t, a, by)
}
func (c *countingMaster) Tenant(ctx context.Context, id string) (master.Tenant, error) {
	c.count()
	return c.Store.Tenant(ctx, id)
}
func (c *countingMaster) TenantByName(ctx context.Context, name string) (master.Tenant, error) {
	c.count()
	return c.Store.TenantByName(ctx, name)
}
func (c *countingMaster) ListTenants(ctx context.Context, p master.Page) ([]master.Tenant, string, error) {
	c.count()
	return c.Store.ListTenants(ctx, p)
}
func (c *countingMaster) SetTenantState(ctx context.Context, id string, s master.TenantState, reason string, a master.AuditEntry, by master.Actor) error {
	c.count()
	return c.Store.SetTenantState(ctx, id, s, reason, a, by)
}
func (c *countingMaster) RemoveTenant(ctx context.Context, id string, a master.AuditEntry, by master.Actor) error {
	c.count()
	return c.Store.RemoveTenant(ctx, id, a, by)
}
func (c *countingMaster) CreateKey(ctx context.Context, k master.Key, a master.AuditEntry, by master.Actor) error {
	c.count()
	return c.Store.CreateKey(ctx, k, a, by)
}
func (c *countingMaster) Key(ctx context.Context, id string) (master.Key, error) {
	c.count()
	return c.Store.Key(ctx, id)
}
func (c *countingMaster) ListKeys(ctx context.Context, p master.Page) ([]master.Key, string, error) {
	c.count()
	return c.Store.ListKeys(ctx, p)
}
func (c *countingMaster) MarkKeyUsed(ctx context.Context, id string, at time.Time, address string) (bool, error) {
	c.count()
	return c.Store.MarkKeyUsed(ctx, id, at, address)
}
func (c *countingMaster) RevokeKey(ctx context.Context, id string, at time.Time, a master.AuditEntry, by master.Actor) (time.Time, error) {
	c.count()
	return c.Store.RevokeKey(ctx, id, at, a, by)
}
func (c *countingMaster) SetGrant(ctx context.Context, g master.Grant, a master.AuditEntry, by master.Actor) (master.Role, error) {
	c.count()
	return c.Store.SetGrant(ctx, g, a, by)
}
func (c *countingMaster) GrantsByKey(ctx context.Context, id string, p master.Page) ([]master.Grant, string, error) {
	c.count()
	return c.Store.GrantsByKey(ctx, id, p)
}
func (c *countingMaster) GrantsByScope(ctx context.Context, s master.Scope, p master.Page) ([]master.Grant, string, error) {
	c.count()
	return c.Store.GrantsByScope(ctx, s, p)
}
func (c *countingMaster) RemoveGrant(ctx context.Context, id string, s master.Scope, a master.AuditEntry, by master.Actor) (master.Role, error) {
	c.count()
	return c.Store.RemoveGrant(ctx, id, s, a, by)
}
func (c *countingMaster) Setting(ctx context.Context, name string) (string, error) {
	c.count()
	return c.Store.Setting(ctx, name)
}
func (c *countingMaster) SetSetting(ctx context.Context, name, value string, a master.AuditEntry, by master.Actor) error {
	c.count()
	return c.Store.SetSetting(ctx, name, value, a, by)
}
func (c *countingMaster) UnsetSetting(ctx context.Context, name string, a master.AuditEntry, by master.Actor) (bool, error) {
	c.count()
	return c.Store.UnsetSetting(ctx, name, a, by)
}
func (c *countingMaster) ListSettings(ctx context.Context, p master.Page) ([]master.Setting, string, error) {
	c.count()
	return c.Store.ListSettings(ctx, p)
}
func (c *countingMaster) AppendAudit(ctx context.Context, entries ...master.AuditEntry) error {
	c.count()
	return c.Store.AppendAudit(ctx, entries...)
}
func (c *countingMaster) ListAudit(ctx context.Context, f master.AuditFilter, p master.Page) ([]master.AuditEntry, string, error) {
	c.count()
	return c.Store.ListAudit(ctx, f, p)
}

// memMaster is a master held in memory, with only what the registry calls,
// for the test of 1,000 tenants: writing 1,000 rows to a SQLite master with
// a synced commit each would measure the master's disk, not the registry.
// The registry's behavior against a real master is tested everywhere else.
type memMaster struct {
	master.Store // nil: a method the registry should not call panics
	mu           sync.Mutex
	tenants      map[string]master.Tenant // by name
	writes       atomic.Int64
}

// newMemMaster returns an empty memMaster.
func newMemMaster() *memMaster { return &memMaster{tenants: map[string]master.Tenant{}} }

// ListTenants lists every tenant in one page, in no order.
func (m *memMaster) ListTenants(_ context.Context, _ master.Page) ([]master.Tenant, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]master.Tenant, 0, len(m.tenants))
	for _, t := range m.tenants {
		out = append(out, t)
	}
	return out, "", nil
}

// SetTenantState sets a tenant's state.
func (m *memMaster) SetTenantState(_ context.Context, id string, s master.TenantState, reason string, _ master.AuditEntry, _ master.Actor) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.writes.Add(1)
	for name, t := range m.tenants {
		if t.ID == id {
			t.State, t.StateReason = s, reason
			m.tenants[name] = t
			return nil
		}
	}
	return fault.New(fault.NotFound, "no such tenant")
}

// add adds a tenant row as Create would leave it, sealed with keys.
func (m *memMaster) add(t testing.TB, keys *secret.Keys, id, name, drv, address string, state master.TenantState) {
	t.Helper()
	sealed, err := keys.Seal(addressContext(id), []byte(address))
	if err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tenants[name] = master.Tenant{ID: id, Name: name, Driver: drv, SealedAddress: sealed, State: state, Created: t0}
}

// mustNotContain fails if text holds any of the needles.
func mustNotContain(t testing.TB, what, text string, needles ...string) {
	t.Helper()
	for _, n := range needles {
		if n != "" && strings.Contains(text, n) {
			t.Errorf("%s holds %q: %s", what, n, text)
		}
	}
}

// waitFor polls cond every millisecond until it holds or 30 seconds pass:
// an upper bound for a slow runner, not a measure of speed.
func waitFor(t testing.TB, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// countingDriver wraps a driver and records the most opens that ran at
// once, to show the limit on opens holds.
type countingDriver struct {
	Driver
	opening, maxOpening atomic.Int64
}

// Open counts the open while it runs.
func (d *countingDriver) Open(ctx context.Context, id, name, address string) (tenant.Store, error) {
	n := d.opening.Add(1)
	defer d.opening.Add(-1)
	for {
		m := d.maxOpening.Load()
		if n <= m || d.maxOpening.CompareAndSwap(m, n) {
			break
		}
	}
	return d.Driver.Open(ctx, id, name, address)
}

// openFiles returns how many files the process has open, read from
// /dev/fd (Linux and macOS), or -1 where that cannot be read.
func openFiles() int {
	// Names only: on macOS a stat of each entry fails for some of them, so
	// os.ReadDir would give up on the whole directory.
	d, err := os.Open("/dev/fd")
	if err != nil {
		return -1
	}
	defer func() { _ = d.Close() }()
	names, err := d.Readdirnames(-1)
	if err != nil {
		return -1
	}
	return len(names) - 1 // less the descriptor reading it
}

// checkNow runs one timer round and waits for its checks.
func checkNow(r *Registry) { r.checkAll().Wait() }

// seed creates the test driver tenants named while their databases answer,
// through a registry it then closes, so a test can make some of them fail
// before the registry under test starts.
func (e *env) seed(names ...string) {
	e.t.Helper()
	r := e.newRegistry(e.config())
	for _, n := range names {
		e.create(r, "id-"+n, n, flakyDriver, flakyAddress(n))
	}
	if err := r.Close(context.Background()); err != nil {
		e.t.Fatal(err)
	}
}

// trickyMaster wraps a master to fail in the ways a real one can between a
// commit and its answer, and to hold a listing until released.
type trickyMaster struct {
	master.Store
	// removeCommitsThenFails makes the next RemoveTenant commit and then
	// return Unavailable, as a master whose answer is lost does.
	removeCommitsThenFails atomic.Bool
	// createCommitsThenFails does the same for the next CreateTenant.
	createCommitsThenFails atomic.Bool
	// stateFails makes every SetTenantState fail without committing.
	stateFails atomic.Bool
	// listGate, when set, holds every ListTenants until it is closed;
	// listEntered is closed when the first one reaches it.
	listGate    chan struct{}
	listEntered chan struct{}
	enterOnce   sync.Once
}

// lostAnswer is the error of a call whose commit the caller did not hear of.
var lostAnswer = fault.New(fault.Unavailable, "the master database did not answer in time")

// RemoveTenant commits, then fails when asked to.
func (m *trickyMaster) RemoveTenant(ctx context.Context, id string, a master.AuditEntry, by master.Actor) error {
	err := m.Store.RemoveTenant(ctx, id, a, by)
	if err == nil && m.removeCommitsThenFails.CompareAndSwap(true, false) {
		return lostAnswer
	}
	return err
}

// CreateTenant commits, then fails when asked to.
func (m *trickyMaster) CreateTenant(ctx context.Context, t master.Tenant, a master.AuditEntry, by master.Actor) error {
	err := m.Store.CreateTenant(ctx, t, a, by)
	if err == nil && m.createCommitsThenFails.CompareAndSwap(true, false) {
		return lostAnswer
	}
	return err
}

// SetTenantState fails without committing when asked to.
func (m *trickyMaster) SetTenantState(ctx context.Context, id string, s master.TenantState, reason string, a master.AuditEntry, by master.Actor) error {
	if m.stateFails.Load() {
		return lostAnswer
	}
	return m.Store.SetTenantState(ctx, id, s, reason, a, by)
}

// ListTenants waits at the gate when one is set.
func (m *trickyMaster) ListTenants(ctx context.Context, p master.Page) ([]master.Tenant, string, error) {
	if m.listGate != nil {
		m.enterOnce.Do(func() { close(m.listEntered) })
		<-m.listGate
	}
	return m.Store.ListTenants(ctx, p)
}
