package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/amrshadid/nopacs/internal/driver/sqlkit"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant/tenanttest"
)

// testMigrations are migrations that fail if run twice: the first makes a
// table without IF NOT EXISTS, and each logs its version into it, so a file
// applied twice either fails or leaves two log rows. The first three spend a
// moment computing inside their transaction, holding the lock, so that
// processes that start together are inside each other's windows.
func testMigrations(t *testing.T, change string) []sqlkit.Migration {
	t.Helper()
	fsys := fstest.MapFS{
		"m/0001_log.sql":   {Data: []byte("CREATE TABLE log (version INTEGER NOT NULL) STRICT;\n" + slowInsert(1) + change)},
		"m/0002_more.sql":  {Data: []byte(slowInsert(2))},
		"m/0003_slow.sql":  {Data: []byte(slowInsert(3))},
		"m/0004_notx.sql":  {Data: []byte(sqlkit.NoTransactionMark + "\nCREATE INDEX IF NOT EXISTS log_by_version ON log (version);\n")},
		"m/0005_after.sql": {Data: []byte("INSERT INTO log VALUES (5);\n")},
	}
	ms, err := sqlkit.Load(fsys, "m")
	if err != nil {
		t.Fatal(err)
	}
	return ms
}

// slowInsert returns SQL that logs version after counting to 20,000, which
// takes a few milliseconds (more under -race) inside the transaction.
func slowInsert(version int) string {
	return fmt.Sprintf("WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x + 1 FROM c WHERE x < 20000) INSERT INTO log SELECT %d FROM c WHERE x = 20000;\n", version)
}

// migrateFile opens the file at path and applies migrations to it.
func migrateFile(ctx context.Context, path string, ms []sqlkit.Migration) (int, error) {
	return migrateFileAt(ctx, path, ms, time.Time{})
}

// migrateFileAt opens the file at path, waits until start, and applies
// migrations to it. Opening first means processes started together all call
// Migrate at the same instant, not staggered by their start-up.
func migrateFileAt(ctx context.Context, path string, ms []sqlkit.Migration, start time.Time) (int, error) {
	p, err := openPools(ctx, path, masterDB)
	if err != nil {
		return 0, err
	}
	defer func() { _ = p.close() }()
	time.Sleep(time.Until(start))
	return sqlkit.Migrate(ctx, p.write, dialect, ms)
}

// logRows returns the versions in the log table, in order.
func logRows(t *testing.T, path string) []int {
	t.Helper()
	p, err := openPools(context.Background(), path, masterDB)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.close() }()
	rows, err := p.read.Query("SELECT version FROM log ORDER BY version")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []int
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			t.Fatal(err)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestMigrateAppliesOnce guards that migrations are applied in order, once,
// the no-transaction file among them, and that a second run applies
// nothing.
func TestMigrateAppliesOnce(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "m.db")
	ms := testMigrations(t, "")
	for run := range 2 {
		v, err := migrateFile(ctx, path, ms)
		if err != nil || v != 5 {
			t.Fatalf("run %d: Migrate = %d, %v; want 5", run, v, err)
		}
	}
	if got := fmt.Sprint(logRows(t, path)); got != "[1 2 3 5]" {
		t.Errorf("log = %s, want [1 2 3 5]", got)
	}
	p, err := openPools(ctx, path, masterDB)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.close() }()
	if v, err := sqlkit.Version(ctx, p.read, dialect); err != nil || v != 5 {
		t.Errorf("Version = %d, %v; want 5", v, err)
	}
}

// TestMigrateRefusesChangedFile guards the checksum: a database that applied
// a file refuses to start with a binary whose copy of that file differs,
// names the file, and applies nothing further.
func TestMigrateRefusesChangedFile(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "m.db")
	if _, err := migrateFile(ctx, path, testMigrations(t, "")[:2]); err != nil {
		t.Fatal(err)
	}
	_, err := migrateFile(ctx, path, testMigrations(t, "-- edited after release\n"))
	if !errors.Is(err, sqlkit.ErrChecksum) {
		t.Fatalf("Migrate with a changed file = %v, want ErrChecksum", err)
	}
	if !strings.Contains(fault.Message(err), "0001_log.sql") {
		t.Errorf("message %q does not name the file", fault.Message(err))
	}
	t.Logf("refused: %s", fault.Message(err))
	if got := fmt.Sprint(logRows(t, path)); got != "[1 2]" {
		t.Errorf("log = %s after the refusal, want [1 2]: nothing more is applied", got)
	}
}

// TestMigrateRefusesNewerDatabase guards that a database a newer binary
// upgraded, recording a migration this binary has no file for, is refused
// rather than used with queries written for an older schema.
func TestMigrateRefusesNewerDatabase(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "m.db")
	ms := testMigrations(t, "")
	if _, err := migrateFile(ctx, path, ms); err != nil {
		t.Fatal(err)
	}
	_, err := migrateFile(ctx, path, ms[:3])
	if !errors.Is(err, sqlkit.ErrNewerSchema) || fault.KindOf(err) != fault.Conflict {
		t.Fatalf("Migrate with an older binary = %v, want ErrNewerSchema as Conflict", err)
	}
	t.Logf("refused: %s", fault.Message(err))
}

// TestMasterMigrateRefusesNewerDatabase guards the same refusal through the
// master port, with the master's real migrations.
func TestMasterMigrateRefusesNewerDatabase(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "master.db")
	m, err := OpenMaster(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	if err := m.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	next := len(masterMigrations) + 1
	if _, err := m.p.write.ExecContext(ctx, "INSERT INTO schema_migrations VALUES (?, ?, ?, ?)",
		next, fmt.Sprintf("%04d_future.sql", next), make([]byte, 32), time.Now().UnixMicro()); err != nil {
		t.Fatal(err)
	}
	if err := m.Migrate(ctx); !errors.Is(err, sqlkit.ErrNewerSchema) {
		t.Errorf("Migrate = %v, want ErrNewerSchema", err)
	}
}

// TestTenantHandoffMigrationKeepsGrants guards the master migration that
// widens the tenants' state check, which makes tenants and grants again in
// SQLite: a tenant and the grants on it and on the installation, written
// before it, read the same after it; the foreign keys still hold (no
// violation found, a grant on no tenant refused) and the tenant may be
// moved.
func TestTenantHandoffMigrationKeepsGrants(t *testing.T) {
	ctx := context.Background()
	m, err := OpenMaster(ctx, filepath.Join(t.TempDir(), "master.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	if _, err := sqlkit.Migrate(ctx, m.p.write, dialect, masterMigrations[:5]); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO tenants VALUES ('t1', 'lab', 'sqlite', x'01', 'ready', '', 1)`,
		`INSERT INTO api_keys (id, hash, holder, created) VALUES ('k1', zeroblob(32), 'h', 1)`,
		`INSERT INTO grants VALUES ('k1', 't1', 't1', 'writer'), ('k1', '', NULL, 'reader')`,
	} {
		if _, err := m.p.write.ExecContext(ctx, q); err != nil {
			t.Fatal(q, err)
		}
	}
	if err := m.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	grants, _, err := m.GrantsByKey(ctx, "k1", master.Page{Limit: 10})
	if err != nil || len(grants) != 2 {
		t.Fatalf("the grants after the migration: %+v %v", grants, err)
	}
	var violations int
	if err := m.p.write.QueryRowContext(ctx, `SELECT count(*) FROM pragma_foreign_key_check`).Scan(&violations); err != nil || violations != 0 {
		t.Fatalf("foreign key check: %d %v", violations, err)
	}
	if _, err := m.p.write.ExecContext(ctx, `INSERT INTO grants VALUES ('k1', 'nope', 'nope', 'reader')`); err == nil {
		t.Error("a grant on no tenant was taken after the migration")
	}
	if err := m.SetTenantState(ctx, "t1", master.StateMoved, "", master.AuditEntry{ID: "a1", Time: time.Now(), LocalUser: "1",
		Method: "Test", Outcome: master.OutcomeDone}, master.LocalOperator()); err != nil {
		t.Fatalf("a tenant made moved after the migration: %v", err)
	}
}

// TestMigrateRereadsUnderLock guards that what to apply is decided from a
// read made under the lock. Another connection, standing for another
// process, applies every file while this one waits for the lock; this one
// must then find them applied and apply nothing again. A runner that decided
// from a read made before taking the lock would apply 0001 a second time and
// fail. Timing alone (TestMigrateAcrossProcesses) leaves that window too
// narrow to hit reliably, so it is made to happen here.
func TestMigrateRereadsUnderLock(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "m.db")
	ms := testMigrations(t, "")
	mine, err := openPools(ctx, path, masterDB)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = mine.close() }()
	other, err := openPools(ctx, path, masterDB)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = other.close() }()

	calls := 0
	d := dialect
	d.BeginLocked = func(ctx context.Context, db *sql.DB) (*sql.Tx, error) {
		calls++
		// The first call makes the applied table; the second is the first
		// step's lock, and the other process gets in just before it.
		if calls == 2 {
			if _, err := sqlkit.Migrate(ctx, other.write, dialect, ms); err != nil {
				return nil, err
			}
		}
		return dialect.BeginLocked(ctx, db)
	}
	v, err := sqlkit.Migrate(ctx, mine.write, d, ms)
	if err != nil || v != 5 {
		t.Fatalf("Migrate after another process applied everything = %d, %v; want 5, nil", v, err)
	}
	if got := fmt.Sprint(logRows(t, path)); got != "[1 2 3 5]" {
		t.Errorf("log = %s, want [1 2 3 5]", got)
	}
}

// TestMigrationLockExcludes guards that the migration lock is a lock: while
// one connection holds it, another that asks for it waits until it is
// released. A plain (deferred) BEGIN would let both in at once.
func TestMigrationLockExcludes(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "m.db")
	a, err := openPools(ctx, path, masterDB)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.close() }()
	b, err := openPools(ctx, path, masterDB)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = b.close() }()

	held, err := dialect.BeginLocked(ctx, a.write)
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan error, 1)
	go func() {
		tx, err := dialect.BeginLocked(ctx, b.write)
		if err == nil {
			err = tx.Rollback()
		}
		got <- err
	}()
	select {
	case err := <-got:
		t.Fatalf("a second taker got the lock while it was held (err %v)", err)
	case <-time.After(300 * time.Millisecond):
	}
	if err := held.Rollback(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-got:
		if err != nil {
			t.Errorf("the second taker, after the release: %v", err)
		}
	case <-time.After(busyTimeout):
		t.Fatal("the second taker did not get the lock after it was released")
	}
}

// helperEnv names the variable that turns this test binary into one of the
// processes of TestMigrateAcrossProcesses.
const helperEnv = "NOPACS_MIGRATE_HELPER"

// TestMigrateAcrossProcesses guards the migration lock across processes:
// several processes started at the same moment on one new database all
// succeed, and every file is applied exactly once.
func TestMigrateAcrossProcesses(t *testing.T) {
	if testing.Short() {
		t.Skip("starts processes")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	const processes = 6
	for round := range 3 {
		path := filepath.Join(t.TempDir(), "m.db")
		start := time.Now().Add(1500 * time.Millisecond)
		cmds := make([]*exec.Cmd, processes)
		outs := make([]strings.Builder, processes)
		for i := range cmds {
			cmd := exec.Command(exe, "-test.run=^TestMigrateHelperProcess$", "-test.count=1") // #nosec G204 -- this test binary itself.
			cmd.Env = append(os.Environ(), helperEnv+"="+path+"|"+strconv.FormatInt(start.UnixNano(), 10))
			cmd.Stdout, cmd.Stderr = &outs[i], &outs[i]
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			cmds[i] = cmd
		}
		for i, cmd := range cmds {
			if err := cmd.Wait(); err != nil {
				t.Errorf("round %d: process %d failed: %v\n%s", round, i, err, outs[i].String())
			}
		}
		if got := fmt.Sprint(logRows(t, path)); got != "[1 2 3 5]" {
			t.Errorf("round %d: log = %s after %d processes migrated at once, want [1 2 3 5]", round, got, processes)
		}
	}
}

// TestMigrateHelperProcess is one of the processes TestMigrateAcrossProcesses
// starts; run on its own it does nothing.
func TestMigrateHelperProcess(t *testing.T) {
	arg := os.Getenv(helperEnv)
	if arg == "" {
		t.Skip("run by TestMigrateAcrossProcesses")
	}
	path, startText, _ := strings.Cut(arg, "|")
	startNano, err := strconv.ParseInt(startText, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	v, err := migrateFileAt(context.Background(), path, testMigrations(t, ""), time.Unix(0, startNano))
	if err != nil || v != 5 {
		t.Fatalf("Migrate = %d, %v; want 5", v, err)
	}
}

// TestCatalogReadsMigrationFillsRows guards the migration that gives the
// catalog what its reads need, on a database that holds a catalog already
// (the rows tenanttest.CheckMigratedCatalog describes):
// each session and scan gets its parents' labels and its storage, the
// receiver of the reception that confirmed its first instance, and running
// totals equal to a count of the instances its scans hold; each subject
// its sessions, instances, bytes and last update; and the lists by label,
// which read those columns, answer the rows in their order.
func TestCatalogReadsMigrationFillsRows(t *testing.T) {
	ctx := context.Background()
	s, err := OpenTenant(ctx, filepath.Join(t.TempDir(), "tenant.db"), "t1")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if _, err := sqlkit.Migrate(ctx, s.p.write, dialect, tenantMigrations[:7]); err != nil {
		t.Fatal(err)
	}
	reception := func(id, receiver, study string, arrival int) string {
		return fmt.Sprintf(`INSERT INTO receptions VALUES ('%s', '%s', '%s', %d, 'done', 1, 0, 0, 0, 0, 0, 0, 0, 0, 0,
			'', '', 'store', '', 0, 0, 0, 0, 0, '[]', 0, '', '', '', NULL)`, id, receiver, study, arrival)
	}
	held := func(uid, study, series, rc string, scan, size, at int) string {
		return fmt.Sprintf(`INSERT INTO instances VALUES ('%s', '%s', '%s', 'held', randomblob(32), '%s', %d, 'store', 'k/%s', 0, %d, %d,
			randomblob(32), %d, %d)`, uid, study, series, rc, scan, uid, size, size, at, at)
	}
	for _, stmt := range []string{
		reception("rc1", "recv-x", "2.25.1.1", 1), reception("rc2", "recv-y", "2.25.1.2", 1), reception("rc3", "recv-y", "2.25.1.3", 1),
		reception("rc4", "recv-z", "2.25.1.1", 2),
		`INSERT INTO subjects VALUES (1, 'Sub-A', 'sub-a', 100), (2, 'sub-b', 'sub-b', 200), (3, 'sub-c', 'sub-c', 300)`,
		`INSERT INTO sessions VALUES (1, '2.25.1.1', 1, 'ses-1', 'ses-1', 'store', 100), (2, '2.25.1.2', 1, 'ses-2', 'ses-2', 'store', 150),
			(3, '2.25.1.3', 2, 'ses-1', 'ses-1', 'store', 200)`,
		`INSERT INTO scans VALUES (1, 1, '2.25.2.1', 'scan-1', 'scan-1', 2, 30, NULL, 100, 120), (2, 1, '2.25.2.2', 'scan-2', 'scan-2', 1, 5, NULL, 110, 110),
			(3, 2, '2.25.2.3', 'scan-1', 'scan-1', 1, 7, NULL, 150, 160), (4, 3, '2.25.2.4', 'scan-1', 'scan-1', 1, 9, NULL, 200, 200)`,
		held("2.25.3.2", "2.25.1.1", "2.25.2.1", "rc4", 1, 20, 120), held("2.25.3.1", "2.25.1.1", "2.25.2.1", "rc1", 1, 10, 100),
		held("2.25.3.3", "2.25.1.1", "2.25.2.2", "rc1", 2, 5, 110), held("2.25.3.4", "2.25.1.2", "2.25.2.3", "rc2", 3, 7, 150),
		held("2.25.3.5", "2.25.1.3", "2.25.2.4", "rc3", 4, 9, 200),
		`INSERT INTO instances VALUES ('2.25.3.9', '2.25.1.1', '2.25.2.1', 'claimed', randomblob(32), 'rc4', NULL, '', '', 0, 0, 0, NULL, 0, NULL)`,
	} {
		if _, err := s.p.write.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	tenanttest.CheckMigratedCatalog(t, s, fromMicros)
}

// TestLeanCatalogMigrationKeepsRows guards the migration that moves claims
// into their own table, writes held instances clustered by scan without the
// key and checksum other rows give, counts each reception's scans, and
// removes the received files of receptions that ended done or quarantined,
// on a database that holds each kind of row already
// (tenanttest.CheckLeanCatalog describes them and reads them back).
func TestLeanCatalogMigrationKeepsRows(t *testing.T) {
	ctx := context.Background()
	s, err := OpenTenant(ctx, filepath.Join(t.TempDir(), "tenant.db"), "t1")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if _, err := sqlkit.Migrate(ctx, s.p.write, dialect, tenantMigrations[:8]); err != nil {
		t.Fatal(err)
	}
	reception := func(id string, arrival int, state string, ended int) string {
		return fmt.Sprintf(`INSERT INTO receptions VALUES ('%s', 'recv', '%s', %d, '%s', %d, 1, 10, 0, 0, 0, 0, 0, 0, 0,
			'Sub-A', 'ses-1', 'store', '', 0, 0, 0, NULL, 0, '[]', 0, '', '', '', NULL)`,
			id, tenanttest.LeanStudy, arrival, state, ended)
	}
	sum := func(uid string) string { s := tenanttest.LeanSum(uid); return fmt.Sprintf("x'%x'", s[:]) }
	archived := tenanttest.LeanArchivedSum()
	received := func(id string) string {
		return fmt.Sprintf(`INSERT INTO received_files VALUES ('%s', '%s', %s, '%s', '%s', '1.2.840.10008.5.1.4.1.1.4', '1.2.840.10008.1.2.1',
			10, 'in/%s', '', '', '', '', 0, 0)`, id, tenanttest.LeanReceivedUID, sum(tenanttest.LeanReceivedUID), tenanttest.LeanSeries,
			tenanttest.LeanStudy, id)
	}
	for _, stmt := range []string{
		reception(tenanttest.LeanDone, 1, "done", 1), reception(tenanttest.LeanQuarantined, 2, "quarantined", 1),
		reception(tenanttest.LeanFailed, 3, "failed", 1), reception(tenanttest.LeanOpen, 4, "identified", 0),
		`INSERT INTO subjects (id, label, folded, created) VALUES (1, 'Sub-A', 'sub-a', 100)`,
		`INSERT INTO sessions (id, study_uid, subject_id, label, folded, storage, created, subject_label, subject_folded)
			VALUES (1, '` + tenanttest.LeanStudy + `', 1, 'ses-1', 'ses-1', 'store', 100, 'Sub-A', 'sub-a')`,
		`INSERT INTO scans (id, session_id, series_uid, label, folded, instances, bytes, manifest_sha256, created, updated,
			subject_label, subject_folded, session_label, session_folded, storage)
			VALUES (1, 1, '` + tenanttest.LeanSeries + `', 'scan-1', 'scan-1', 2, 30, NULL, 100, 100, 'Sub-A', 'sub-a', 'ses-1', 'ses-1', 'store')`,
		fmt.Sprintf(`INSERT INTO instances VALUES ('%s', '%s', '%s', 'held', %s, '%s', 1, 'store', 'data/Sub-A/ses-1/scan-1/%s.dcm', 0, 10, 10,
			%s, 100, 100)`, tenanttest.LeanLayoutUID, tenanttest.LeanStudy, tenanttest.LeanSeries, sum(tenanttest.LeanLayoutUID),
			tenanttest.LeanDone, tenanttest.LeanLayoutUID, sum(tenanttest.LeanLayoutUID)),
		fmt.Sprintf(`INSERT INTO instances VALUES ('%s', '%s', '%s', 'held', %s, '%s', 1, 'store', '%s', 0, 20, 20, x'%x', 100, 100)`,
			tenanttest.LeanKeptUID, tenanttest.LeanStudy, tenanttest.LeanSeries, sum(tenanttest.LeanKeptUID), tenanttest.LeanDone,
			tenanttest.LeanKeptKey, archived[:]),
		fmt.Sprintf(`INSERT INTO instances VALUES ('%s', '%s', '%s', 'claimed', %s, '%s', NULL, '', '', 0, 0, 0, NULL, 100, NULL)`,
			tenanttest.LeanClaimedUID, tenanttest.LeanStudy, tenanttest.LeanSeries, sum(tenanttest.LeanClaimedUID), tenanttest.LeanOpen),
		received(tenanttest.LeanDone), received(tenanttest.LeanQuarantined), received(tenanttest.LeanFailed), received(tenanttest.LeanOpen),
	} {
		if _, err := s.p.write.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	tenanttest.CheckLeanCatalog(t, s)
	var kept string
	if err := s.p.read.QueryRowContext(ctx, `SELECT group_concat(sop_instance_uid || ':' || storage_key || ':' ||
		(archived_sha256 IS NULL), ' ') FROM (SELECT * FROM instances ORDER BY sop_instance_uid)`).Scan(&kept); err != nil {
		t.Fatal(err)
	}
	if want := tenanttest.LeanLayoutUID + "::1 " + tenanttest.LeanKeptUID + ":" + tenanttest.LeanKeptKey + ":0"; kept != want {
		t.Errorf("the migrated rows keep %q, want %q: only a key and a checksum no other row gives", kept, want)
	}
}

// TestLeanCatalogMigrationRefusesAMismatchedStorage guards the migration
// that drops each instance's own storage and reads its scan's: a database
// at the schema before it, where a held instance is on another storage
// than its scan, is refused with a message that says why, and is left at
// the schema it was at, with its rows as they were.
func TestLeanCatalogMigrationRefusesAMismatchedStorage(t *testing.T) {
	ctx := context.Background()
	s, err := OpenTenant(ctx, filepath.Join(t.TempDir(), "tenant.db"), "t1")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if _, err := sqlkit.Migrate(ctx, s.p.write, dialect, tenantMigrations[:8]); err != nil {
		t.Fatal(err)
	}
	sum := func(uid string) string { s := tenanttest.LeanSum(uid); return fmt.Sprintf("x'%x'", s[:]) }
	for _, stmt := range []string{
		`INSERT INTO subjects (id, label, folded, created) VALUES (1, 'Sub-A', 'sub-a', 100)`,
		`INSERT INTO sessions (id, study_uid, subject_id, label, folded, storage, created, subject_label, subject_folded)
			VALUES (1, '` + tenanttest.LeanStudy + `', 1, 'ses-1', 'ses-1', 'store', 100, 'Sub-A', 'sub-a')`,
		`INSERT INTO scans (id, session_id, series_uid, label, folded, instances, bytes, manifest_sha256, created, updated,
			subject_label, subject_folded, session_label, session_folded, storage)
			VALUES (1, 1, '` + tenanttest.LeanSeries + `', 'scan-1', 'scan-1', 1, 10, NULL, 100, 100, 'Sub-A', 'sub-a', 'ses-1', 'ses-1', 'store')`,
		fmt.Sprintf(`INSERT INTO instances VALUES ('%s', '%s', '%s', 'held', %s, 'rc', 1, 'elsewhere', 'k', 0, 10, 10, %s, 100, 100)`,
			tenanttest.LeanLayoutUID, tenanttest.LeanStudy, tenanttest.LeanSeries, sum(tenanttest.LeanLayoutUID), sum(tenanttest.LeanLayoutUID)),
	} {
		if _, err := s.p.write.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	err = s.Migrate(ctx)
	if err == nil || !strings.Contains(err.Error(), "a held instance is on a different storage than its scan") {
		t.Fatalf("migrating with a mismatched storage: %v, want a refusal that names it", err)
	}
	var version, held int
	if err := s.p.read.QueryRowContext(ctx, `SELECT (SELECT max(version) FROM schema_migrations),
		(SELECT count(*) FROM instances WHERE state = 'held' AND storage = 'elsewhere')`).Scan(&version, &held); err != nil {
		t.Fatal(err)
	}
	if version != 8 || held != 1 {
		t.Errorf("after the refusal the schema is at %d with %d rows on the other storage, want 8 and 1", version, held)
	}
}

// lendImportArrivals lends a schema before the import's the table every
// deciding transaction of the port probes for an import's files (empty, so
// it finds none), and the table AddReceiver probes for a name an import job
// still holds, for a fill that writes through the port.
func lendImportArrivals(t *testing.T, s *Tenant) {
	t.Helper()
	for _, q := range []string{`CREATE TABLE import_arrivals (reception_id TEXT)`, `CREATE TABLE import_jobs (receiver TEXT)`} {
		if _, err := s.p.write.ExecContext(context.Background(), q); err != nil {
			t.Fatal(err)
		}
	}
}

// takeBackImportArrivals drops the tables lendImportArrivals lent, before
// the migrations run.
func takeBackImportArrivals(t *testing.T, s *Tenant) {
	t.Helper()
	for _, q := range []string{`DROP TABLE import_arrivals`, `DROP TABLE import_jobs`} {
		if _, err := s.p.write.ExecContext(context.Background(), q); err != nil {
			t.Fatal(err)
		}
	}
}

// correctionHoldTables are, empty, the tables a scan's commit looks in for
// labels and UIDs a correction holds, in the columns it reads.
var correctionHoldTables = []string{
	`hidden_subjects (correction_id TEXT, folded TEXT)`,
	`hidden_sessions (correction_id TEXT, study_uid TEXT, subject_folded TEXT, folded TEXT)`,
	`hidden_scans (correction_id TEXT, series_uid TEXT, subject_folded TEXT, session_folded TEXT, folded TEXT)`,
	`hidden_instances (correction_id TEXT, sop_instance_uid TEXT)`,
	`correction_scans (correction_id TEXT, subject_label TEXT, session_label TEXT, scan_label TEXT, new_subject TEXT, new_session TEXT, new_scan TEXT)`,
	`corrections (id TEXT, kind TEXT, level TEXT, state TEXT)`,
}

// lendCorrectionHolds lends a schema before the corrections' the tables a
// scan's commit looks in for what a correction holds (empty, so it finds
// none), for a fill that commits through the port.
func lendCorrectionHolds(t *testing.T, s *Tenant) {
	t.Helper()
	for _, q := range correctionHoldTables {
		if _, err := s.p.write.ExecContext(context.Background(), `CREATE TABLE `+q); err != nil {
			t.Fatal(err)
		}
	}
}

// takeBackCorrectionHolds drops the tables lendCorrectionHolds lent, before
// the migrations run.
func takeBackCorrectionHolds(t *testing.T, s *Tenant) {
	t.Helper()
	for _, q := range correctionHoldTables {
		if _, err := s.p.write.ExecContext(context.Background(), `DROP TABLE `+strings.Fields(q)[0]); err != nil {
			t.Fatal(err)
		}
	}
}

// lendInstanceDamage lends a schema before the scrub's the table every read
// of a job's stream probes for a scrub's marks (empty, so it finds none),
// for a fill that reads through the port.
func lendInstanceDamage(t *testing.T, s *Tenant) {
	t.Helper()
	if _, err := s.p.write.ExecContext(context.Background(), `CREATE TABLE instance_damage (sop_instance_uid TEXT, finding_id TEXT)`); err != nil {
		t.Fatal(err)
	}
}

// takeBackInstanceDamage drops the table lendInstanceDamage lent, before
// the migrations run.
func takeBackInstanceDamage(t *testing.T, s *Tenant) {
	t.Helper()
	if _, err := s.p.write.ExecContext(context.Background(), `DROP TABLE instance_damage`); err != nil {
		t.Fatal(err)
	}
}

// TestSendingMigrationKeepsData guards the migration that adds senders and
// jobs on a database that holds data: a tenant filled through the port at
// the schema before it (tenanttest.FillBeforeSending) reads the same after
// it, and a job over that data lists and sends every instance
// (tenanttest.CheckAfterSending).
func TestSendingMigrationKeepsData(t *testing.T) {
	ctx := context.Background()
	s, err := OpenTenant(ctx, filepath.Join(t.TempDir(), "tenant.db"), "t1")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if _, err := sqlkit.Migrate(ctx, s.p.write, dialect, tenantMigrations[:10]); err != nil {
		t.Fatal(err)
	}
	// The port of this version reads and writes columns a later migration
	// adds (a reception's and a session's route, a release's storage);
	// they are lent for the fill, which writes through the port, and taken
	// back before the migrations run.
	lent := []string{"receptions ADD COLUMN route", "sessions ADD COLUMN route", "quarantine ADD COLUMN release_storage",
		"quarantine ADD COLUMN release_profile", "quarantine ADD COLUMN import_job", "quarantine ADD COLUMN import_path"}
	for _, c := range lent {
		if _, err := s.p.write.ExecContext(ctx, `ALTER TABLE `+c+` TEXT NOT NULL DEFAULT ''`); err != nil {
			t.Fatal(err)
		}
	}
	lendImportArrivals(t, s)
	lendCorrectionHolds(t, s)
	tenanttest.FillBeforeSending(t, s)
	for _, c := range lent {
		if _, err := s.p.write.ExecContext(ctx, `ALTER TABLE `+strings.Replace(c, "ADD", "DROP", 1)); err != nil {
			t.Fatal(err)
		}
	}
	takeBackCorrectionHolds(t, s)
	takeBackImportArrivals(t, s)
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if v, err := s.Version(ctx); err != nil || v != len(tenantMigrations) {
		t.Fatalf("the version after the migration = %d, %v", v, err)
	}
	tenanttest.CheckAfterSending(t, s)
}

// TestDeidJobsMigrationKeepsJobs guards the migration that lets a job be
// of the kind deid, which rebuilds jobs in SQLite to widen its kind's
// check: send jobs written through the port at the schema before it
// (tenanttest.FillBeforeDeidJobs, the new columns lent for the port and
// taken back) read exactly as before and go on working after it
// (tenanttest.CheckAfterDeidJobs); every index of jobs is there again; and
// the checks hold: a kind other than send or deid, and a send job with a
// mode, are refused.
func TestDeidJobsMigrationKeepsJobs(t *testing.T) {
	ctx := context.Background()
	s, err := OpenTenant(ctx, filepath.Join(t.TempDir(), "tenant.db"), "t1")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if _, err := sqlkit.Migrate(ctx, s.p.write, dialect, tenantMigrations[:14]); err != nil {
		t.Fatal(err)
	}
	lent := []string{"deid_mode TEXT NOT NULL DEFAULT ''", "profile_name TEXT NOT NULL DEFAULT ''", "profile_version INTEGER NOT NULL DEFAULT 0"}
	for _, c := range lent {
		if _, err := s.p.write.ExecContext(ctx, `ALTER TABLE jobs ADD COLUMN `+c); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.p.write.ExecContext(ctx, `CREATE TABLE job_tallies (job_id TEXT, tally TEXT, count INTEGER)`); err != nil {
		t.Fatal(err)
	}
	lendImportArrivals(t, s)
	lendInstanceDamage(t, s)
	lendCorrectionHolds(t, s)
	before := tenanttest.FillBeforeDeidJobs(t, s)
	takeBackCorrectionHolds(t, s)
	takeBackInstanceDamage(t, s)
	takeBackImportArrivals(t, s)
	for _, c := range lent {
		if _, err := s.p.write.ExecContext(ctx, `ALTER TABLE jobs DROP COLUMN `+strings.Fields(c)[0]); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.p.write.ExecContext(ctx, `DROP TABLE job_tallies`); err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var indexes string
	if err := s.p.read.QueryRowContext(ctx, `SELECT group_concat(name, ',') FROM (SELECT name FROM sqlite_master
		WHERE type = 'index' AND tbl_name = 'jobs' AND name NOT LIKE 'sqlite_%' ORDER BY name)`).Scan(&indexes); err != nil {
		t.Fatal(err)
	}
	if indexes != "jobs_by_created,jobs_by_key,jobs_by_sender,jobs_by_state,jobs_in_progress,jobs_of_sender_in_progress" {
		t.Errorf("the indexes of jobs after the migration: %s", indexes)
	}
	for _, stmt := range []string{
		`UPDATE jobs SET kind = 'other' WHERE id = 'j-queued'`,
		`UPDATE jobs SET deid_mode = 'check' WHERE id = 'j-queued'`,
	} {
		if _, err := s.p.write.ExecContext(ctx, stmt); err == nil {
			t.Errorf("the migrated jobs take %s", stmt)
		}
	}
	tenanttest.CheckAfterDeidJobs(t, s, before)
}
