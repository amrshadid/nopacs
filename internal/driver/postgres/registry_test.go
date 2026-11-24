package postgres_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/amrshadid/nopacs/internal/driver"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/registry"
	"github.com/amrshadid/nopacs/internal/secret"
	"github.com/amrshadid/nopacs/internal/tenant/tenanttest"
)

// This file tests the registry on real Postgres tenants. It is an external
// test package because the registry's tests, and this driver's, cannot import
// each other's internals, and package driver imports this one.

// The variables that point the tests at a server, as in the package's own
// tests (helpers_test.go): the address of a scratch database, and a switch
// that turns its absence from a skip into a failure.
const (
	addressEnv = "NOPACS_TEST_POSTGRES"
	requireEnv = "NOPACS_REQUIRE_POSTGRES"
)

// serverAddress returns the test database's address, or skips the test, or
// fails it when the Postgres tests are required.
func serverAddress(tb testing.TB) string {
	tb.Helper()
	if a := os.Getenv(addressEnv); a != "" {
		return a
	}
	if os.Getenv(requireEnv) != "" {
		tb.Fatalf("%s is not set, and %s is: the Postgres tests must run here, not be skipped", addressEnv, requireEnv)
	}
	tb.Skipf("%s is not set; the Postgres tests are skipped", addressEnv)
	return ""
}

// connect opens a connection to the database at address for a test's own
// setup, closed when tb ends.
func connect(tb testing.TB, address string) *pgx.Conn {
	tb.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, address)
	if err != nil {
		tb.Fatal("cannot connect to the test database")
	}
	tb.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer ccancel()
		_ = conn.Close(cctx) // A failed close of a test's connection hides nothing.
	})
	return conn
}

// run executes statements on conn.
func run(tb testing.TB, conn *pgx.Conn, statements ...string) {
	tb.Helper()
	for _, s := range statements {
		if _, err := conn.Exec(context.Background(), s); err != nil {
			tb.Fatalf("%s: %v", s, err)
		}
	}
}

// unique returns prefix and 12 hex digits.
func unique(tb testing.TB, prefix string) string {
	tb.Helper()
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		tb.Fatal(err)
	}
	return prefix + hex.EncodeToString(b)
}

// atDatabase returns address with its database, login and search_path
// replaced.
func atDatabase(tb testing.TB, address, database, user, password, schema string) string {
	tb.Helper()
	u, err := url.Parse(address)
	if err != nil {
		tb.Fatal("the test address is not a URL")
	}
	u.Path = "/" + database
	u.User = url.UserPassword(user, password)
	q := u.Query()
	if schema != "" {
		q.Set("search_path", schema)
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// fakeClock is the registry's clock, which a test moves by hand: the registry
// writes a state change to the master at most once a minute of this clock.
type fakeClock struct{ offset atomic.Int64 }

// Now returns a fixed start plus what the test has added.
func (c *fakeClock) Now() time.Time {
	return time.Date(2026, 11, 18, 9, 30, 0, 0, time.UTC).Add(time.Duration(c.offset.Load()))
}

// Add moves the clock forward.
func (c *fakeClock) Add(d time.Duration) { c.offset.Add(int64(d)) }

// syncBuffer is a log destination safe for the registry's goroutines.
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

// pgInstallation is a registry with Postgres tenants on a SQLite master.
type pgInstallation struct {
	t      *testing.T
	super  *pgx.Conn
	base   string
	role   string
	pw     string
	master master.Store
	r      *registry.Registry
	clock  *fakeClock
	log    *syncBuffer
	seq    atomic.Int64
}

// newInstallation makes a role with a known password, a registry with the
// Postgres driver, and a master. The registry checks every 100ms.
func newInstallation(t *testing.T) *pgInstallation {
	t.Helper()
	base := serverAddress(t)
	super := connect(t, base)
	role, pw := unique(t, "mt_reg_role_"), unique(t, "Pw-NotARealPassword-")
	run(t, super, fmt.Sprintf(`CREATE ROLE %s LOGIN PASSWORD '%s' NOSUPERUSER NOCREATEDB NOCREATEROLE`, role, pw))
	t.Cleanup(func() {
		c := connect(t, base)
		run(t, c, `DROP OWNED BY `+role, `DROP ROLE `+role)
	})

	ctx := context.Background()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // G302: a directory needs its execute bit.
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
	m, err := driver.OpenMaster(ctx, driver.SQLiteScheme+filepath.Join(dir, "master.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	if err := m.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	in := &pgInstallation{t: t, super: super, base: base, role: role, pw: pw, master: m, clock: &fakeClock{}, log: &syncBuffer{}}
	r, err := registry.New(registry.Config{
		Master: m, Keys: keys,
		Drivers: map[string]registry.Driver{driver.PostgresDriver: driver.PostgresTenants{}},
		Now:     in.clock.Now, Interval: 100 * time.Millisecond,
		OpenTimeout: 10 * time.Second, PingTimeout: 2 * time.Second, MaxOpening: 4,
		Log: slog.New(slog.NewTextHandler(in.log, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	if err != nil {
		t.Fatal(err)
	}
	in.r = r
	t.Cleanup(func() { _ = r.Close(context.Background()) })
	if err := r.Start(ctx); err != nil {
		t.Fatal(err)
	}
	return in
}

// audit returns a new audit entry for a request by the local operator.
func (in *pgInstallation) audit(method string) master.AuditEntry {
	return master.AuditEntry{
		ID: fmt.Sprintf("req-%06d", in.seq.Add(1)), Time: in.clock.Now(), LocalUser: "501",
		Method: method, Outcome: master.OutcomeDone, RequestID: fmt.Sprintf("request-%06d", in.seq.Load()),
	}
}

// byRequest returns, oldest first, every entry of the master's log whose
// request id is id, each as "method outcome", with the new state when the
// entry has one and "undone" when the entry says the creation's row was
// removed again.
func (in *pgInstallation) byRequest(id string) []string {
	in.t.Helper()
	var out []string
	page := master.Page{Limit: master.MaxPageLimit}
	for {
		entries, next, err := in.master.ListAudit(context.Background(), master.AuditFilter{}, page)
		if err != nil {
			in.t.Fatal(err)
		}
		for _, a := range entries {
			if a.RequestID != id {
				continue
			}
			line := a.Method + " " + string(a.Outcome)
			if a.Details[registry.DetailUndone] != "" {
				line += " undone"
			}
			if st := a.Details[master.DetailNewState]; st != "" {
				line += " ->" + st
			}
			out = append([]string{line}, out...)
		}
		if next == "" {
			return out
		}
		page.Cursor = next
	}
}

// wantCreationTrail fails unless the request's id leads to the entries a
// creation leaves: a system entry when the row is written, then the request's
// own entry, done with the change to ready or failed with the row removed.
func (in *pgInstallation) wantCreationTrail(request string, ready bool) {
	in.t.Helper()
	want := []string{registry.StartMethod + " done", "CreateTenant failed undone"}
	if ready {
		want = []string{registry.StartMethod + " done", "CreateTenant done ->ready"}
	}
	if got := in.byRequest(request); !slices.Equal(got, want) {
		in.t.Errorf("the entries of request %s are %q, want %q", request, got, want)
	}
}

// database makes a database owned by the installation's role and returns its
// name; it is dropped when the test ends.
func (in *pgInstallation) database() string {
	in.t.Helper()
	name := unique(in.t, "mt_regdb_")
	run(in.t, in.super, fmt.Sprintf(`CREATE DATABASE %s OWNER %s`, name, in.role))
	in.t.Cleanup(func() {
		c := connect(in.t, in.base)
		run(in.t, c, `DROP DATABASE IF EXISTS `+name+` WITH (FORCE)`)
	})
	return name
}

// address returns the address, with the role's password, of a database.
func (in *pgInstallation) address(database string) string {
	return atDatabase(in.t, in.base, database, in.role, in.pw, "")
}

// create creates a tenant on a database through the registry, and returns
// the id of the request, which leads to the entries of the creation.
func (in *pgInstallation) create(name, database string) (master.Tenant, string, error) {
	audit := in.audit("CreateTenant")
	tn, _, err := in.r.Create(context.Background(), registry.NewTenant{
		ID: "id-" + name, Name: name, Driver: driver.PostgresDriver, Address: in.address(database),
	}, audit, master.LocalOperator())
	return tn, audit.RequestID, err
}

// waitState polls until the tenant is in state, failing after 30 seconds.
func (in *pgInstallation) waitState(name string, state master.TenantState) registry.State {
	in.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		st, _ := in.r.State(name)
		if st.State == state {
			return st
		}
		if time.Now().After(deadline) {
			in.t.Fatalf("%s is %s %q after 30 seconds, want %s", name, st.State, st.Reason, state)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// everything returns every piece of text the installation produced: the log,
// each tenant's state reason in the registry and the master, and every field
// of every audit entry in the master.
func (in *pgInstallation) everything(names ...string) string {
	in.t.Helper()
	var b strings.Builder
	b.WriteString(in.log.String())
	for _, n := range names {
		st, _ := in.r.State(n)
		b.WriteString("\nreason: " + st.Reason)
		if row, err := in.master.TenantByName(context.Background(), n); err == nil {
			b.WriteString("\nrow: " + row.StateReason + " " + row.Driver)
		}
	}
	page := master.Page{Limit: master.MaxPageLimit}
	for {
		entries, next, err := in.master.ListAudit(context.Background(), master.AuditFilter{}, page)
		if err != nil {
			in.t.Fatal(err)
		}
		for _, a := range entries {
			fmt.Fprintf(&b, "\naudit: %+v", a)
		}
		if next == "" {
			return b.String()
		}
		page.Cursor = next
	}
}

// TestRegistryServesPostgresTenants guards the registry on real Postgres
// tenants: one tenant's database going down stops that tenant alone, and a
// creation is whole or not at all. It creates two tenants, each in a database
// of its own, and sees both ready. It then stops one tenant's database by
// forbidding connections to it (ALTER DATABASE ... ALLOW_CONNECTIONS false,
// which stops everyone, superusers too) and ending the sessions already open
// to it, and sees that tenant unreachable alone, the other still served, and
// the master told. When connections are allowed again it returns to ready
// with no restart, and what it held is still there. A creation against a
// database that does not answer, or does not exist, leaves no row. The
// password is searched for in every reason, error, audit entry and log line.
func TestRegistryServesPostgresTenants(t *testing.T) {
	ctx := context.Background()
	in := newInstallation(t)
	dbA, dbB := in.database(), in.database()

	tn, request, err := in.create("pga", dbA)
	if err != nil || tn.State != master.StateReady {
		t.Fatalf("creating pga: %s, %v; want ready", tn.State, err)
	}
	in.wantCreationTrail(request, true)
	if tn, _, err := in.create("pgb", dbB); err != nil || tn.State != master.StateReady {
		t.Fatalf("creating pgb: %s, %v; want ready", tn.State, err)
	}
	a, err := in.r.Store("pga")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.SetSetting(ctx, "kept", "through the outage", in.audit("SetSetting"), nil); err != nil {
		t.Fatal(err)
	}
	// Stop pga's database.
	run(t, in.super, fmt.Sprintf(`ALTER DATABASE %s WITH ALLOW_CONNECTIONS false`, dbA))
	run(t, in.super, fmt.Sprintf(`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = '%s'`, dbA))
	st := in.waitState("pga", master.StateUnreachable)
	t.Logf("pga while stopped: %s %q", st.State, st.Reason)
	if got, _ := in.r.State("pgb"); got.State != master.StateReady {
		t.Errorf("pgb is %s while pga is stopped, want ready: only the stopped tenant is unreachable", got.State)
	}
	b, err := in.r.Store("pgb")
	if err != nil {
		t.Fatalf("Store(pgb) while pga is stopped: %v", err)
	}
	if err := b.SetSetting(ctx, "alive", "yes", in.audit("SetSetting"), nil); err != nil {
		t.Errorf("pgb does not take a write while pga is stopped: %v", err)
	}
	if _, err := in.r.Store("pga"); fault.KindOf(err) != fault.Unavailable {
		t.Errorf("Store(pga) while it is stopped = %v, want Unavailable", err)
	}
	in.clock.Add(2 * registry.StateChangeInterval)
	waitMaster(t, in, "pga", master.StateUnreachable)

	// Restore it.
	run(t, in.super, fmt.Sprintf(`ALTER DATABASE %s WITH ALLOW_CONNECTIONS true`, dbA))
	in.waitState("pga", master.StateReady)
	in.clock.Add(2 * registry.StateChangeInterval)
	waitMaster(t, in, "pga", master.StateReady)
	back, err := in.r.Store("pga")
	if err != nil {
		t.Fatalf("Store(pga) after it came back: %v", err)
	}
	if v, err := back.Setting(ctx, "kept"); err != nil || v != "through the outage" {
		t.Errorf("pga reads %q, %v after the outage; want what it held", v, err)
	}

	// A creation against a database that does not answer, and one against a
	// database that does not exist, leave no row and no entry: an operator
	// who mistypes an address gets an error, not a broken tenant to clean up.
	refused := url.URL{}
	if u, err := url.Parse(in.base); err == nil {
		refused = *u
	}
	refused.Host = "127.0.0.1:1" // a port nothing listens on
	for name, address := range map[string]string{
		"nobody": atDatabase(t, refused.String(), dbB, in.role, in.pw, "") + "&connect_timeout=2",
		"nodb":   in.address("no_such_database_" + dbB),
	} {
		audit := in.audit("CreateTenant")
		_, _, err := in.r.Create(ctx, registry.NewTenant{ID: "id-" + name, Name: name, Driver: driver.PostgresDriver, Address: address}, audit, master.LocalOperator())
		in.wantCreationTrail(audit.RequestID, false)
		if fault.KindOf(err) != fault.Unavailable {
			t.Errorf("creating %s against a database that does not answer = %v, want Unavailable", name, err)
		}
		if _, merr := in.master.TenantByName(ctx, name); fault.KindOf(merr) != fault.NotFound {
			t.Errorf("the master has a row for %s after a failed creation (%v)", name, merr)
		}
		if _, known := in.r.State(name); known {
			t.Errorf("the registry knows %s after a failed creation", name)
		}
		if err != nil && strings.Contains(err.Error(), in.pw) {
			t.Errorf("the error of creating %s repeats the password: %v", name, err)
		}
	}

	// The password, and the address's pieces, are nowhere.
	text := in.everything("pga", "pgb", "nobody", "nodb")
	for _, needle := range []string{in.pw, url.QueryEscape(in.pw), in.role} {
		if strings.Contains(text, needle) {
			t.Errorf("a reason, audit entry or log line holds %q", needle)
		}
	}
	if h := hostOf(in.base); h != "" && strings.Contains(strings.ReplaceAll(text, "\nreason:", ""), "@"+h) {
		t.Errorf("the log or the audit holds the address's host with its user")
	}
}

// waitMaster polls until the master holds state for the tenant.
func waitMaster(t *testing.T, in *pgInstallation, name string, state master.TenantState) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		row, err := in.master.TenantByName(context.Background(), name)
		if err == nil && row.State == state {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the master holds %s %q for %s, want %s", row.State, row.StateReason, name, state)
		}
		in.clock.Add(registry.StateChangeInterval)
		time.Sleep(50 * time.Millisecond)
	}
}

// hostOf returns the host of an address.
func hostOf(address string) string {
	u, err := url.Parse(address)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// TestRegistryRefusesTwoTenantsOnOneDatabase guards the decision for two
// tenants pointed at one database through the registry: the second is
// refused at creation, as Invalid with no row, and the first is untouched.
func TestRegistryRefusesTwoTenantsOnOneDatabase(t *testing.T) {
	ctx := context.Background()
	in := newInstallation(t)
	db := in.database()
	if tn, _, err := in.create("first", db); err != nil || tn.State != master.StateReady {
		t.Fatalf("creating first: %s, %v", tn.State, err)
	}
	_, request, err := in.create("second", db)
	in.wantCreationTrail(request, false)
	if fault.KindOf(err) != fault.Invalid {
		t.Errorf("creating a second tenant on the same database = %v, want Invalid", err)
	}
	if _, merr := in.master.TenantByName(ctx, "second"); fault.KindOf(merr) != fault.NotFound {
		t.Errorf("the master has a row for the second tenant (%v)", merr)
	}
	if st, _ := in.r.State("first"); st.State != master.StateReady {
		t.Errorf("the first tenant is %s after the refusal, want ready", st.State)
	}
	if err != nil {
		t.Logf("refused: %s", fault.Message(err))
	}
}

// TestWriteTenantSample creates a tenant on Postgres through the registry, as
// the server will, in the schema NOPACS_SAMPLE_SCHEMA names, and writes the
// tenant suite's sample into it, for the outside check
// (scripts/check/tenant-postgres.sh), which reads it with psql and pg_dump. It
// runs only when that variable is set. The master is a Postgres one too, in
// the schema NOPACS_SAMPLE_SCHEMA + "_master", so psql reads the tenant's
// row and its state change as well. The login's password is made up and its
// absence from the dump is checked by the script.
func TestWriteTenantSample(t *testing.T) {
	schema, dir := os.Getenv("NOPACS_SAMPLE_SCHEMA"), os.Getenv("NOPACS_SAMPLE_DIR")
	if schema == "" || dir == "" {
		t.Skip("NOPACS_SAMPLE_SCHEMA and NOPACS_SAMPLE_DIR are not set; scripts/check/tenant-postgres.sh sets them")
	}
	ctx := context.Background()
	base := serverAddress(t)
	super := connect(t, base)
	masterSchema := schema + "_master"
	run(t, super, `CREATE SCHEMA `+pgx.Identifier{schema}.Sanitize(), `CREATE SCHEMA `+pgx.Identifier{masterSchema}.Sanitize())

	keyPath := filepath.Join(dir, "key")
	if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // G302: a directory needs its execute bit.
		t.Fatal(err)
	}
	if err := secret.CreateKeyFile(keyPath); err != nil {
		t.Fatal(err)
	}
	keys, err := secret.LoadKeyFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	maddr := withSchema(t, base, masterSchema)
	m, err := driver.OpenMaster(ctx, maddr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = m.Close() }()
	if err := m.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	clock := &fakeClock{}
	r, err := registry.New(registry.Config{
		Master: m, Keys: keys, Drivers: map[string]registry.Driver{driver.PostgresDriver: driver.PostgresTenants{}},
		Now: clock.Now, Interval: time.Hour, OpenTimeout: 20 * time.Second, PingTimeout: 5 * time.Second, MaxOpening: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	audit := master.AuditEntry{ID: "sample-create", Time: clock.Now(), LocalUser: "501", Method: "/nopacs.v1.TenantService/CreateTenant", TenantName: "sample", Outcome: master.OutcomeDone, RequestID: "sample-request"}
	tn, _, err := r.Create(ctx, registry.NewTenant{ID: "sample-tenant", Name: "sample", Driver: driver.PostgresDriver, Address: withSchema(t, base, schema)}, audit, master.LocalOperator())
	if err != nil {
		t.Fatal(err)
	}
	if tn.State != master.StateReady {
		t.Fatalf("the sample tenant is %s %q", tn.State, tn.StateReason)
	}
	s, err := r.Store("sample")
	if err != nil {
		t.Fatal(err)
	}
	tenanttest.WriteSample(t, s)
	if err := r.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// withSchema returns address with its search_path set to schema.
func withSchema(tb testing.TB, address, schema string) string {
	tb.Helper()
	u, err := url.Parse(address)
	if err != nil {
		tb.Fatal("the test address is not a URL")
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	return u.String()
}
