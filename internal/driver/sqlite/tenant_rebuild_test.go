package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// TestRebuildShadowFile guards where a SQLite tenant's shadow lives: a file
// beside the tenant's own, private to its owner, holding the catalog's
// tables and the build's; a shadow file another job left is made anew,
// not taken; and dropping the shadow removes its file and detaches it, the
// tenant's file untouched.
func TestRebuildShadowFile(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "tenant.db")
	ts, err := OpenTenant(ctx, path, "T0000000000001")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ts.Close() })
	if err := ts.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	audit := func(id string) master.AuditEntry {
		return master.AuditEntry{ID: id, Time: time.Now(), KeyID: "K1234567890a", RemoteAddress: "192.0.2.10:50000", Method: "Test",
			TenantName: "lab", Outcome: master.OutcomeDone, RequestID: "r1"}
	}
	now := time.Now()
	if _, err := ts.CreateJob(ctx, tenant.NewJob{ID: "rb-1", Kind: tenant.JobRebuild, Rebuild: &tenant.RebuildJob{Mode: tenant.RebuildCompare},
		Created: now}, audit("a1"), nil); err != nil {
		t.Fatal(err)
	}
	w, err := ts.ClaimWork(ctx, tenant.RebuildWorkKind, "w", now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ts.StartRebuildUnit(ctx, w.Fence(), now); err != nil {
		t.Fatal(err)
	}
	// A shadow another job left: a file of that name holding a table.
	shadow := path + ShadowSuffix
	stale, err := sql.Open("sqlite", shadow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stale.Exec(`CREATE TABLE rebuild_state (job_id TEXT, made INTEGER); INSERT INTO rebuild_state VALUES ('someone-else', 1)`); err != nil {
		t.Fatal(err)
	}
	_ = stale.Close()
	if err := os.Chmod(shadow, 0o600); err != nil {
		t.Fatal(err)
	}
	prog, err := ts.PrepareShadow(ctx, w.Fence())
	if err != nil {
		t.Fatal(err)
	}
	if prog.Job != "rb-1" || prog.Scans != 0 {
		t.Fatalf("the shadow prepared is %+v", prog)
	}
	info, err := os.Stat(shadow)
	if err != nil {
		t.Fatalf("no shadow file beside the tenant's: %v", err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Errorf("the shadow file's mode is %v, want 0600", info.Mode().Perm())
	}
	tables, err := (shadowHost{ts}).Tables(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(tables) != len(tenant.CatalogTables())+2 {
		t.Errorf("the shadow holds %v", tables)
	}
	if _, err := ts.CancelJob(ctx, "rb-1", now, audit("a2"), nil); err != nil {
		t.Fatal(err)
	}
	if err := ts.DropShadow(ctx, "rb-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(shadow); !os.IsNotExist(err) {
		t.Errorf("the shadow file is still there after the drop: %v", err)
	}
	if on, err := (shadowHost{ts}).attached(ctx); err != nil || on {
		t.Errorf("the shadow is still attached after the drop: %v %v", on, err)
	}
	if _, err := ts.Version(ctx); err != nil {
		t.Errorf("the tenant's database after the drop: %v", err)
	}
}

// rebuildTenant opens a migrated tenant in a file of the test's own.
func rebuildTenant(tb testing.TB) (*Tenant, string) {
	tb.Helper()
	path := filepath.Join(tb.TempDir(), "tenant.db")
	ts, err := OpenTenant(context.Background(), path, "T0000000000001")
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { _ = ts.Close() })
	if err := ts.Migrate(context.Background()); err != nil {
		tb.Fatal(err)
	}
	return ts, path
}

// prepareRebuild makes a compare rebuild id on ts, claims and starts its
// unit at now, and prepares its shadow.
func prepareRebuild(tb testing.TB, ts *Tenant, id string, now time.Time) tenant.Work {
	tb.Helper()
	ctx := context.Background()
	audit := master.AuditEntry{ID: "a" + id, Time: now, KeyID: "K1234567890a", RemoteAddress: "192.0.2.10:50000", Method: "Test",
		TenantName: "lab", Outcome: master.OutcomeDone, RequestID: "r1"}
	if _, err := ts.CreateJob(ctx, tenant.NewJob{ID: id, Kind: tenant.JobRebuild, Rebuild: &tenant.RebuildJob{Mode: tenant.RebuildCompare},
		Created: now}, audit, nil); err != nil {
		tb.Fatal(err)
	}
	w, err := ts.ClaimWork(ctx, tenant.RebuildWorkKind, "w", now, time.Minute)
	if err != nil {
		tb.Fatal(err)
	}
	if _, err := ts.StartRebuildUnit(ctx, w.Fence(), now); err != nil {
		tb.Fatal(err)
	}
	if _, err := ts.PrepareShadow(ctx, w.Fence()); err != nil {
		tb.Fatal(err)
	}
	return w
}

// TestRebuildDropLeavesAnotherJobsShadow guards the one shadow file of a
// SQLite tenant: a job that ended without its shadow dropped (canceled)
// and a later job that made the file again are both rebuilds of the
// tenant, and dropping the first's shadow then leaves the file, which
// names the second; dropping the second's removes it.
func TestRebuildDropLeavesAnotherJobsShadow(t *testing.T) {
	ctx := context.Background()
	ts, path := rebuildTenant(t)
	audit := func(id string) master.AuditEntry {
		return master.AuditEntry{ID: id, Time: time.Now(), KeyID: "K1234567890a", RemoteAddress: "192.0.2.10:50000", Method: "Test",
			TenantName: "lab", Outcome: master.OutcomeDone, RequestID: "r1"}
	}
	now := time.Now()
	w1 := prepareRebuild(t, ts, "rb-1", now)
	if _, err := ts.CancelJob(ctx, "rb-1", now, audit("c1"), nil); err != nil {
		t.Fatal(err)
	}
	if err := ts.CompleteWork(ctx, w1.Fence(), now, nil); err != nil {
		t.Fatal(err)
	}
	w2 := prepareRebuild(t, ts, "rb-2", now.Add(time.Second))
	if err := ts.DropShadow(ctx, "rb-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ShadowSuffix); err != nil {
		t.Fatalf("dropping an ended job's shadow removed another job's file: %v", err)
	}
	prog, err := ts.PrepareShadow(ctx, w2.Fence())
	if err != nil || prog.Job != "rb-2" {
		t.Fatalf("the second job's shadow after the first's drop: %+v %v", prog, err)
	}
	if left, err := ts.ShadowsLeft(ctx); err != nil || len(left) != 0 {
		t.Errorf("the shadows left after the drop are %v (%v)", left, err)
	}
	if _, err := ts.CancelJob(ctx, "rb-2", now, audit("c2"), nil); err != nil {
		t.Fatal(err)
	}
	if err := ts.DropShadow(ctx, "rb-2"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ShadowSuffix); !os.IsNotExist(err) {
		t.Errorf("the second job's shadow is still there after its drop: %v", err)
	}
}

// benchSessions is how many sessions the compare benchmark's catalog
// holds, each of one scan of one instance, of a subject of its own.
const benchSessions = 8000

// BenchmarkRebuildCompare measures CompareCatalog over a catalog of
// benchSessions sessions and a shadow equal to it, the tables that name a
// row by its reference (session_modalities, reception_scans) among them.
func BenchmarkRebuildCompare(b *testing.B) {
	ctx := context.Background()
	ts, path := rebuildTenant(b)
	n := fmt.Sprint(benchSessions)
	live, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(10000)")
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = live.Close() }()
	// One connection, so the shadow attached to it below stays attached.
	live.SetMaxOpenConns(1)
	gen := `WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < ` + n + `) `
	for _, s := range []string{
		gen + `INSERT INTO subjects (id, label, folded, created, session_count, instance_count, bytes, updated)
			SELECT i, 'sub-' || i, 'sub-' || i, 1, 1, 1, 10, 1 FROM n`,
		gen + `INSERT INTO sessions (id, study_uid, subject_id, label, folded, storage, created, subject_label, subject_folded, study_date,
			receiver, scan_count, instance_count, bytes, updated, route)
			SELECT i, '1.2.' || i, i, 'ses-1', 'ses-1', 'store', 1, 'sub-' || i, 'sub-' || i, '', '', 1, 1, 10, 1, '' FROM n`,
		gen + `INSERT INTO scans (id, session_id, series_uid, label, folded, instances, bytes, manifest_sha256, created, updated,
			subject_label, subject_folded, session_label, session_folded, storage, modality, series_description, description_folded,
			series_number, receiver, study_uid)
			SELECT i, i, '1.3.' || i, 'scan-1', 'scan-1', 1, 10, NULL, 1, 1, 'sub-' || i, 'sub-' || i, 'ses-1', 'ses-1', 'store', 'CT',
			'', '', NULL, '', '1.2.' || i FROM n`,
		gen + `INSERT INTO instances (scan_id, sop_instance_uid, reception_id, sha256, archived_sha256, storage_key, byte_offset,
			byte_length, size, held_at, profile_version_id) SELECT i, '1.4.' || i, 'rc1', zeroblob(32), NULL, '', 0, 10, 10, 1, NULL FROM n`,
		gen + `INSERT INTO session_modalities (session_id, modality, created) SELECT i, 'CT', 1 FROM n`,
		gen + `INSERT INTO reception_scans (reception_id, scan_id, instances, bytes) SELECT 'rc1', i, 1, 10 FROM n`,
		`INSERT INTO storage_totals (storage, bytes) VALUES ('store', ` + n + `0)`,
		`UPDATE catalog_totals SET subjects = ` + n + `, sessions = ` + n + `, scans = ` + n + `, instances = ` + n,
	} {
		if _, err := live.Exec(s); err != nil {
			b.Fatalf("%s: %v", s, err)
		}
	}
	w := prepareRebuild(b, ts, "rb-1", time.Now())
	if _, err := live.Exec(`ATTACH DATABASE ? AS s`, path+ShadowSuffix); err != nil {
		b.Fatal(err)
	}
	for _, table := range tenant.CatalogTables() {
		if _, err := live.Exec(`DELETE FROM s.` + table); err != nil { // #nosec G202 -- the catalog's own table names.
			b.Fatal(err)
		}
		if _, err := live.Exec(`INSERT INTO s.` + table + ` SELECT * FROM main.` + table); err != nil { // #nosec G202 -- the catalog's own table names.
			b.Fatal(err)
		}
	}
	if _, err := live.Exec(`UPDATE s.rebuild_state SET complete = 1`); err != nil {
		b.Fatal(err)
	}
	if _, err := live.Exec(`DETACH DATABASE s`); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for b.Loop() {
		cmp, err := ts.CompareCatalog(ctx, w.Fence())
		if err != nil {
			b.Fatal(err)
		}
		if !cmp.Equal() {
			b.Fatalf("an equal shadow compares with differences: %+v", cmp.Tables)
		}
	}
}
