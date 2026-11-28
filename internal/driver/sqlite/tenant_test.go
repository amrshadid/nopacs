package sqlite

import (
	"context"
	"database/sql"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/master/mastertest"
	"github.com/amrshadid/nopacs/internal/tenant"
	"github.com/amrshadid/nopacs/internal/tenant/tenanttest"
)

// openTenantTest opens a fresh, migrated database for the tenant with this
// id in a new file under tb's temporary directory, closed when tb ends.
func openTenantTest(tb testing.TB, tenantID string) tenant.Store {
	tb.Helper()
	ctx := context.Background()
	s, err := OpenTenant(ctx, filepath.Join(tb.TempDir(), "tenant.db"), tenantID)
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { _ = s.Close() })
	if err := s.Migrate(ctx); err != nil {
		tb.Fatal(err)
	}
	return s
}

// TestTenantContract runs the tenant port's whole contract suite on SQLite:
// the store, the pipeline and the events, each also across two instances
// on one database.
func TestTenantContract(t *testing.T) {
	tenanttest.Run(t, tenanttest.Driver{
		Open: func(tb testing.TB, id string) tenanttest.EventStore { return openTenantTest(tb, id).(*Tenant) },
		OpenPair: func(tb testing.TB, id string) (tenanttest.EventStore, tenanttest.EventStore) {
			a, b := openTenantPair(tb, id)
			return a.(*Tenant), b.(*Tenant)
		},
	})
}

// openTenantPair opens two stores on one new tenant database, as two
// processes would, for the suite's tests across instances.
func openTenantPair(tb testing.TB, tenantID string) (tenant.Store, tenant.Store) {
	tb.Helper()
	ctx := context.Background()
	path := filepath.Join(tb.TempDir(), "tenant.db")
	var out [2]tenant.Store
	for i := range out {
		s, err := OpenTenant(ctx, path, tenantID)
		if err != nil {
			tb.Fatal(err)
		}
		tb.Cleanup(func() { _ = s.Close() })
		if err := s.Migrate(ctx); err != nil {
			tb.Fatal(err)
		}
		out[i] = s
	}
	return out[0], out[1]
}

// TestTenantPanicInWriteReleasesTheWriter guards the deferred rollback on
// the tenant side: a panic inside a change ends its transaction, so the one
// write connection is free for the next write and nothing half-done is
// committed.
func TestTenantPanicInWriteReleasesTheWriter(t *testing.T) {
	ctx := context.Background()
	s := openTenantTest(t, "t1").(*Tenant)
	func() {
		defer func() { _ = recover() }()
		_ = s.writeShared(ctx, "test", func(tx *sql.Tx, st *sharedStatements) error {
			if _, err := tx.StmtContext(ctx, st.upsertSetting).ExecContext(ctx, "half", "done"); err != nil {
				return err
			}
			panic("inside a change")
		})
	}()
	deadline, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := s.SetSetting(deadline, "after", "v", master.AuditEntry{ID: "a1", Time: mastertest.T0, Method: "Test", Outcome: master.OutcomeDone}, nil); err != nil {
		t.Fatalf("a write after a panic in a change: %v", err)
	}
	if _, err := s.Setting(ctx, "half"); fault.KindOf(err) != fault.NotFound {
		t.Errorf("the panicked change was committed (%v)", err)
	}
}

// TestFindingClearedWithNoJobNamesItsPrimary guards the schema's rule on a
// scrub finding cleared with no job: only a finding of a replica's damage
// (replica_of names its primary), which the replicator's verified copy or a
// detach clears, may be; any other finding is cleared by the scrub job that
// read its object good.
func TestFindingClearedWithNoJobNamesItsPrimary(t *testing.T) {
	ctx := context.Background()
	s := openTenantTest(t, "t1").(*Tenant)
	insert := func(id, replicaOf string) error {
		return s.writeShared(ctx, "test", func(tx *sql.Tx, _ *sharedStatements) error {
			_, err := tx.ExecContext(ctx, `INSERT INTO scrub_findings (id, kind, storage, object_key, subject_label, session_label,
				scan_label, sop_instance_uid, revision, job_id, found_at, last_job, last_seen, cleared_at, clear_job, replica_of)
				VALUES (?, 'damaged', 'copy', 'data/x', 'S', 'E', 'C', '1.2.3', 1, 'j1', 1, 'j1', 1, 2, '', ?)`, id, replicaOf)
			return err
		})
	}
	if err := insert("f1", ""); err == nil {
		t.Error("a finding of no replica was kept cleared with no job")
	}
	if err := insert("f2", "store"); err != nil {
		t.Errorf("a finding of a replica's damage cleared with no job was refused: %v", err)
	}
}

// TestTenantFailingFiles guards the kinds and messages of a tenant file the
// driver must not or cannot use, which the registry turns into the tenant's
// state: a file that is not a SQLite database (Unavailable), and on Unix a
// file someone loosened so others can read it (Invalid, with the fix). Each
// message names the tenant database and no path.
func TestTenantFailingFiles(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	notDB := filepath.Join(dir, "notes.db")
	if err := os.WriteFile(notDB, []byte(strings.Repeat("this is not a database\n", 400)), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := OpenTenant(ctx, notDB, "t1")
	if fault.KindOf(err) != fault.Unavailable || fault.Message(err) != "the tenant database file is not a SQLite database" {
		t.Errorf("OpenTenant on a text file = %v, want Unavailable saying the tenant database file is not one", err)
	}
	if err != nil && strings.Contains(err.Error(), dir) {
		t.Errorf("the error names the path: %v", err)
	}
	if runtime.GOOS == "windows" {
		return
	}
	loose := filepath.Join(dir, "loose.db")
	s, err := OpenTenant(ctx, loose, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(loose, 0o644); err != nil { //nolint:gosec // G302: the loosened mode is what is tested.
		t.Fatal(err)
	}
	_, err = OpenTenant(ctx, loose, "t1")
	if fault.KindOf(err) != fault.Invalid || !strings.Contains(fault.Message(err), "the tenant database file can be read or written by users other than its owner") ||
		!strings.Contains(fault.Message(err), "chmod 600") {
		t.Errorf("OpenTenant on a file others can read = %v, want Invalid naming the tenant database and the fix", err)
	}
	if err != nil && strings.Contains(err.Error(), dir) {
		t.Errorf("the error names the path: %v", err)
	}
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
	want := []string{
		"0001_baseline.sql " + shippedTenantBaseline,
	}
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
const shippedTenantBaseline = "ac9c59b2c08ddc761b0f2f0317fd4114b9d67eb537a04078a06fe8c2cd88793d"

// TestTenantNotMigrated guards that a tenant store used before Migrate
// fails as Unavailable with a message that says so, instead of a SQL error,
// and reads as version 0.
func TestTenantNotMigrated(t *testing.T) {
	s, err := OpenTenant(context.Background(), filepath.Join(t.TempDir(), "tenant.db"), "t1")
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

// TestTenantOpenRefuses guards that the tenant open keeps the kinds the
// registry acts on: a path the driver would misread is Invalid (the tenant
// is failed), a file that cannot be made is Unavailable (it is retried), and
// neither error repeats the path.
func TestTenantOpenRefuses(t *testing.T) {
	_, err := OpenTenant(context.Background(), "file:/tmp/x.db", "t1")
	if fault.KindOf(err) != fault.Invalid {
		t.Errorf("a file: URI: %v, want Invalid", err)
	}
	missing := filepath.Join(t.TempDir(), "no", "such", "dir", "t.db")
	_, err = OpenTenant(context.Background(), missing, "t1")
	if fault.KindOf(err) != fault.Unavailable {
		t.Errorf("a path in no directory: %v, want Unavailable", err)
	}
	if err != nil && strings.Contains(err.Error(), missing) {
		t.Errorf("the error repeats the path: %v", err)
	}
	if !strings.Contains(fault.Message(err), "tenant database") || strings.Contains(fault.Message(err), "master") {
		t.Errorf("message %q does not say it was the tenant database", fault.Message(err))
	}
}

// TestTenantAndMasterMigrationsApart guards that a tenant database and a
// master database are told apart by their migrations: a master file opened
// as a tenant's is refused (its applied files are not the tenant's), so a
// mistyped path can never write a tenant's tables into the master.
func TestTenantAndMasterMigrationsApart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "master.db")
	m, err := OpenMaster(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := OpenTenant(ctx, path, "t1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Migrate(ctx); err == nil {
		t.Error("a master database was migrated as a tenant's")
	}
}

// TestTenantRefusesMasterCursors guards that a tenant's lists are named
// apart from the master's in their cursors: a page token from the master's
// settings or audit list, with the same filter, is refused by the tenant's,
// so a token can never start one log's listing inside another's.
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
