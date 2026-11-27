package postgres

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/master/mastertest"
	"github.com/amrshadid/nopacs/internal/tenant"
	"github.com/amrshadid/nopacs/internal/tenant/tenanttest"
	"github.com/amrshadid/nopacs/internal/testkit"
)

// openTenantAt opens the tenant with this id at address, closed when tb ends,
// and migrates it.
func openTenantAt(tb testing.TB, address, tenantID string) *Tenant {
	tb.Helper()
	ctx := context.Background()
	t, err := OpenTenant(ctx, address, tenantID)
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { _ = t.Close() })
	defer holdSchemaWork()()
	if err := t.Migrate(ctx); err != nil {
		tb.Fatal(err)
	}
	return t
}

// openTenantTest opens a fresh, migrated database for the tenant with this
// id in a new schema, closed when tb ends.
func openTenantTest(tb testing.TB, tenantID string) tenant.Store {
	tb.Helper()
	return openTenantAt(tb, newSchema(tb), tenantID)
}

// openTenantPair opens two stores on one new tenant database, as two
// processes would, each with a pool of its own and each migrating, for the
// suite's tests across instances.
func openTenantPair(tb testing.TB, tenantID string) (tenant.Store, tenant.Store) {
	tb.Helper()
	address := newSchema(tb)
	return openTenantAt(tb, address, tenantID), openTenantAt(tb, address, tenantID)
}

// TestTenantContract runs the tenant port's whole contract suite on
// Postgres: the store, the pipeline and the events, each also across two
// instances on one database. Each case has a schema of its own; the two
// stores of a pair share one, each with a pool of its own, as two processes
// would, so their transactions are truly at once.
func TestTenantContract(t *testing.T) {
	tenanttest.Run(t, tenanttest.Driver{
		Open: func(tb testing.TB, id string) tenanttest.EventStore { return openTenantTest(tb, id).(*Tenant) },
		OpenPair: func(tb testing.TB, id string) (tenanttest.EventStore, tenanttest.EventStore) {
			a, b := openTenantPair(tb, id)
			return a.(*Tenant), b.(*Tenant)
		},
		Scratch: func(tb testing.TB) string {
			s, ok := testkit.PostgresScratch(tb)
			if !ok {
				tb.Skip("no Postgres to make a scratch schema in")
			}
			return s
		},
	})
}

// TestShippedTenantMigrationsUnchanged guards a tenant's migration files
// against an edit nobody meant: a changed baseline makes every tenant
// database made from it refuse to open (sqlkit.ErrEarlierBuild), a changed
// later file every database that applied it (sqlkit.ErrChecksum). Until a
// first release the baseline is edited in place and its checksum here with
// it, on purpose, since every database made before is then made again; from
// the first release on a listed checksum is never changed, and a new file
// is added to the list.
func TestShippedTenantMigrationsUnchanged(t *testing.T) {
	want := []string{"0001_baseline.sql " + shippedTenantBaseline}
	if tenantMigrationsErr != nil {
		t.Fatal(tenantMigrationsErr)
	}
	if len(tenantMigrations) < len(want) {
		t.Fatalf("%d migrations, want at least %d", len(tenantMigrations), len(want))
	}
	for i, w := range want {
		m := tenantMigrations[i]
		if got := m.Name + " " + hex.EncodeToString(m.Sum[:]); got != w {
			t.Errorf("migration %d is %s, want %s: an applied migration is never changed", i+1, got, w)
		}
	}
}

// shippedTenantBaseline is the SHA-256 of migrations/tenant/0001_baseline.sql
// as this change ships it.
const shippedTenantBaseline = "dd6fa47f17a5e90cf1376f440823f0ca41f60f94ab23a485311e78ab035fa355"

// TestTenantNotMigrated guards that a tenant store used before Migrate fails
// as Unavailable with a message that says so, instead of a SQL error, and
// reads as version 0.
func TestTenantNotMigrated(t *testing.T) {
	s, err := OpenTenant(context.Background(), newSchema(t), "t1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if v, err := s.Version(context.Background()); err != nil || v != 0 {
		t.Errorf("Version before Migrate = %d, %v; want 0", v, err)
	}
	_, err = s.Setting(context.Background(), "a")
	if fault.KindOf(err) != fault.Unavailable || !strings.Contains(fault.Message(err), "migrated") {
		t.Errorf("Setting before Migrate = %v, want Unavailable saying it is not migrated", err)
	}
}

// TestTenantMessagesName guards that a tenant store's messages say "tenant
// database", never "master": a failure of a tenant's database is shown in the
// tenant's state reason.
func TestTenantMessagesName(t *testing.T) {
	_, err := OpenTenant(context.Background(), "postgres://needleuser:hunter2needle@127.0.0.1:1/needledb?sslmode=disable&connect_timeout=2", "t1")
	if fault.KindOf(err) != fault.Unavailable {
		t.Fatalf("a port nothing listens on: %v, want Unavailable", err)
	}
	if m := fault.Message(err); !strings.Contains(m, "tenant database") || strings.Contains(m, "master") {
		t.Errorf("message %q does not say it was the tenant database", m)
	}
	noSecrets(t, "a tenant open", err, "127.0.0.1")
	if _, err := OpenTenant(context.Background(), "postgres://127.0.0.1/db", "a/b"); fault.KindOf(err) != fault.Invalid {
		t.Errorf("a bad tenant id: %v, want Invalid", err)
	}
}

// TestTwoTenantsOnOneDatabase guards the decision for two tenants pointed at
// one database and schema: the second is refused as Invalid and nothing of
// the first is touched. The schema records the id of the tenant it was
// migrated for (tenant_identity). The refusal comes at open when the
// database is already claimed, and at Migrate when both were opened before
// either migrated; two instances of one tenant are not refused.
func TestTwoTenantsOnOneDatabase(t *testing.T) {
	ctx := context.Background()
	address := newSchema(t)
	audit := func(id string) master.AuditEntry {
		return master.AuditEntry{ID: id, Time: mastertest.T0, Method: "Test", Outcome: master.OutcomeDone}
	}

	// Both open before either migrates: nothing is recorded yet, so both
	// open; the first to migrate claims the schema, the second is refused.
	a, err := OpenTenant(ctx, address, "tenant-a")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	b, err := OpenTenant(ctx, address, "tenant-b")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Close() })
	if err := a.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	err = b.Migrate(ctx)
	if fault.KindOf(err) != fault.Invalid || !strings.Contains(fault.Message(err), "another tenant") {
		t.Fatalf("the second tenant's Migrate = %v, want Invalid saying the database belongs to another tenant", err)
	}
	t.Logf("refused at Migrate: %s", fault.Message(err))

	// Opening once the schema is claimed is refused at once.
	_, err = OpenTenant(ctx, address, "tenant-b")
	if fault.KindOf(err) != fault.Invalid || !strings.Contains(fault.Message(err), "another tenant") {
		t.Errorf("a second tenant's open = %v, want Invalid at once", err)
	}

	// The first tenant is as it was, and a second instance of it is let in.
	if err := a.SetSetting(ctx, "mine", "yes", audit("a-1"), nil); err != nil {
		t.Fatal(err)
	}
	again := openTenantAt(t, address, "tenant-a")
	if v, err := again.Setting(ctx, "mine"); err != nil || v != "yes" {
		t.Errorf("another instance of the first tenant reads %q, %v", v, err)
	}
	// Nothing of the refused tenant was written: its setting call finds its
	// own failure, not the first tenant's data.
	if _, err := b.Setting(ctx, "mine"); err != nil {
		t.Logf("the refused store reads: %v", err)
	}
	var rows int
	if err := a.pool.QueryRow(ctx, `SELECT count(*) FROM tenant_identity`).Scan(&rows); err != nil || rows != 1 {
		t.Errorf("tenant_identity holds %d rows, %v; want exactly the first tenant's", rows, err)
	}
	var recorded string
	if err := a.pool.QueryRow(ctx, `SELECT id FROM tenant_identity`).Scan(&recorded); err != nil || recorded != "tenant-a" {
		t.Errorf("tenant_identity holds %q, %v; want tenant-a", recorded, err)
	}
}

// TestClaimRace guards the claim under concurrency: tenants opened at once on
// one new schema, each trying to migrate it, end with exactly one that
// succeeded and every other refused as Invalid, and one row.
func TestClaimRace(t *testing.T) {
	ctx := context.Background()
	address := newSchema(t)
	const racers = 6
	stores := make([]*Tenant, racers)
	for i := range stores {
		s, err := OpenTenant(ctx, address, fmt.Sprintf("racer-%d", i))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Close() })
		stores[i] = s
	}
	errs := make([]error, racers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range stores {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs[i] = stores[i].Migrate(ctx)
		}()
	}
	close(start)
	wg.Wait()
	winners := 0
	for i, err := range errs {
		switch {
		case err == nil:
			winners++
		case fault.KindOf(err) != fault.Invalid:
			t.Errorf("racer %d: %v, want Invalid", i, err)
		}
	}
	if winners != 1 {
		t.Errorf("%d tenants claimed one schema, want 1", winners)
	}
}

// TestTenantPoolDefaults guards the pool's size: a tenant's pool is small by
// default (DefaultTenantConnections) and its idle connections are let go
// after TenantIdleTime, an address's pool_max_conns and pool_max_conn_idle_time
// win, and the pool is never smaller than the two connections Migrate needs.
func TestTenantPoolDefaults(t *testing.T) {
	base := testAddress(t)
	for _, tt := range []struct {
		params map[string]string
		conns  int32
		idle   time.Duration
	}{
		// The defaults are written out, not read from the constants, so
		// changing one is a decision this test shows.
		{nil, 4, time.Minute},
		{map[string]string{"pool_max_conns": "9"}, 9, time.Minute},
		{map[string]string{"pool_max_conns": "1"}, 2, time.Minute},
		{map[string]string{"pool_max_conn_idle_time": "7s"}, 4, 7 * time.Second},
	} {
		s, err := OpenTenant(context.Background(), withParams(t, base, tt.params), "t1")
		if err != nil {
			t.Fatal(err)
		}
		if got := s.pool.Stat().MaxConns(); got != tt.conns {
			t.Errorf("%v: pool of %d connections, want %d", tt.params, got, tt.conns)
		}
		if got := s.pool.Config().MaxConnIdleTime; got != tt.idle {
			t.Errorf("%v: idle time %v, want %v", tt.params, got, tt.idle)
		}
		_ = s.Close()
	}
}

// TestTenantPoolIsNotLeaked guards that no connection stays out of the pool
// after calls that succeed, calls that fail, calls with a context already
// canceled and many calls at once, and that after Close none is open.
func TestTenantPoolIsNotLeaked(t *testing.T) {
	ctx := context.Background()
	s := openTenantAt(t, newSchema(t), "t1")
	n := 0
	next := func() master.AuditEntry {
		n++
		return master.AuditEntry{ID: fmt.Sprintf("leak-%04d", n), Time: mastertest.T0, Method: "Test", Outcome: master.OutcomeDone}
	}
	if err := s.SetSetting(ctx, "a", "1", next(), nil); err != nil {
		t.Fatal(err)
	}
	dup := next()
	_ = s.AppendAudit(ctx, dup)
	if err := s.AppendAudit(ctx, dup); !errors.Is(err, master.ErrDuplicateAuditID) {
		t.Errorf("a duplicate audit id: %v", err)
	}
	_, _ = s.Setting(ctx, "nope")
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := s.SetSetting(canceled, "b", "1", next(), nil); fault.KindOf(err) != fault.Unavailable {
		t.Errorf("SetSetting with a canceled context = %v, want Unavailable", err)
	}
	var wg sync.WaitGroup
	for range 40 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = s.Setting(ctx, "a")
			_, _, _ = s.ListSettings(ctx, master.Page{Limit: 5})
			_, _, _ = s.ListAudit(ctx, master.AuditFilter{}, master.Page{Limit: 5})
		}()
	}
	wg.Wait()
	if got := settledAcquired(&s.base); got != 0 {
		t.Errorf("%d connections are out of the pool after the calls, want 0", got)
	}
	pool := s.pool
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if got := pool.Stat().TotalConns(); got != 0 {
		t.Errorf("%d connections are open after Close, want 0", got)
	}
}

// TestTenantMigrationLockWaits guards that a tenant's Migrate takes the same
// advisory lock as the master's: while another session holds it, Migrate waits
// and gives up with Unavailable at its deadline, applying nothing.
func TestTenantMigrationLockWaits(t *testing.T) {
	address := newSchema(t)
	holder := admin(t, address)
	holdLock(t, holder)
	s, err := OpenTenant(context.Background(), address, "t1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	err = s.Migrate(ctx)
	if fault.KindOf(err) != fault.Unavailable || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Migrate while the lock was held = %v, want Unavailable by the deadline", err)
	}
	if v, err := s.Version(context.Background()); err != nil || v != 0 {
		t.Errorf("Version after the refused Migrate = %d, %v; want 0", v, err)
	}
	mustExec(t, holder, `SELECT pg_advisory_unlock(`+lockName+`)`)
	if err := s.Migrate(context.Background()); err != nil {
		t.Errorf("Migrate after the lock was released: %v", err)
	}
	if !lockAvailable(t, address) {
		t.Error("the migration lock is still held after Migrate")
	}
}

// TestTenantAndMasterApart guards that a master's schema opened as a tenant's
// is refused, and a tenant's as a master's: their applied migrations differ,
// so a mistyped address never writes one's tables into the other.
func TestTenantAndMasterApart(t *testing.T) {
	ctx := context.Background()
	address := newSchema(t)
	m := openAt(t, address)
	if err := m.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	s, err := OpenTenant(ctx, address, "t1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Migrate(ctx); err == nil {
		t.Error("a master database was migrated as a tenant's")
	}
}

// TestTenantRefusesMasterCursors guards that a tenant's lists are named apart
// from the master's in their cursors: a page token from the master's settings
// or audit list, with the same filter, is refused by the tenant's.
func TestTenantRefusesMasterCursors(t *testing.T) {
	ctx := context.Background()
	m := openTest(t)
	s := openTenantTest(t, "t1")
	for _, n := range []string{"s0", "s1", "s2"} {
		a := master.AuditEntry{ID: "a-" + n, Time: mastertest.T0, Method: "Test", Outcome: master.OutcomeDone}
		if err := m.SetSetting(ctx, n, "v", a, master.LocalOperator()); err != nil {
			t.Fatal(err)
		}
	}
	_, settings, err := m.ListSettings(ctx, master.Page{Limit: 1})
	if err != nil || settings == "" {
		t.Fatalf("ListSettings = %q, %v", settings, err)
	}
	_, audit, err := m.ListAudit(ctx, master.AuditFilter{}, master.Page{Limit: 1})
	if err != nil || audit == "" {
		t.Fatalf("ListAudit = %q, %v", audit, err)
	}
	if _, _, err := s.ListSettings(ctx, master.Page{Limit: 1, Cursor: settings}); fault.KindOf(err) != fault.Invalid {
		t.Errorf("the tenant's ListSettings took the master's cursor: %v", err)
	}
	if _, _, err := s.ListAudit(ctx, master.AuditFilter{}, master.Page{Limit: 1, Cursor: audit}); fault.KindOf(err) != fault.Invalid {
		t.Errorf("the tenant's ListAudit took the master's cursor: %v", err)
	}
}

// TestTenantLeastPrivilege guards that a tenant works with a login that owns
// one schema and nothing else, as the master does.
func TestTenantLeastPrivilege(t *testing.T) {
	ctx := context.Background()
	base := testAddress(t)
	super := admin(t, base)
	role, schema, password := randomName(t, "mt_trole_"), randomName(t, "mt_town_"), randomName(t, "pw")
	mustExec(t, super,
		fmt.Sprintf(`CREATE ROLE %s LOGIN PASSWORD '%s' NOSUPERUSER NOCREATEDB NOCREATEROLE`, role, password),
		fmt.Sprintf(`CREATE SCHEMA %s AUTHORIZATION %s`, schema, role))
	t.Cleanup(func() {
		c := admin(t, base)
		mustExec(t, c, `DROP SCHEMA `+schema+` CASCADE`, `DROP OWNED BY `+role, `DROP ROLE `+role)
	})
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	u.User = url.UserPassword(role, password)
	address := withParams(t, u.String(), map[string]string{"search_path": schema})
	s := openTenantAt(t, address, "t1")
	if err := s.SetSetting(ctx, "a", "1", master.AuditEntry{ID: "lp-1", Time: mastertest.T0, Method: "Test", Outcome: master.OutcomeDone}, nil); err != nil {
		t.Fatal(err)
	}
	var inPublic int
	if err := super.QueryRow(ctx, `SELECT count(*) FROM information_schema.tables WHERE table_schema = 'public' AND table_name IN ('settings', 'audit', 'tenant_identity')`).Scan(&inPublic); err != nil || inPublic != 0 {
		t.Errorf("%d of the tenant's tables were made in schema public, %v", inPublic, err)
	}
}

// TestTenantErrorsNeverCarryTheAddress guards that no error from a tenant's
// open repeats the password, user, host or database name: a wrong login and a
// missing database on a real server.
func TestTenantErrorsNeverCarryTheAddress(t *testing.T) {
	ctx := context.Background()
	u, err := url.Parse(testAddress(t))
	if err != nil {
		t.Fatal(err)
	}
	host := u.Hostname()
	wrong := *u
	wrong.User = url.UserPassword("needleuser", "hunter2needle")
	_, err = OpenTenant(ctx, wrong.String(), "t1")
	if fault.KindOf(err) != fault.Unavailable {
		t.Errorf("a login the server does not know: %v, want Unavailable", err)
	}
	noSecrets(t, "a wrong login", err, host)
	nodb := *u
	nodb.Path = "/needledb"
	_, err = OpenTenant(ctx, nodb.String(), "t1")
	if fault.KindOf(err) != fault.Unavailable {
		t.Errorf("a database that does not exist: %v, want Unavailable", err)
	}
	noSecrets(t, "a missing database", err, host)
	if pw, ok := u.User.Password(); ok && err != nil && strings.Contains(err.Error(), pw) {
		t.Error("a missing database: the error repeats the real password")
	}
}

// TestLocation guards what an operator reads of a tenant's address: host,
// port and database for a TCP address, directory and database for a socket,
// several hosts joined, and never the user or the password; an address that
// is not usable is Invalid with a message that repeats none of it.
func TestLocation(t *testing.T) {
	t.Setenv("PGSSLMODE", "")
	for _, tt := range []struct{ address, want string }{
		{"postgres://u:pw%40x@db.example:6543/lab?sslmode=verify-full", "db.example:6543/lab"},
		{"postgresql://u:pw@db.example/lab?sslmode=require", "db.example:5432/lab"},
		{"postgres://u:pw@127.0.0.1/lab", "127.0.0.1:5432/lab"},
		{"postgres://u:pw@%2Fvar%2Frun%2Fpostgresql/lab", "unix:/var/run/postgresql/lab"},
		{"postgres://u:pw@[::1]:5433/lab", "[::1]:5433/lab"},
		{"postgres://u:pw@a.example:1,b.example:2/lab?sslmode=disable", "a.example:1/lab,b.example:2/lab"},
	} {
		got, err := Location(tt.address)
		if err != nil || got != tt.want {
			t.Errorf("Location(%q) = %q, %v; want %q", tt.address, got, err, tt.want)
		}
		if strings.Contains(got, "pw") || strings.Contains(got, "u:") {
			t.Errorf("Location(%q) = %q holds the user or the password", tt.address, got)
		}
	}
	for _, address := range []string{
		"", "sqlite:/tmp/x.db", "host=db user=needleuser password=hunter2needle",
		"postgres://needleuser:hunter2needle@needlehost.invalid/needledb", // remote, no sslmode
		"postgres://needleuser:hunter2needle@127.0.0.1:notaport/needledb",
	} {
		got, err := Location(address)
		if fault.KindOf(err) != fault.Invalid || got != "" {
			t.Errorf("Location(%q) = %q, %v; want Invalid", address, got, err)
		}
		noSecrets(t, "Location", err, "127.0.0.1")
	}
}

// TestTenantRequestPathPlans guards that a tenant's reads are index reads: a
// setting by name, the settings list, and every combination of the audit
// filters, each reading the index the schema names for it. It fills the tables
// past the size where a sequential scan would win and logs each plan.
func TestTenantRequestPathPlans(t *testing.T) {
	ctx := context.Background()
	address := newSchema(t)
	openTenantAt(t, address, "t1")
	conn := admin(t, address)
	mustExec(t, conn,
		`INSERT INTO settings (name, value) SELECT 'setting.' || lpad(n::text, 5, '0'), 'v' FROM generate_series(1, 5000) n`,
		`INSERT INTO audit (id, at, key_id, local_user, remote_addr, method, tenant_name, object, outcome, reason, details, request_id)
		 SELECT 'a' || lpad(n::text, 7, '0'), timestamptz '2026-11-01' + n * interval '8 seconds', 'Key' || lpad((n % 1000)::text, 6, '0'), '', '', 'M', 'tenant' || (n % 50),
		        '', CASE WHEN n % 200 = 0 THEN 'failed' WHEN n % 20 = 0 THEN 'refused' ELSE 'done' END, '', '{}', 'r' FROM generate_series(1, 100000) n`,
		`ANALYZE`)
	type planCase struct {
		name, statement string
		args            []any
		want            string
	}
	cases := []planCase{
		{"setting by name", `SELECT value FROM settings WHERE name = 'setting.02500'`, nil, "settings_pkey"},
		{"settings list, next page", `SELECT name, value FROM settings WHERE name > 'setting.02500' ORDER BY name LIMIT 1001`, nil, "settings_pkey"},
	}
	since, until := time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC), time.Date(2026, 11, 3, 6, 0, 0, 0, time.UTC)
	after := &auditPosition{at: time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC), id: "a0300000"}
	for _, a := range []struct {
		name   string
		filter master.AuditFilter
		after  *auditPosition
		want   string
	}{
		{"no filter", master.AuditFilter{}, nil, "audit_by_time"},
		{"no filter, next page", master.AuditFilter{}, after, "audit_by_time"},
		{"tenant", master.AuditFilter{TenantName: "tenant7"}, nil, "audit_by_tenant"},
		{"key", master.AuditFilter{KeyID: "Key000007"}, nil, "audit_by_key"},
		{"outcome failed", master.AuditFilter{Outcome: master.OutcomeFailed}, nil, "audit_by_outcome"},
		{"outcome failed, next page", master.AuditFilter{Outcome: master.OutcomeFailed}, after, "audit_by_outcome"},
		{"time range", master.AuditFilter{Since: since, Until: until}, nil, "audit_by_time"},
		{"tenant and key", master.AuditFilter{TenantName: "tenant7", KeyID: "Key000007"}, nil, "audit_by_"},
		{"key and outcome", master.AuditFilter{KeyID: "Key000007", Outcome: master.OutcomeFailed}, nil, "audit_by_"},
		{"tenant, key and outcome", master.AuditFilter{TenantName: "tenant7", KeyID: "Key000007", Outcome: master.OutcomeFailed}, nil, "audit_by_"},
		{"outcome failed and a time range", master.AuditFilter{Outcome: master.OutcomeFailed, Since: since, Until: until}, nil, "audit_by_"},
	} {
		q, args := auditQuery(a.filter, a.after, 1001)
		cases = append(cases, planCase{"audit: " + a.name, q, args, a.want})
	}
	_ = ctx
	for _, c := range cases {
		plan := explain(t, conn, c.statement, c.args...)
		t.Logf("%s:\n%s", c.name, plan)
		if !strings.Contains(plan, c.want) || strings.Contains(plan, "Seq Scan") {
			t.Errorf("%s: the plan does not use %q, or scans the table:\n%s", c.name, c.want, plan)
		}
	}
}

// TestTenantCollationDoesNotChangeOrder guards that a tenant's settings and
// audit ids order byte by byte whatever collation the database was made with:
// in a database whose default collation is en_US.UTF-8, which ignores
// punctuation and puts "a1" before "a-b", every list is checked against byte
// order. A column of the tenant schema left to the database's default
// collation fails here.
func TestTenantCollationDoesNotChangeOrder(t *testing.T) {
	ctx := context.Background()
	base := testAddress(t)
	super := admin(t, base)
	db := randomName(t, "mt_tcoll_")
	if _, err := super.Exec(ctx, `CREATE DATABASE `+db+` TEMPLATE template0 ENCODING 'UTF8' LC_COLLATE 'en_US.utf8' LC_CTYPE 'en_US.utf8'`); err != nil {
		t.Skipf("the server has no en_US.utf8 locale to make a differently collated database with: %v", err)
	}
	t.Cleanup(func() {
		c := admin(t, base)
		mustExec(t, c, `DROP DATABASE `+db+` WITH (FORCE)`)
	})
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + db
	s := openTenantAt(t, newSchemaAt(t, u.String()), "t1")

	n := 0
	audit := func() master.AuditEntry {
		n++
		return master.AuditEntry{ID: fmt.Sprintf("coll-%03d", n), Time: mastertest.T0, Method: "Test", Outcome: master.OutcomeDone}
	}
	settings := []string{"a.b", "a_b", "a-b", "ab", "a1", "b", "a"}
	for _, name := range settings {
		if err := s.SetSetting(ctx, name, "v", audit(), nil); err != nil {
			t.Fatal(err)
		}
	}
	ids := []string{"Ab", "aB", "a_b", "a-b", "B1", "b1", "_x", "Zed"}
	var batch []master.AuditEntry
	for _, id := range ids {
		batch = append(batch, master.AuditEntry{ID: "z" + id, Time: mastertest.T0.Add(time.Hour), Method: "Test", Outcome: master.OutcomeDone})
	}
	if err := s.AppendAudit(ctx, batch...); err != nil {
		t.Fatal(err)
	}

	var gotSettings, gotAudit []string
	for page := (master.Page{Limit: 2}); ; {
		ss, next, err := s.ListSettings(ctx, page)
		if err != nil {
			t.Fatal(err)
		}
		for _, x := range ss {
			gotSettings = append(gotSettings, x.Name)
		}
		if next == "" {
			break
		}
		page.Cursor = next
	}
	for page := (master.Page{Limit: 3}); ; {
		es, next, err := s.ListAudit(ctx, master.AuditFilter{Since: mastertest.T0.Add(30 * time.Minute)}, page)
		if err != nil {
			t.Fatal(err)
		}
		for _, x := range es {
			gotAudit = append(gotAudit, x.ID)
		}
		if next == "" {
			break
		}
		page.Cursor = next
	}
	wantSettings := slices.Clone(settings)
	slices.Sort(wantSettings)
	var wantAudit []string
	for _, id := range ids {
		wantAudit = append(wantAudit, "z"+id)
	}
	slices.Sort(wantAudit)
	slices.Reverse(wantAudit) // Newest first, and at one time the larger id first.
	if !slices.Equal(gotSettings, wantSettings) {
		t.Errorf("ListSettings: %v, want byte order %v", gotSettings, wantSettings)
	}
	if !slices.Equal(gotAudit, wantAudit) {
		t.Errorf("ListAudit: %v, want %v", gotAudit, wantAudit)
	}
}

// TestTenantFailuresHideSecrets runs every failure the Postgres tenant driver
// can produce through the suite's check that no error, in its message or its
// full text, holds the password, the user or the host (and the database name):
// a wrong password, a database that does not exist, a refused connection, a
// host that does not resolve, another tenant's database (at open and at
// Migrate), an address that does not parse, a keyword/value string, and a
// remote address with no sslmode. The login and password are made up so a
// plain search finds them.
func TestTenantFailuresHideSecrets(t *testing.T) {
	ctx := context.Background()
	t.Setenv("PGSSLMODE", "")
	base := testAddress(t)
	super := admin(t, base)
	user, password, dbName := randomName(t, "needle_user_"), randomName(t, "Needle-Pw-"), randomName(t, "needle_db_")
	mustExec(t, super, fmt.Sprintf(`CREATE ROLE %s LOGIN PASSWORD '%s' NOSUPERUSER NOCREATEDB NOCREATEROLE`, user, password))
	t.Cleanup(func() {
		c := admin(t, base)
		mustExec(t, c, `DROP OWNED BY `+user, `DROP ROLE `+user)
	})
	bu, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	host := bu.Hostname()
	as := func(path, pw, query string) string {
		u := *bu
		u.User = url.UserPassword(user, pw)
		u.Path = path
		u.RawQuery = query
		return u.String()
	}
	secrets := []string{password, user, host, dbName, url.QueryEscape(password), "needle-host.invalid"}

	// Another tenant's database: a schema the login owns, claimed by tenant-a.
	schema := randomName(t, "mt_fs_")
	mustExec(t, super, fmt.Sprintf(`CREATE SCHEMA %s AUTHORIZATION %s`, schema, user))
	t.Cleanup(func() {
		c := admin(t, base)
		mustExec(t, c, `DROP SCHEMA `+schema+` CASCADE`)
	})
	owned := as(bu.Path, password, "sslmode=disable&search_path="+schema)
	// A store for tenant-b opened before the schema was claimed, to migrate
	// after: it is let in at open and refused at Migrate.
	late, err := OpenTenant(ctx, owned, "tenant-b")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = late.Close() })
	a, err := OpenTenant(ctx, owned, "tenant-a")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	if err := a.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	type failure struct {
		name string
		kind fault.Kind
		err  error
	}
	var failures []failure
	add := func(name string, kind fault.Kind, err error) { failures = append(failures, failure{name, kind, err}) }

	_, err = OpenTenant(ctx, as(bu.Path, "Wrong-"+password, "sslmode=disable"), "t1")
	add("a wrong password", fault.Unavailable, err)
	_, err = OpenTenant(ctx, as("/"+dbName, password, "sslmode=disable"), "t1")
	add("a database that does not exist", fault.Unavailable, err)
	refused := *bu
	refused.Host = "127.0.0.1:1"
	refused.User = url.UserPassword(user, password)
	refused.Path = "/" + dbName
	refused.RawQuery = "sslmode=disable&connect_timeout=2"
	_, err = OpenTenant(ctx, refused.String(), "t1")
	add("a refused connection", fault.Unavailable, err)
	nohost := refused
	nohost.Host = "needle-host.invalid"
	_, err = OpenTenant(ctx, nohost.String(), "t1")
	add("a host that does not resolve", fault.Unavailable, err)
	_, err = OpenTenant(ctx, owned, "tenant-c")
	add("another tenant's database, at open", fault.Invalid, err)
	add("another tenant's database, at Migrate", fault.Invalid, late.Migrate(ctx))
	_, err = OpenTenant(ctx, "postgres://"+user+":"+password+"@"+host+":notaport/"+dbName, "t1")
	add("an address that does not parse", fault.Invalid, err)
	_, err = OpenTenant(ctx, "host="+host+" user="+user+" password="+password+" dbname="+dbName, "t1")
	add("a keyword/value string", fault.Invalid, err)
	_, err = OpenTenant(ctx, "postgres://"+user+":"+password+"@needle-host.invalid/"+dbName, "t1")
	add("a remote address with no sslmode", fault.Invalid, err)
	_, err = Location("postgres://" + user + ":" + password + "@needle-host.invalid/" + dbName)
	add("Location of a remote address with no sslmode", fault.Invalid, err)
	_, err = Location("postgres://" + user + ":" + password + "@" + host + ":notaport/" + dbName)
	add("Location of an address that does not parse", fault.Invalid, err)

	for _, f := range failures {
		if got := fault.KindOf(f.err); got != f.kind {
			t.Errorf("%s: kind %v (%v), want %v", f.name, got, f.err, f.kind)
		}
		tenanttest.CheckErrorHidesSecrets(t, f.err, secrets...)
		if f.err != nil {
			t.Logf("%s: %s", f.name, fault.Message(f.err))
		}
	}
}
