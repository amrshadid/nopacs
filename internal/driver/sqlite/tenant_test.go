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
// against an edit after release: every tenant database that applied a file
// refuses to open once it changes (sqlkit.ErrChecksum). A new file is added
// to the list; a listed checksum is never changed.
func TestShippedTenantMigrationsUnchanged(t *testing.T) {
	want := []string{
		"0001_init.sql " + shippedTenantInit,
		"0002_pipeline.sql " + shippedTenantPipeline,
		"0003_released_places.sql " + shippedTenantReleasedPlaces,
		"0004_release_reason_and_scans.sql " + shippedTenantReleaseReason,
		"0005_tenant_secrets.sql " + shippedTenantSealedKeys,
		"0006_event_checkpoints.sql " + shippedTenantCheckpoints,
		"0007_storage_totals.sql " + shippedTenantStorageTotals,
		"0008_catalog_reads.sql " + shippedTenantCatalogReads,
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

// shippedTenantInit is the SHA-256 of migrations/tenant/0001_init.sql as
// this change ships it.
const shippedTenantInit = "72f52dbc4393307b684dd2f48a301e2d36d20cfcca5844e0710795fc630847b9"

// shippedTenantPipeline is the SHA-256 of migrations/tenant/0002_pipeline.sql
// as this change ships it.
const shippedTenantPipeline = "f8c81ca376d4c87c070ba4e674751f40911737af2c58c65b21a348a9e53a643a"

// shippedTenantReleasedPlaces is the SHA-256 of
// migrations/tenant/0003_released_places.sql as this change ships it.
const shippedTenantReleasedPlaces = "2641f017ef90b5c528e6544c0f4986f555b72ca8974adc7aaab998e9530ead50"

// shippedTenantReleaseReason is the SHA-256 of
// migrations/tenant/0004_release_reason_and_scans.sql as this change ships
// it.
const shippedTenantReleaseReason = "01042ce745ca84db776330e80595534557db09fc119f8fc85cdf984fbd86ac3d"

// shippedTenantSealedKeys is the SHA-256 of migrations/tenant/0005_tenant_secrets.sql,
// as this change ships it.
const shippedTenantSealedKeys = "fd806bad7453d38a20301963fac797b7eb9f15b982079f69c2389b78c4288d91"

// shippedTenantCheckpoints is the SHA-256 of
// migrations/tenant/0006_event_checkpoints.sql as this change ships it.
const shippedTenantCheckpoints = "81a7d67803fb370929d6fb4bf3af00dd05e3dd4c712ee46a1e7c33901098e4b2"

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

// shippedTenantStorageTotals is the SHA-256 of
// migrations/tenant/0007_storage_totals.sql as this change ships it.
const shippedTenantStorageTotals = "667329adb71f5ac6f5c9889ad3fd66ea871fea864504ab2d1973d7ea90f8d3d4"

// shippedTenantCatalogReads is the SHA-256 of
// migrations/tenant/0008_catalog_reads.sql as this change ships it.
const shippedTenantCatalogReads = "aec825e134ca713bef17b52b824f3653c037f1ab4cad5c2f98f609e17601e943"
