package postgres

import (
	"context"
	"errors"
	"net/url"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// startRebuild makes a compare rebuild in scratch on ts, claims its unit
// and starts it.
func startRebuild(t *testing.T, ts *Tenant, id, scratch string) tenant.Work {
	t.Helper()
	ctx := context.Background()
	if _, err := ts.CreateJob(ctx, tenant.NewJob{ID: id, Kind: tenant.JobRebuild,
		Rebuild: &tenant.RebuildJob{Mode: tenant.RebuildCompare, Scratch: scratch}, Created: time.Now()}, rebuildAudit("a"+id), nil); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	w, err := ts.ClaimWork(ctx, tenant.RebuildWorkKind, "w", now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ts.StartRebuildUnit(ctx, w.Fence(), now); err != nil {
		t.Fatal(err)
	}
	return w
}

// scratchCount counts what a query over the scratch schema answers, $1
// being the schema's name.
func scratchCount(t *testing.T, base, scratch, query string) int {
	t.Helper()
	var n int
	if err := admin(t, base).QueryRow(context.Background(), query, scratch).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestRebuildMarksItsScratch guards whose a scratch schema's tables are:
// CreateJob leaves the job's mark in the schema in its own transaction
// (rebuild_state naming the job, its tables not made), so a crash after it
// leaves nothing unowned; tables someone else makes in the schema later,
// even of a catalog table's name, are refused by PrepareShadow with a
// refusal that ends the job, and DropShadow removes the job's mark and
// never those tables.
func TestRebuildMarksItsScratch(t *testing.T) {
	ctx := context.Background()
	base := testAddress(t)
	ts := openTenantAt(t, newSchema(t), "T0000000000001")
	scratch := searchPath(t, newSchema(t))
	w := startRebuild(t, ts, "rb-1", scratch)
	var job string
	var made bool
	if err := admin(t, base).QueryRow(ctx, `SELECT job_id, made FROM `+scratch+`.rebuild_state`).Scan(&job, &made); err != nil {
		t.Fatalf("CreateJob left no mark in the scratch schema: %v", err)
	}
	if job != "rb-1" || made {
		t.Errorf("the mark names %q, made %v; want rb-1, not made", job, made)
	}
	mustExec(t, admin(t, base), `CREATE TABLE `+scratch+`.subjects (x integer)`, `INSERT INTO `+scratch+`.subjects VALUES (42)`,
		`CREATE TABLE `+scratch+`.instances (x integer)`)
	_, err := ts.PrepareShadow(ctx, w.Fence())
	if !errors.Is(err, tenant.ErrRebuildRefused) || fault.KindOf(err) != fault.Precondition {
		t.Errorf("PrepareShadow beside tables not the job's: %v, want a refusal that ends the job", err)
	}
	if _, err := ts.CancelJob(ctx, "rb-1", time.Now(), rebuildAudit("a2"), nil); err != nil {
		t.Fatal(err)
	}
	if err := ts.DropShadow(ctx, "rb-1"); err != nil {
		t.Fatal(err)
	}
	if n := scratchCount(t, base, scratch, `SELECT count(*) FROM pg_tables WHERE schemaname = $1 AND tablename IN ('subjects', 'instances')`); n != 2 {
		t.Errorf("DropShadow left %d of the 2 tables that were never the job's", n)
	}
	if n := scratchCount(t, base, scratch, `SELECT count(*) FROM pg_tables WHERE schemaname = $1 AND tablename = 'rebuild_state'`); n != 0 {
		t.Errorf("DropShadow left the job's mark")
	}
	var kept int
	if err := admin(t, base).QueryRow(ctx, `SELECT x FROM `+scratch+`.subjects`).Scan(&kept); err != nil || kept != 42 {
		t.Errorf("a table that was never the job's lost its row: %v %v", kept, err)
	}
}

// TestRebuildScratchSharedWithNoOne guards the privacy of a shadow, the
// whole catalog's UIDs and labels: a scratch schema that grants a privilege
// to another role, or holds default privileges of its own, is refused when
// the job is made, and when its shadow would be made (a grant given after
// the job was made); and default privileges of the whole database give no
// other role any of the shadow's tables.
func TestRebuildScratchSharedWithNoOne(t *testing.T) {
	ctx := context.Background()
	base := testAddress(t)
	ts := openTenantAt(t, newSchema(t), "T0000000000001")
	a := admin(t, base)
	role := randomName(t, "rbreader_")
	mustExec(t, a, `CREATE ROLE `+role)
	t.Cleanup(func() {
		c := admin(t, base)
		mustExec(t, c, `ALTER DEFAULT PRIVILEGES REVOKE ALL ON TABLES FROM `+role, `DROP OWNED BY `+role, `DROP ROLE `+role)
	})
	granted := searchPath(t, newSchema(t))
	mustExec(t, a, `GRANT USAGE ON SCHEMA `+granted+` TO `+role)
	defaults := searchPath(t, newSchema(t))
	mustExec(t, a, `ALTER DEFAULT PRIVILEGES IN SCHEMA `+defaults+` GRANT SELECT ON TABLES TO `+role)
	for i, s := range []string{granted, defaults} {
		_, err := ts.CreateJob(ctx, tenant.NewJob{ID: "rb-x" + string(rune('a'+i)), Kind: tenant.JobRebuild,
			Rebuild: &tenant.RebuildJob{Mode: tenant.RebuildCompare, Scratch: s}, Created: time.Now()}, rebuildAudit("ax"+string(rune('a'+i))), nil)
		if fault.KindOf(err) != fault.Precondition {
			t.Errorf("a rebuild in a schema shared with another role (%d): %v, want a precondition failure", i, err)
		}
	}
	later := searchPath(t, newSchema(t))
	w := startRebuild(t, ts, "rb-1", later)
	mustExec(t, a, `GRANT USAGE ON SCHEMA `+later+` TO `+role)
	if _, err := ts.PrepareShadow(ctx, w.Fence()); !errors.Is(err, tenant.ErrRebuildRefused) {
		t.Errorf("PrepareShadow in a schema shared since the job was made: %v, want a refusal that ends the job", err)
	}
	if n := scratchCount(t, base, later, `SELECT count(*) FROM pg_tables WHERE schemaname = $1`); n != 1 {
		t.Errorf("a refused shadow made %d tables, want only the mark", n)
	}
	if _, err := ts.CancelJob(ctx, "rb-1", time.Now(), rebuildAudit("a2"), nil); err != nil {
		t.Fatal(err)
	}
	if err := ts.DropShadow(ctx, "rb-1"); err != nil {
		t.Fatal(err)
	}
	// Default privileges of the whole database, for the role the tenant
	// connects as, would give each table it makes to the reader.
	mustExec(t, a, `ALTER DEFAULT PRIVILEGES GRANT SELECT ON TABLES TO `+role)
	private := searchPath(t, newSchema(t))
	w = startRebuild(t, ts, "rb-2", private)
	if _, err := ts.PrepareShadow(ctx, w.Fence()); err != nil {
		t.Fatal(err)
	}
	mustExec(t, a, `ALTER DEFAULT PRIVILEGES REVOKE ALL ON TABLES FROM `+role)
	if n := scratchCount(t, base, private, `SELECT count(*) FROM pg_tables WHERE schemaname = $1`); n != len(tenant.CatalogTables())+3 {
		t.Fatalf("the shadow made %d tables", n)
	}
	readable := scratchCount(t, base, private, `SELECT count(*) FROM pg_tables WHERE schemaname = $1
		AND has_table_privilege('`+role+`', quote_ident(schemaname) || '.' || quote_ident(tablename), 'SELECT')`)
	if readable != 0 {
		t.Errorf("another role may read %d of the shadow's tables", readable)
	}
}

// rebuildAudit is an audit entry for a rebuild job a test makes.
func rebuildAudit(id string) master.AuditEntry {
	return master.AuditEntry{ID: id, Time: time.Now(), KeyID: "K1234567890a", RemoteAddress: "192.0.2.10:50000", Method: "Test",
		TenantName: "lab", Outcome: master.OutcomeDone, RequestID: "r1"}
}

// searchPath returns the schema an address's search_path names.
func searchPath(tb testing.TB, address string) string {
	tb.Helper()
	u, err := url.Parse(address)
	if err != nil {
		tb.Fatal(err)
	}
	return u.Query().Get("search_path")
}

// TestRebuildScratchRules guards the contract's precondition on a
// rebuild's scratch schema: one that does not exist, the tenant's own
// schema, one on the tenant's search path, and one that holds a table are
// each refused, so the shadow can never be taken for the catalog in use or
// overwrite what the operator keeps there; an empty one is taken.
func TestRebuildScratchRules(t *testing.T) {
	ctx := context.Background()
	base := testAddress(t)
	tenantAddress := newSchema(t)
	own := searchPath(t, tenantAddress)
	onPath := searchPath(t, newSchema(t))
	full := searchPath(t, newSchema(t))
	empty := searchPath(t, newSchema(t))
	mustExec(t, admin(t, base), `CREATE TABLE `+full+`.kept (a integer)`)
	// The tenant's connection searches its own schema, then onPath.
	ts := openTenantAt(t, withParams(t, tenantAddress, map[string]string{"search_path": own + "," + onPath}), "T0000000000001")
	for i, c := range []struct{ scratch, why string }{
		{"no_such_schema_x", "a schema that does not exist"},
		{own, "the tenant's own schema"},
		{onPath, "a schema on the tenant's search path"},
		{full, "a schema that holds a table"},
	} {
		_, err := ts.CreateJob(ctx, tenant.NewJob{ID: "rb-" + string(rune('a'+i)), Kind: tenant.JobRebuild,
			Rebuild: &tenant.RebuildJob{Mode: tenant.RebuildCompare, Scratch: c.scratch}, Created: time.Now()}, rebuildAudit("a"+string(rune('a'+i))), nil)
		if fault.KindOf(err) != fault.Precondition {
			t.Errorf("a rebuild in %s: %v, want a precondition failure", c.why, err)
		}
	}
	j, err := ts.CreateJob(ctx, tenant.NewJob{ID: "rb-ok", Kind: tenant.JobRebuild,
		Rebuild: &tenant.RebuildJob{Mode: tenant.RebuildCompare, Scratch: empty}, Created: time.Now()}, rebuildAudit("aok"), nil)
	if err != nil {
		t.Fatalf("a rebuild in an empty scratch schema: %v", err)
	}
	if j.Rebuild == nil || j.Rebuild.Scratch != empty {
		t.Errorf("the job made is %+v", j)
	}
}

// TestRebuildShadowLeavesTheSchema guards that a shadow is dropped table by
// table, its scratch schema left in place for the operator who made it,
// and that a shadow's tables found in the scratch schema of another job
// are refused rather than taken or overwritten.
func TestRebuildShadowLeavesTheSchema(t *testing.T) {
	ctx := context.Background()
	base := testAddress(t)
	ts := openTenantAt(t, newSchema(t), "T0000000000001")
	scratch := searchPath(t, newSchema(t))
	if _, err := ts.CreateJob(ctx, tenant.NewJob{ID: "rb-1", Kind: tenant.JobRebuild,
		Rebuild: &tenant.RebuildJob{Mode: tenant.RebuildCompare, Scratch: scratch}, Created: time.Now()}, rebuildAudit("a1"), nil); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	w, err := ts.ClaimWork(ctx, tenant.RebuildWorkKind, "w", now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ts.StartRebuildUnit(ctx, w.Fence(), now); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.PrepareShadow(ctx, w.Fence()); err != nil {
		t.Fatal(err)
	}
	count := func() int {
		var n int
		if err := admin(t, base).QueryRow(ctx, `SELECT count(*) FROM pg_tables WHERE schemaname = $1`, scratch).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := count(); n != len(tenant.CatalogTables())+3 {
		t.Fatalf("the shadow made %d tables, want %d", n, len(tenant.CatalogTables())+3)
	}
	// Another job's shadow in the schema: the rebuild_state row names it.
	mustExec(t, admin(t, base), `UPDATE `+scratch+`.rebuild_state SET job_id = 'someone-else'`)
	if _, err := ts.PrepareShadow(ctx, w.Fence()); fault.KindOf(err) != fault.Precondition {
		t.Errorf("a shadow of another job: %v, want a precondition failure", err)
	}
	mustExec(t, admin(t, base), `UPDATE `+scratch+`.rebuild_state SET job_id = 'rb-1'`)
	if _, err := ts.CancelJob(ctx, "rb-1", now, rebuildAudit("a2"), nil); err != nil {
		t.Fatal(err)
	}
	if err := ts.DropShadow(ctx, "rb-1"); err != nil {
		t.Fatal(err)
	}
	if n := count(); n != 0 {
		t.Errorf("%d tables left in the scratch schema after the shadow was dropped", n)
	}
	var there bool
	if err := admin(t, base).QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = $1)`, scratch).Scan(&there); err != nil || !there {
		t.Errorf("the scratch schema itself is gone: %v", err)
	}
}
