package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/master/mastertest"
)

// TestShippedMigrationsUnchanged guards the master's migration files against
// an edit after release: an applied file that changes makes every database
// that applied it refuse to start (sqlkit.ErrChecksum). A new file is added
// to the list; a listed checksum is never changed.
func TestShippedMigrationsUnchanged(t *testing.T) {
	want := []string{
		"0001_init.sql " + shippedInit,
	}
	if masterMigrationsErr != nil {
		t.Fatal(masterMigrationsErr)
	}
	if len(masterMigrations) < len(want) {
		t.Fatalf("%d migrations, want at least %d", len(masterMigrations), len(want))
	}
	for i, w := range want {
		m := masterMigrations[i]
		if got := m.Name + " " + hex.EncodeToString(m.Sum[:]); got != w {
			t.Errorf("migration %d is %s, want %s: an applied migration is never changed", i+1, got, w)
		}
	}
}

// shippedInit is the SHA-256 of 0001_init.sql as this change ships it.
const shippedInit = "0929aba62fc03e2588556d2886e81ff228d5b19d45cb20c4472dbc03c3ae7ade"

// TestNotMigrated guards that a store used before Migrate fails as
// Unavailable with a message that says so, instead of a SQL error.
func TestNotMigrated(t *testing.T) {
	m := openUnmigrated(t)
	if v, err := m.Version(context.Background()); err != nil || v != 0 {
		t.Errorf("Version before Migrate = %d, %v; want 0", v, err)
	}
	_, err := m.Key(context.Background(), "k")
	if fault.KindOf(err) != fault.Unavailable || !strings.Contains(fault.Message(err), "migrated") {
		t.Errorf("Key before Migrate = %v, want Unavailable saying it is not migrated", err)
	}
}

// TestSchemaIsWhereTheSearchPathPoints guards that the tables are made in the
// schema the address names and nowhere else, so a login that owns only that
// schema is enough.
func TestSchemaIsWhereTheSearchPathPoints(t *testing.T) {
	ctx := context.Background()
	address := newSchema(t)
	m := openAt(t, address)
	if err := m.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(address)
	if err != nil {
		t.Fatal(err)
	}
	schema := u.Query().Get("search_path")
	conn := admin(t, address)
	rows, err := conn.Query(ctx, `SELECT table_schema, table_name FROM information_schema.tables
		WHERE table_name IN ('tenants', 'api_keys', 'grants', 'settings', 'audit', 'schema_migrations')`)
	if err != nil {
		t.Fatal(err)
	}
	type row struct{ schema, table string }
	got, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (row, error) {
		var x row
		return x, r.Scan(&x.schema, &x.table)
	})
	if err != nil {
		t.Fatal(err)
	}
	// Other tests' schemas are in the database too, so the check is that all
	// six tables are in this one and none is in public, which is where an
	// unqualified CREATE TABLE goes when the search_path is not honored.
	own := 0
	for _, r := range got {
		switch r.schema {
		case schema:
			own++
		case "public":
			t.Errorf("table %s.%s was made in schema public", r.schema, r.table)
		}
	}
	if own != 6 {
		t.Errorf("%d of the six tables are in the schema %s", own, schema)
	}
}

// TestLeastPrivilege guards that the driver works with a login that owns one
// schema and nothing else: no right to make a database or a role, and no
// table made anywhere but in its schema. It makes the login for the test and
// drops it after.
func TestLeastPrivilege(t *testing.T) {
	ctx := context.Background()
	base := testAddress(t)
	super := admin(t, base)
	role, schema := randomName(t, "mt_role_"), randomName(t, "mt_own_")
	password := randomName(t, "pw")
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
	address := withParams(t, u.String(), map[string]string{"search_path": schema, "pool_max_conns": "3"})

	m := openAt(t, address)
	if err := m.Migrate(ctx); err != nil {
		t.Fatalf("Migrate as a login that owns only its schema: %v", err)
	}
	key := master.Key{ID: "LeastPriv001", Hash: make([]byte, 32), Holder: "least", Created: mastertest.T0}
	audit := master.AuditEntry{ID: "lp-1", Time: mastertest.T0, Method: "Test", Outcome: master.OutcomeDone}
	if err := m.CreateKey(ctx, key, audit, master.LocalOperator()); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Key(ctx, key.ID); err != nil {
		t.Fatal(err)
	}
	var createDB, createRole, super2 bool
	if err := super.QueryRow(ctx, `SELECT rolcreatedb, rolcreaterole, rolsuper FROM pg_roles WHERE rolname = $1`, role).Scan(&createDB, &createRole, &super2); err != nil {
		t.Fatal(err)
	}
	if createDB || createRole || super2 {
		t.Errorf("the login can make databases %v, roles %v, is a superuser %v", createDB, createRole, super2)
	}
	var inPublic int
	if err := super.QueryRow(ctx, `SELECT count(*) FROM information_schema.tables WHERE table_schema = 'public' AND table_name IN ('tenants', 'api_keys', 'audit')`).Scan(&inPublic); err != nil {
		t.Fatal(err)
	}
	if inPublic != 0 {
		t.Errorf("%d of the master's tables were made in schema public", inPublic)
	}
}

// TestPoolIsNotLeaked guards that no connection stays out of the pool: after
// successful calls, calls that fail (not found, conflict, invalid, a context
// that is already canceled), and many calls at once, the pool's statistics
// show none acquired, and after Close none open.
func TestPoolIsNotLeaked(t *testing.T) {
	ctx := context.Background()
	m := openUnmigrated(t)
	if err := m.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	audit := func(i int) master.AuditEntry {
		return master.AuditEntry{ID: fmt.Sprintf("leak-%04d", i), Time: mastertest.T0, Method: "Test", Outcome: master.OutcomeDone}
	}
	key := master.Key{ID: "LeakKey0001", Hash: make([]byte, 32), Holder: "leak", Created: mastertest.T0}
	n := 0
	next := func() master.AuditEntry { n++; return audit(n) }
	if err := m.CreateKey(ctx, key, next(), master.LocalOperator()); err != nil {
		t.Fatal(err)
	}
	_ = m.CreateKey(ctx, key, next(), master.LocalOperator())                                          // Conflict.
	_, _ = m.Key(ctx, "nope")                                                                          // NotFound.
	_, _ = m.Key(ctx, "a/b")                                                                           // Invalid.
	_, _ = m.SetGrant(ctx, master.Grant{KeyID: "nope", Role: "admin"}, next(), master.LocalOperator()) // NotFound inside a transaction.
	_ = m.AppendAudit(ctx, next(), next())
	_, _, _ = m.ListAudit(ctx, master.AuditFilter{}, master.Page{Limit: 1})
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := m.Key(canceled, key.ID); fault.KindOf(err) != fault.Unavailable {
		t.Errorf("Key with a canceled context = %v, want Unavailable", err)
	}
	if err := m.CreateKey(canceled, master.Key{ID: "LeakKey0002", Hash: make([]byte, 32), Holder: "x", Created: mastertest.T0}, next(), master.LocalOperator()); fault.KindOf(err) != fault.Unavailable {
		t.Errorf("CreateKey with a canceled context = %v, want Unavailable", err)
	}
	var wg sync.WaitGroup
	for range 40 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = m.Key(ctx, key.ID)
			_, _, _ = m.GrantsByKey(ctx, key.ID, master.Page{Limit: 10})
			_, _ = m.MarkKeyUsed(ctx, key.ID, mastertest.T0, "")
		}()
	}
	wg.Wait()
	if got := settledAcquired(&m.base); got != 0 {
		t.Errorf("%d connections are out of the pool after the calls, want 0", got)
	}
	if got := m.pool.Stat().MaxConns(); got != 3 {
		t.Errorf("the pool's size is %d, want the 3 the address asks for", got)
	}
	pool := m.pool
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if got := pool.Stat().TotalConns(); got != 0 {
		t.Errorf("%d connections are open after Close, want 0", got)
	}
}

// TestContextDeadline guards that a deadline is honored by the database: a
// write that waits on a lock another session holds returns Unavailable at its
// deadline, in about that long, with the deadline in its chain; the
// connection it used is given back, and the same write succeeds once the lock
// is let go.
func TestContextDeadline(t *testing.T) {
	ctx := context.Background()
	address := newSchema(t)
	m := openAt(t, address)
	if err := m.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	holder := admin(t, address)
	tx, err := holder.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `LOCK TABLE settings IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	audit := func(id string) master.AuditEntry {
		return master.AuditEntry{ID: id, Time: mastertest.T0, Method: "Test", Outcome: master.OutcomeDone}
	}
	dctx, cancel := context.WithTimeout(ctx, 400*time.Millisecond)
	defer cancel()
	began := time.Now()
	err = m.SetSetting(dctx, "api.limit", "1", audit("deadline-1"), master.LocalOperator())
	elapsed := time.Since(began)
	if fault.KindOf(err) != fault.Unavailable || !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("SetSetting past its deadline = %v, want Unavailable with the deadline in the chain", err)
	}
	if elapsed < 300*time.Millisecond || elapsed > 5*time.Second {
		t.Errorf("SetSetting returned after %v, want about 400ms", elapsed)
	}
	if got := settledAcquired(&m.base); got != 0 {
		t.Errorf("%d connections are still out of the pool after the deadline", got)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err := m.SetSetting(ctx, "api.limit", "1", audit("deadline-2"), master.LocalOperator()); err != nil {
		t.Errorf("SetSetting after the lock was let go: %v", err)
	}
	// The failed attempt's audit entry was not written, nor its change.
	if entries, _, err := m.ListAudit(ctx, master.AuditFilter{}, master.Page{Limit: 10}); err != nil || len(entries) != 1 || entries[0].ID != "deadline-2" {
		t.Errorf("audit after the deadline = %v, %v; want only deadline-2", entries, err)
	}
}

// TestTimesAtTheEdges guards that the first and the last instant the store
// accepts come back exactly, in UTC, to the microsecond: 1970-01-01 and
// 9999-12-31 23:59:59.999999.
func TestTimesAtTheEdges(t *testing.T) {
	ctx := context.Background()
	m := openUnmigrated(t)
	if err := m.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	first := time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC)
	last := time.Date(9999, 12, 31, 23, 59, 59, 999999000, time.UTC)
	k := master.Key{ID: "EdgeKey0001", Hash: make([]byte, 32), Holder: "edge", Created: first, Expires: last}
	if err := m.CreateKey(ctx, k, master.AuditEntry{ID: "edge-1", Time: last, Method: "Test", Outcome: master.OutcomeDone}, master.LocalOperator()); err != nil {
		t.Fatal(err)
	}
	got, err := m.Key(ctx, k.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Created.Equal(first) || got.Created.Location() != time.UTC || !got.Expires.Equal(last) || got.Expires.Location() != time.UTC {
		t.Errorf("created %v, expires %v; want %v and %v in UTC", got.Created, got.Expires, first, last)
	}
	entries, _, err := m.ListAudit(ctx, master.AuditFilter{}, master.Page{Limit: 1})
	if err != nil || len(entries) != 1 || !entries[0].Time.Equal(last) || entries[0].Time.Location() != time.UTC {
		t.Errorf("audit time = %v, %v; want %v in UTC", entries, err, last)
	}
}

// TestStatementCache guards what the request path relies on: the pool uses
// pgx's statement cache mode, and a statement used twice is prepared once on
// its connection, so the second call is one round trip.
func TestStatementCache(t *testing.T) {
	ctx := context.Background()
	m := openUnmigrated(t)
	if err := m.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if mode := m.pool.Config().ConnConfig.DefaultQueryExecMode; mode != pgx.QueryExecModeCacheStatement {
		t.Errorf("query exec mode = %v, want cache_statement", mode)
	}
	k := master.Key{ID: "CacheKey001", Hash: make([]byte, 32), Holder: "cache", Created: mastertest.T0}
	if err := m.CreateKey(ctx, k, master.AuditEntry{ID: "cache-1", Time: mastertest.T0, Method: "Test", Outcome: master.OutcomeDone}, master.LocalOperator()); err != nil {
		t.Fatal(err)
	}
	conn, err := m.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	prepared := func() int {
		var n int
		if err := conn.QueryRow(ctx, `SELECT count(*) FROM pg_prepared_statements`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	keyByID := `SELECT ` + keyColumns + ` FROM api_keys WHERE id = $1`
	for range 3 {
		if _, err := scanKey(conn.QueryRow(ctx, keyByID, k.ID)); err != nil {
			t.Fatal(err)
		}
	}
	one := prepared()
	for range 10 {
		if _, err := scanKey(conn.QueryRow(ctx, keyByID, k.ID)); err != nil {
			t.Fatal(err)
		}
	}
	if one < 1 || prepared() != one {
		t.Errorf("prepared statements: %d after three calls, %d after thirteen; want at least 1 and no growth", one, prepared())
	}
}

// explain returns the plan Postgres makes for statement with args, run for
// real (ANALYZE) so the buffers it touched are shown.
func explain(t *testing.T, conn *pgx.Conn, statement string, args ...any) string {
	t.Helper()
	rows, err := conn.Query(context.Background(), `EXPLAIN (ANALYZE, BUFFERS, COSTS OFF, TIMING OFF) `+statement, args...)
	if err != nil {
		t.Fatal(err)
	}
	lines, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return strings.Join(lines, "\n")
}

// TestRequestPathPlans guards that each read on the path of every request is
// point lookups of an index: the key by id, the key's roles on the
// installation and a named tenant (RolesFor), the tenant by name; and that
// every combination of ListAudit's filters reads an index, never the whole
// table. It fills the tables well past the size where a sequential scan would
// win, runs ANALYZE, and asks Postgres for the plan of the very statements the
// driver sends; the plans are logged for the pull request.
func TestRequestPathPlans(t *testing.T) {
	ctx := context.Background()
	address := newSchema(t)
	m := openAt(t, address)
	if err := m.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	conn := admin(t, address)
	mustExec(t, conn,
		`INSERT INTO api_keys (id, hash, holder, created) SELECT 'Key' || lpad(n::text, 6, '0'), decode(repeat('00', 32), 'hex'), 'plan', now() FROM generate_series(1, 20000) n`,
		`INSERT INTO tenants (id, name, driver, address, state, state_reason, created) SELECT 't' || n, 'tenant' || n, 'sqlite', '\x01', 'ready', '', now() FROM generate_series(1, 2000) n`,
		`INSERT INTO grants (key_id, scope, tenant_id, role) SELECT 'Key' || lpad(n::text, 6, '0'), 't' || (n % 2000 + 1), 't' || (n % 2000 + 1), 'reader' FROM generate_series(1, 20000) n`,
		`INSERT INTO grants (key_id, scope, tenant_id, role) SELECT 'Key' || lpad(n::text, 6, '0'), '', NULL, 'admin' FROM generate_series(1, 20000) n`,
		// 100,000 audit entries over ten days: 50 tenant names, 1,000 keys,
		// and outcomes of which "failed" is rare (0.5%) and "done" common.
		`INSERT INTO audit (id, at, key_id, local_user, remote_addr, method, tenant_name, object, outcome, reason, details, request_id)
		 SELECT 'a' || lpad(n::text, 7, '0'), timestamptz '2026-11-01' + n * interval '8 seconds', 'Key' || lpad((n % 1000)::text, 6, '0'), '', '', 'M', 'tenant' || (n % 50),
		        '', CASE WHEN n % 200 = 0 THEN 'failed' WHEN n % 20 = 0 THEN 'refused' ELSE 'done' END, '', '{}', 'r' FROM generate_series(1, 100000) n`,
		`ANALYZE`)

	type planCase struct {
		name, statement string
		args            []any
		want            string // strings, one of which the plan must hold, separated by |
	}
	roles := strings.NewReplacer("$1", "'Key010000'", "$2", "'tenant1000'").Replace(rolesForSQL)
	cases := []planCase{
		{"key by id", `SELECT ` + keyColumns + ` FROM api_keys WHERE id = 'Key010000'`, nil, "Index Scan using api_keys_pkey"},
		// Both lookups give key_id and scope exactly; the planner reads whichever of the primary key and grants_by_scope it prefers.
		{"roles for (key, tenant name): grants", roles, nil, "grants_pkey|grants_by_scope"},
		{"roles for (key, tenant name): tenant", roles, nil, "tenants_name_key"},
		{"tenant by name", `SELECT ` + tenantColumns + ` FROM tenants WHERE name = 'tenant1000'`, nil, "Index Scan using tenants_name_key"},
		{"grants by key, with tenant names", `SELECT ` + grantColumns + grantFrom + ` WHERE g.key_id = 'Key010000' ORDER BY g.key_id, g.scope LIMIT 1001`, nil, "grants_pkey"},
		{"grants by tenant (foreign key check)", `SELECT 1 FROM grants WHERE tenant_id = 't1000' LIMIT 1`, nil, "grants_by_tenant"},
	}
	since, until := time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC), time.Date(2026, 11, 3, 6, 0, 0, 0, time.UTC)
	after := &auditPosition{at: time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC), id: "a0300000"}
	audits := []struct {
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
		{"tenant and outcome", master.AuditFilter{TenantName: "tenant7", Outcome: master.OutcomeFailed}, nil, "audit_by_"},
		{"key and outcome", master.AuditFilter{KeyID: "Key000007", Outcome: master.OutcomeFailed}, nil, "audit_by_"},
		{"tenant, key and outcome", master.AuditFilter{TenantName: "tenant7", KeyID: "Key000007", Outcome: master.OutcomeFailed}, nil, "audit_by_"},
		{"outcome failed and a time range", master.AuditFilter{Outcome: master.OutcomeFailed, Since: since, Until: until}, nil, "audit_by_"},
		{"tenant and a time range, next page", master.AuditFilter{TenantName: "tenant7", Since: since, Until: until}, after, "audit_by_"},
	}
	for _, a := range audits {
		q, args := auditQuery(a.filter, a.after, 1001)
		cases = append(cases, planCase{"audit: " + a.name, q, args, a.want})
	}
	for _, c := range cases {
		plan := explain(t, conn, c.statement, c.args...)
		t.Logf("%s:\n%s", c.name, plan)
		found := false
		for _, alt := range strings.Split(c.want, "|") {
			found = found || strings.Contains(plan, alt)
		}
		if !found || strings.Contains(plan, "Seq Scan") {
			t.Errorf("%s: the plan does not use %q, or scans the table:\n%s", c.name, c.want, plan)
		}
	}
}

// TestCollationDoesNotChangeOrder guards that names and ids order byte by
// byte whatever collation the database was made with. It makes a database
// whose default collation is en_US.UTF-8, which ignores punctuation and puts
// "a1" before "a-b" and "B" after "a", proves that default differs from byte
// order, and then checks every list against byte order, and runs the whole
// contract suite there. A column left to the database's default collation
// fails here.
func TestCollationDoesNotChangeOrder(t *testing.T) {
	ctx := context.Background()
	base := testAddress(t)
	super := admin(t, base)
	db := randomName(t, "mt_coll_")
	if _, err := super.Exec(ctx, `CREATE DATABASE `+db+` TEMPLATE template0 ENCODING 'UTF8' LC_COLLATE 'en_US.utf8' LC_CTYPE 'en_US.utf8'`); err != nil {
		if os.Getenv(requireEnv) != "" {
			t.Fatalf("the test needs the en_US.utf8 locale on the server: %v", err)
		}
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
	inDB := u.String()

	names := []string{"a-b", "a1", "ab", "b", "a", "a-a"}
	ids := []string{"Ab", "aB", "a_b", "a-b", "_x", "B1", "b1", "a1", "Zed", "alpha"}
	// The database's own default collation orders these differently from
	// bytes, or this test proves nothing.
	var viaDefault string
	if err := admin(t, inDB).QueryRow(ctx, `SELECT string_agg(x, ',' ORDER BY x) FROM unnest($1::text[]) x`, ids).Scan(&viaDefault); err != nil {
		t.Fatal(err)
	}
	byBytes := slices.Clone(ids)
	slices.Sort(byBytes)
	if viaDefault == strings.Join(byBytes, ",") {
		t.Fatalf("the database's default collation orders %v as bytes do; the test cannot tell", ids)
	}
	t.Logf("default collation: %s; byte order: %s", viaDefault, strings.Join(byBytes, ","))

	m := openAt(t, newSchemaAt(t, inDB))
	if err := m.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	n := 0
	audit := func() master.AuditEntry {
		n++
		return master.AuditEntry{ID: fmt.Sprintf("coll-%03d", n), Time: mastertest.T0, Method: "Test", Outcome: master.OutcomeDone}
	}
	for _, name := range names {
		if err := m.CreateTenant(ctx, master.Tenant{ID: "t-" + name, Name: name, Driver: "sqlite", SealedAddress: []byte{1}, State: master.StateReady, Created: mastertest.T0}, audit(), master.LocalOperator()); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range ids {
		if err := m.CreateKey(ctx, master.Key{ID: id, Hash: make([]byte, 32), Holder: "c", Created: mastertest.T0}, audit(), master.LocalOperator()); err != nil {
			t.Fatal(err)
		}
		if _, err := m.SetGrant(ctx, master.Grant{KeyID: id, Scope: master.Installation, Role: master.RoleReader}, audit(), master.LocalOperator()); err != nil {
			t.Fatal(err)
		}
	}
	settings := []string{"a.b", "a_b", "a-b", "ab", "a1", "b"}
	for _, s := range settings {
		if err := m.SetSetting(ctx, s, "v", audit(), master.LocalOperator()); err != nil {
			t.Fatal(err)
		}
	}
	auditIDs := []string{"Ab", "aB", "a_b", "a-b", "B1", "b1"}
	var batch []master.AuditEntry
	for _, id := range auditIDs {
		batch = append(batch, master.AuditEntry{ID: "z" + id, Time: mastertest.T0.Add(time.Hour), Method: "Test", Outcome: master.OutcomeDone})
	}
	if err := m.AppendAudit(ctx, batch...); err != nil {
		t.Fatal(err)
	}

	sorted := func(in []string) []string { out := slices.Clone(in); slices.Sort(out); return out }
	// pages reads a whole list two items at a time, as the cursors do.
	check := func(what string, want, got []string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		if !slices.Equal(want, got) {
			t.Errorf("%s: %v, want byte order %v", what, got, want)
		}
	}
	var gotNames, gotKeys, gotScopeKeys, gotSettings, gotAudit []string
	page := master.Page{Limit: 2}
	for {
		ts, next, err := m.ListTenants(ctx, page)
		check("ListTenants page", nil, nil, err)
		for _, x := range ts {
			gotNames = append(gotNames, x.Name)
		}
		if next == "" {
			break
		}
		page.Cursor = next
	}
	check("ListTenants", sorted(names), gotNames, nil)
	for page = (master.Page{Limit: 3}); ; {
		ks, next, err := m.ListKeys(ctx, page)
		check("ListKeys page", nil, nil, err)
		for _, x := range ks {
			gotKeys = append(gotKeys, x.ID)
		}
		if next == "" {
			break
		}
		page.Cursor = next
	}
	check("ListKeys (created, id)", sorted(ids), gotKeys, nil)
	for page = (master.Page{Limit: 3}); ; {
		gs, next, err := m.GrantsByScope(ctx, master.Installation, page)
		check("GrantsByScope page", nil, nil, err)
		for _, x := range gs {
			gotScopeKeys = append(gotScopeKeys, x.KeyID)
		}
		if next == "" {
			break
		}
		page.Cursor = next
	}
	check("GrantsByScope", sorted(ids), gotScopeKeys, nil)
	for page = (master.Page{Limit: 2}); ; {
		ss, next, err := m.ListSettings(ctx, page)
		check("ListSettings page", nil, nil, err)
		for _, x := range ss {
			gotSettings = append(gotSettings, x.Name)
		}
		if next == "" {
			break
		}
		page.Cursor = next
	}
	check("ListSettings", sorted(settings), gotSettings, nil)
	for page = (master.Page{Limit: 2}); ; {
		es, next, err := m.ListAudit(ctx, master.AuditFilter{Since: mastertest.T0.Add(30 * time.Minute)}, page)
		check("ListAudit page", nil, nil, err)
		for _, x := range es {
			gotAudit = append(gotAudit, x.ID)
		}
		if next == "" {
			break
		}
		page.Cursor = next
	}
	wantAudit := sorted(func() []string {
		var out []string
		for _, id := range auditIDs {
			out = append(out, "z"+id)
		}
		return out
	}())
	slices.Reverse(wantAudit) // Newest first, and at one time the larger id first.
	check("ListAudit (at, id) descending", wantAudit, gotAudit, nil)

	t.Run("contract suite", func(t *testing.T) {
		mastertest.Run(t, func(tb testing.TB) master.Store {
			tb.Helper()
			m := openAt(tb, newSchemaAt(tb, inDB))
			if err := m.Migrate(context.Background()); err != nil {
				tb.Fatal(err)
			}
			return m
		})
	})
}

// TestErrorsNeverCarryTheAddress guards that no error from a failing
// database repeats the password, the user, the host or the database name:
// a wrong password, a database that does not exist, and a connection the
// server ends while the store is in use. The test makes the password and user
// distinctive so a plain search finds them.
func TestErrorsNeverCarryTheAddress(t *testing.T) {
	ctx := context.Background()
	base := testAddress(t)
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	host := u.Hostname()

	wrong := *u
	wrong.User = url.UserPassword("needleuser", "hunter2needle")
	_, err = OpenMaster(ctx, withParams(t, wrong.String(), nil))
	if fault.KindOf(err) != fault.Unavailable {
		t.Errorf("a login the server does not know: %v, want Unavailable", err)
	}
	noSecrets(t, "a wrong login", err, host)

	nodb := *u
	nodb.Path = "/needledb"
	_, err = OpenMaster(ctx, nodb.String())
	if fault.KindOf(err) != fault.Unavailable {
		t.Errorf("a database that does not exist: %v, want Unavailable", err)
	}
	noSecrets(t, "a missing database", err, host)
	if pw, ok := u.User.Password(); ok {
		if err != nil && strings.Contains(err.Error(), pw) {
			t.Errorf("a missing database: the error repeats the real password")
		}
	}

	// A connection the server ends: every backend of this store is
	// terminated, and the next calls fail as Unavailable without the
	// address.
	m := openUnmigrated(t)
	if err := m.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	super := admin(t, base)
	var gone int
	if err := super.QueryRow(ctx, `SELECT count(pg_terminate_backend(pid)) FROM pg_stat_activity WHERE application_name = 'nopacs' AND pid <> pg_backend_pid() AND datname = current_database()`).Scan(&gone); err != nil {
		t.Fatal(err)
	}
	t.Logf("terminated %d backends", gone)
	var sawUnavailable bool
	for range 6 {
		_, err := m.Key(ctx, "nope")
		if err == nil || fault.KindOf(err) == fault.NotFound {
			continue
		}
		noSecrets(t, "a terminated connection", err, host)
		if fault.KindOf(err) == fault.Unavailable {
			sawUnavailable = true
		}
	}
	if gone > 0 && !sawUnavailable {
		t.Log("the pool replaced the terminated connections before any call met one")
	}
	if _, err := m.Key(ctx, "nope"); fault.KindOf(err) != fault.NotFound {
		t.Errorf("the store did not recover after its connections were terminated: %v", err)
	}
}

// TestWriteSample writes the contract suite's sample data into a schema for
// the outside check (scripts/check/master-postgres.sh), which reads it with
// psql. It runs only when NOPACS_SAMPLE_SCHEMA names a schema to make
// (which must not exist) and NOPACS_SAMPLE_DIR a directory; there it leaves
// the sample key's hash in hex, and never the secret, which the script holds
// itself.
func TestWriteSample(t *testing.T) {
	schema, dir := os.Getenv("NOPACS_SAMPLE_SCHEMA"), os.Getenv("NOPACS_SAMPLE_DIR")
	if schema == "" || dir == "" {
		t.Skip("NOPACS_SAMPLE_SCHEMA and NOPACS_SAMPLE_DIR are not set; scripts/check/master-postgres.sh sets them")
	}
	base := testAddress(t)
	super := admin(t, base)
	mustExec(t, super, `CREATE SCHEMA `+pgx.Identifier{schema}.Sanitize())
	ctx := context.Background()
	m := openAt(t, withParams(t, base, map[string]string{"search_path": schema, "pool_max_conns": "3"}))
	if err := m.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	mastertest.WriteSample(t, m)
	sum := sha256.Sum256([]byte(mastertest.SampleSecret))
	hashFile := filepath.Join(dir, "hash.hex")
	if err := os.WriteFile(hashFile, []byte(hex.EncodeToString(sum[:])+"\n"), 0o600); err != nil { // #nosec G703 -- the directory is the check script's own temporary one.
		t.Fatal(err)
	}
}
