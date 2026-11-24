package postgres

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/amrshadid/nopacs/internal/driver/sqlkit"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant/tenanttest"
)

// testMigrations are migrations that fail if run twice: the first makes a
// table without IF NOT EXISTS, and each logs its version into it, so a file
// applied twice either fails or leaves two log rows. The first three sleep
// inside their transaction, holding what they hold, so that processes that
// start together are inside each other's windows.
func testMigrations(t *testing.T, change string) []sqlkit.Migration {
	t.Helper()
	fsys := fstest.MapFS{
		"m/0001_log.sql":   {Data: []byte("CREATE TABLE log (version integer NOT NULL);\n" + slowInsert(1) + change)},
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

// slowInsert returns SQL that logs version after sleeping 40 milliseconds.
func slowInsert(version int) string {
	return fmt.Sprintf("INSERT INTO log SELECT %d FROM (SELECT pg_sleep(0.04)) AS s;\n", version)
}

// migrateAt opens a store at address, waits until start, and applies
// migrations to it. Opening first means stores started together all call
// Migrate at the same instant, not staggered by their start-up.
func migrateAt(ctx context.Context, address string, ms []sqlkit.Migration, start time.Time) (int, error) {
	m, err := OpenMaster(ctx, address)
	if err != nil {
		return 0, err
	}
	defer func() { _ = m.Close() }()
	time.Sleep(time.Until(start))
	return m.migrate(ctx, ms)
}

// logRows returns the versions in the log table of the schema address names,
// in order.
func logRows(t *testing.T, address string) []int {
	t.Helper()
	conn := admin(t, address)
	rows, err := conn.Query(context.Background(), "SELECT version FROM log ORDER BY version")
	if err != nil {
		t.Fatal(err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowTo[int])
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// lockAvailable reports whether no session holds the migration lock of the
// schema address names, by trying to take it from a session of its own.
func lockAvailable(t *testing.T, address string) bool {
	t.Helper()
	conn := admin(t, address)
	var got bool
	if err := conn.QueryRow(context.Background(), `SELECT pg_try_advisory_lock(`+lockName+`)`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got {
		mustExec(t, conn, `SELECT pg_advisory_unlock(`+lockName+`)`)
	}
	return got
}

// holdLock takes the migration lock of conn's schema for conn's session, as a
// process that is migrating would. It tries, and fails the test if another
// session holds the lock already, rather than waiting for it forever: a lock
// the driver forgot to release must fail a test, not hang it.
func holdLock(t *testing.T, conn *pgx.Conn) {
	t.Helper()
	var got bool
	if err := conn.QueryRow(context.Background(), `SELECT pg_try_advisory_lock(`+lockName+`)`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if !got {
		t.Fatal("the migration lock is held by another session; a lock was not released")
	}
}

// TestMigrateAppliesOnce guards that migrations are applied in order, once,
// the no-transaction file among them, and that a second run applies
// nothing.
func TestMigrateAppliesOnce(t *testing.T) {
	ctx := context.Background()
	address := newSchema(t)
	ms := testMigrations(t, "")
	for run := range 2 {
		v, err := migrateAt(ctx, address, ms, time.Time{})
		if err != nil || v != 5 {
			t.Fatalf("run %d: Migrate = %d, %v; want 5", run, v, err)
		}
	}
	if got := fmt.Sprint(logRows(t, address)); got != "[1 2 3 5]" {
		t.Errorf("log = %s, want [1 2 3 5]", got)
	}
	m := openAt(t, address)
	if v, err := m.Version(ctx); err != nil || v != 5 {
		t.Errorf("Version = %d, %v; want 5", v, err)
	}
}

// TestMigrateRefusesChangedFile guards the checksum: a database that applied
// a file refuses to start with a binary whose copy of that file differs,
// names the file, and applies nothing further.
func TestMigrateRefusesChangedFile(t *testing.T) {
	ctx := context.Background()
	address := newSchema(t)
	if _, err := migrateAt(ctx, address, testMigrations(t, "")[:2], time.Time{}); err != nil {
		t.Fatal(err)
	}
	_, err := migrateAt(ctx, address, testMigrations(t, "-- edited after release\n"), time.Time{})
	if !errors.Is(err, sqlkit.ErrChecksum) {
		t.Fatalf("Migrate with a changed file = %v, want ErrChecksum", err)
	}
	if !strings.Contains(fault.Message(err), "0001_log.sql") {
		t.Errorf("message %q does not name the file", fault.Message(err))
	}
	t.Logf("refused: %s", fault.Message(err))
	if got := fmt.Sprint(logRows(t, address)); got != "[1 2]" {
		t.Errorf("log = %s after the refusal, want [1 2]: nothing more is applied", got)
	}
}

// TestMigrateRefusesNewerDatabase guards that a database a newer binary
// upgraded, recording a migration this binary has no file for, is refused
// rather than used with queries written for an older schema.
func TestMigrateRefusesNewerDatabase(t *testing.T) {
	ctx := context.Background()
	address := newSchema(t)
	ms := testMigrations(t, "")
	if _, err := migrateAt(ctx, address, ms, time.Time{}); err != nil {
		t.Fatal(err)
	}
	_, err := migrateAt(ctx, address, ms[:3], time.Time{})
	if !errors.Is(err, sqlkit.ErrNewerSchema) || fault.KindOf(err) != fault.Conflict {
		t.Fatalf("Migrate with an older binary = %v, want ErrNewerSchema as Conflict", err)
	}
	t.Logf("refused: %s", fault.Message(err))
}

// TestMasterMigrateRefusesNewerDatabase guards the same refusal through the
// master port, with the master's real migrations.
func TestMasterMigrateRefusesNewerDatabase(t *testing.T) {
	ctx := context.Background()
	m := openUnmigrated(t)
	if err := m.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	next := len(masterMigrations) + 1
	if _, err := m.pool.Exec(ctx, "INSERT INTO schema_migrations VALUES ($1, $2, $3, now())",
		next, fmt.Sprintf("%04d_future.sql", next), make([]byte, 32)); err != nil {
		t.Fatal(err)
	}
	if err := m.Migrate(ctx); !errors.Is(err, sqlkit.ErrNewerSchema) {
		t.Errorf("Migrate = %v, want ErrNewerSchema", err)
	}
}

// TestMigrationLockWaits guards that the migration lock is a lock: while
// another session holds it, Migrate waits, applies nothing, and gives up with
// Unavailable when its context ends; once the lock is let go, Migrate gets
// it and applies the files. Without the lock the first Migrate would have
// gone straight through.
func TestMigrationLockWaits(t *testing.T) {
	address := newSchema(t)
	holder := admin(t, address)
	holdLock(t, holder)
	m := openAt(t, address)

	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()
	began := time.Now()
	_, err := m.migrate(ctx, masterMigrations)
	if fault.KindOf(err) != fault.Unavailable || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Migrate while the lock was held = %v, want Unavailable by the deadline", err)
	}
	if waited := time.Since(began); waited < 500*time.Millisecond {
		t.Errorf("Migrate gave up after %v; it should have waited for its deadline", waited)
	}
	if v, err := m.Version(context.Background()); err != nil || v != 0 {
		t.Errorf("Version after the refused Migrate = %d, %v; want 0: nothing applied", v, err)
	}

	done := make(chan error, 1)
	go func() { done <- m.Migrate(context.Background()) }()
	select {
	case err := <-done:
		t.Fatalf("Migrate finished (%v) while the lock was held", err)
	case <-time.After(400 * time.Millisecond):
	}
	mustExec(t, holder, `SELECT pg_advisory_unlock(`+lockName+`)`)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Migrate after the lock was released: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Migrate did not get the lock after it was released")
	}
	if v, err := m.Version(context.Background()); err != nil || v != len(masterMigrations) {
		t.Errorf("Version = %d, %v; want %d", v, err, len(masterMigrations))
	}
}

// TestMigrationLockIsPerSchema guards that two installations in two schemas
// of one database do not wait for each other: the lock's name holds the
// schema's.
func TestMigrationLockIsPerSchema(t *testing.T) {
	base := testAddress(t)
	one, other := newSchemaAt(t, base), newSchemaAt(t, base)
	holdLock(t, admin(t, one))
	if !lockAvailable(t, other) {
		t.Fatal("a lock held in one schema is held in another")
	}
	if lockAvailable(t, one) {
		t.Fatal("the lock held in the first schema is not held")
	}
}

// TestMigrationLockReleased guards that the lock is let go on every path:
// success, a failed file, a refused checksum, and a request that ends while
// it waits. After each, another session can take it at once, and the pool has
// no connection out.
func TestMigrationLockReleased(t *testing.T) {
	// A bound, so a lock that was not released fails the test with a message
	// rather than hanging it in the next Migrate.
	ctx, stop := context.WithTimeout(context.Background(), 40*time.Second)
	defer stop()
	address := newSchema(t)
	m := openAt(t, address)
	check := func(path string) {
		t.Helper()
		if !lockAvailable(t, address) {
			t.Errorf("the migration lock is still held after %s", path)
		}
		if n := settledAcquired(&m.base); n != 0 {
			t.Errorf("%d connections are still out of the pool after %s", n, path)
		}
	}

	bad := fstest.MapFS{"m/0001_bad.sql": {Data: []byte("SELECT no_such_function();\n")}}
	badMigrations, err := sqlkit.Load(bad, "m")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.migrate(ctx, badMigrations); fault.KindOf(err) != fault.Internal {
		t.Fatalf("a migration that fails: %v, want Internal", err)
	}
	check("a failed file")

	if err := m.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	check("a successful run")

	changed := fstest.MapFS{"m/0001_init.sql": {Data: []byte("SELECT 1;\n")}}
	changedMigrations, err := sqlkit.Load(changed, "m")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.migrate(ctx, changedMigrations); !errors.Is(err, sqlkit.ErrChecksum) {
		t.Fatalf("a changed file: %v, want ErrChecksum", err)
	}
	check("a refused checksum")

	holder := admin(t, address)
	holdLock(t, holder)
	cctx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	if err := m.Migrate(cctx); fault.KindOf(err) != fault.Unavailable {
		t.Fatalf("Migrate canceled while waiting: %v, want Unavailable", err)
	}
	mustExec(t, holder, `SELECT pg_advisory_unlock(`+lockName+`)`)
	check("a request that ended while waiting")

	already, cancelNow := context.WithCancel(ctx)
	cancelNow()
	if err := m.Migrate(already); fault.KindOf(err) != fault.Unavailable {
		t.Fatalf("Migrate with a canceled context: %v, want Unavailable", err)
	}
	check("a context already canceled")
}

// TestMigrateAcrossStores guards the lock inside one process: stores opened
// separately, each with its own pool, all migrating a new schema at the same
// moment, all succeed and each file is applied exactly once.
func TestMigrateAcrossStores(t *testing.T) {
	const stores = 6
	for round := range 3 {
		address := newSchema(t)
		ms := testMigrations(t, "")
		start := time.Now().Add(500 * time.Millisecond)
		errs := make([]error, stores)
		var wg sync.WaitGroup
		for i := range stores {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, errs[i] = migrateAt(context.Background(), address, ms, start)
			}()
		}
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Errorf("round %d: store %d: %v", round, i, err)
			}
		}
		if got := fmt.Sprint(logRows(t, address)); got != "[1 2 3 5]" {
			t.Errorf("round %d: log = %s after %d stores migrated at once, want [1 2 3 5]", round, got, stores)
		}
	}
}

// helperEnv names the variable that turns this test binary into one of the
// processes of TestMigrateAcrossProcesses.
const helperEnv = "NOPACS_MIGRATE_HELPER"

// TestMigrateAcrossProcesses guards the migration lock across processes:
// several processes started at the same moment on one new schema all
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
		address := newSchema(t)
		start := time.Now().Add(1500 * time.Millisecond)
		cmds := make([]*exec.Cmd, processes)
		outs := make([]strings.Builder, processes)
		for i := range cmds {
			cmd := exec.Command(exe, "-test.run=^TestMigrateHelperProcess$", "-test.count=1") // #nosec G204 -- this test binary itself.
			// The address goes in the environment, not the arguments, which any
			// user of the machine can read.
			cmd.Env = append(os.Environ(), helperEnv+"="+strconv.FormatInt(start.UnixNano(), 10)+"|"+address)
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
		if got := fmt.Sprint(logRows(t, address)); got != "[1 2 3 5]" {
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
	startText, address, _ := strings.Cut(arg, "|")
	startNano, err := strconv.ParseInt(startText, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	v, err := migrateAt(context.Background(), address, testMigrations(t, ""), time.Unix(0, startNano))
	if err != nil || v != 5 {
		t.Fatalf("Migrate = %d, %v; want 5", v, err)
	}
}

// TestCatalogReadsMigrationFillsRows guards the migration that gives the
// catalog what its reads need, on a database that holds a catalog already
// (the rows tenanttest.CheckMigratedCatalog describes): each session and
// scan gets its parents' labels and its storage, the receiver of the
// reception that confirmed its first instance, and running totals equal to
// a count of the instances its scans hold; each subject its sessions,
// instances, bytes and last update; and the lists by label, which read
// those columns, answer the rows in their order.
func TestCatalogReadsMigrationFillsRows(t *testing.T) {
	ctx := context.Background()
	address := newSchema(t)
	s, err := OpenTenant(ctx, address, "t1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if _, err := s.migrate(ctx, tenantMigrations[:7]); err != nil {
		t.Fatal(err)
	}
	at := func(us int) string { return fmt.Sprintf(`timestamptz 'epoch' + %d * interval '1 microsecond'`, us) }
	reception := func(id, receiver, study string, arrival int) string {
		return fmt.Sprintf(`INSERT INTO receptions VALUES ('%s', '%s', '%s', %d, 'done', true, 0, 0, 0, 0, 0, 0, 0, 0, 0,
			'', '', 'store', '', now(), now(), now(), now(), 0, '[]', false, '', '', '', NULL)`, id, receiver, study, arrival)
	}
	held := func(uid, study, series, rc string, scan, size, us int) string {
		return fmt.Sprintf(`INSERT INTO instances VALUES ('%s', '%s', '%s', 'held', sha256('%s'::bytea), '%s', %d, 'store', 'k/%s', 0, %d, %d,
			sha256('%s'::bytea), %s, %s)`, uid, study, series, uid, rc, scan, uid, size, size, uid, at(us), at(us))
	}
	mustExec(t, admin(t, address),
		reception("rc1", "recv-x", "2.25.1.1", 1), reception("rc2", "recv-y", "2.25.1.2", 1), reception("rc3", "recv-y", "2.25.1.3", 1),
		reception("rc4", "recv-z", "2.25.1.1", 2),
		`INSERT INTO subjects (id, label, folded, created) OVERRIDING SYSTEM VALUE VALUES (1, 'Sub-A', 'sub-a', `+at(100)+`),
			(2, 'sub-b', 'sub-b', `+at(200)+`), (3, 'sub-c', 'sub-c', `+at(300)+`)`,
		`INSERT INTO sessions (id, study_uid, subject_id, label, folded, storage, created) OVERRIDING SYSTEM VALUE VALUES
			(1, '2.25.1.1', 1, 'ses-1', 'ses-1', 'store', `+at(100)+`), (2, '2.25.1.2', 1, 'ses-2', 'ses-2', 'store', `+at(150)+`),
			(3, '2.25.1.3', 2, 'ses-1', 'ses-1', 'store', `+at(200)+`)`,
		`INSERT INTO scans (id, session_id, series_uid, label, folded, instances, bytes, manifest_sha256, created, updated)
			OVERRIDING SYSTEM VALUE VALUES (1, 1, '2.25.2.1', 'scan-1', 'scan-1', 2, 30, NULL, `+at(100)+`, `+at(120)+`),
			(2, 1, '2.25.2.2', 'scan-2', 'scan-2', 1, 5, NULL, `+at(110)+`, `+at(110)+`),
			(3, 2, '2.25.2.3', 'scan-1', 'scan-1', 1, 7, NULL, `+at(150)+`, `+at(160)+`),
			(4, 3, '2.25.2.4', 'scan-1', 'scan-1', 1, 9, NULL, `+at(200)+`, `+at(200)+`)`,
		held("2.25.3.2", "2.25.1.1", "2.25.2.1", "rc4", 1, 20, 120), held("2.25.3.1", "2.25.1.1", "2.25.2.1", "rc1", 1, 10, 100),
		held("2.25.3.3", "2.25.1.1", "2.25.2.2", "rc1", 2, 5, 110), held("2.25.3.4", "2.25.1.2", "2.25.2.3", "rc2", 3, 7, 150),
		held("2.25.3.5", "2.25.1.3", "2.25.2.4", "rc3", 4, 9, 200),
		`INSERT INTO instances VALUES ('2.25.3.9', '2.25.1.1', '2.25.2.1', 'claimed', sha256('x'::bytea), 'rc4', NULL, '', '', 0, 0, 0,
			NULL, now(), NULL)`)
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	tenanttest.CheckMigratedCatalog(t, s, microsTime)
}

// TestLeanCatalogMigrationKeepsRows guards the migration that moves claims
// into their own table, writes held instances once without the key and
// checksum other rows give, counts each reception's scans, and removes the
// received files of receptions that ended done or quarantined, on a
// database that holds each kind of row already (tenanttest.CheckLeanCatalog
// describes them and reads them back).
func TestLeanCatalogMigrationKeepsRows(t *testing.T) {
	ctx := context.Background()
	address := newSchema(t)
	s, err := OpenTenant(ctx, address, "t1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if _, err := s.migrate(ctx, tenantMigrations[:8]); err != nil {
		t.Fatal(err)
	}
	reception := func(id string, arrival int, state string, ended bool) string {
		return fmt.Sprintf(`INSERT INTO receptions VALUES ('%s', 'recv', '%s', %d, '%s', %t, 1, 10, 0, 0, 0, 0, 0, 0, 0,
			'Sub-A', 'ses-1', 'store', '', now(), now(), now(), %s, 0, '[]', false, '', '', '', NULL)`,
			id, tenanttest.LeanStudy, arrival, state, ended, map[bool]string{true: "now()", false: "NULL"}[ended])
	}
	sum := func(uid string) string { return fmt.Sprintf("sha256('%s'::bytea)", uid) }
	received := func(id string) string {
		return fmt.Sprintf(`INSERT INTO received_files VALUES ('%s', '%s', %s, '%s', '%s', '1.2.840.10008.5.1.4.1.1.4', '1.2.840.10008.1.2.1',
			10, 'in/%s', '', '', '', '', false, now())`, id, tenanttest.LeanReceivedUID, sum(tenanttest.LeanReceivedUID), tenanttest.LeanSeries,
			tenanttest.LeanStudy, id)
	}
	mustExec(t, admin(t, address),
		reception(tenanttest.LeanDone, 1, "done", true), reception(tenanttest.LeanQuarantined, 2, "quarantined", true),
		reception(tenanttest.LeanFailed, 3, "failed", true), reception(tenanttest.LeanOpen, 4, "identified", false),
		`INSERT INTO subjects (id, label, folded, created) OVERRIDING SYSTEM VALUE VALUES (1, 'Sub-A', 'sub-a', now())`,
		`INSERT INTO sessions (id, study_uid, subject_id, label, folded, storage, created, subject_label, subject_folded)
			OVERRIDING SYSTEM VALUE VALUES (1, '`+tenanttest.LeanStudy+`', 1, 'ses-1', 'ses-1', 'store', now(), 'Sub-A', 'sub-a')`,
		`INSERT INTO scans (id, session_id, series_uid, label, folded, instances, bytes, manifest_sha256, created, updated,
			subject_label, subject_folded, session_label, session_folded, storage)
			OVERRIDING SYSTEM VALUE VALUES (1, 1, '`+tenanttest.LeanSeries+`', 'scan-1', 'scan-1', 2, 30, NULL, now(), now(),
			'Sub-A', 'sub-a', 'ses-1', 'ses-1', 'store')`,
		fmt.Sprintf(`INSERT INTO instances VALUES ('%s', '%s', '%s', 'held', %s, '%s', 1, 'store', 'data/Sub-A/ses-1/scan-1/%s.dcm', 0, 10, 10,
			%s, now(), now())`, tenanttest.LeanLayoutUID, tenanttest.LeanStudy, tenanttest.LeanSeries, sum(tenanttest.LeanLayoutUID),
			tenanttest.LeanDone, tenanttest.LeanLayoutUID, sum(tenanttest.LeanLayoutUID)),
		fmt.Sprintf(`INSERT INTO instances VALUES ('%s', '%s', '%s', 'held', %s, '%s', 1, 'store', '%s', 0, 20, 20, %s, now(), now())`,
			tenanttest.LeanKeptUID, tenanttest.LeanStudy, tenanttest.LeanSeries, sum(tenanttest.LeanKeptUID), tenanttest.LeanDone,
			tenanttest.LeanKeptKey, sum(tenanttest.LeanKeptUID+"x")),
		fmt.Sprintf(`INSERT INTO instances VALUES ('%s', '%s', '%s', 'claimed', %s, '%s', NULL, '', '', 0, 0, 0, NULL, now(), NULL)`,
			tenanttest.LeanClaimedUID, tenanttest.LeanStudy, tenanttest.LeanSeries, sum(tenanttest.LeanClaimedUID), tenanttest.LeanOpen),
		received(tenanttest.LeanDone), received(tenanttest.LeanQuarantined), received(tenanttest.LeanFailed), received(tenanttest.LeanOpen))
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	tenanttest.CheckLeanCatalog(t, s)
	var kept string
	if err := admin(t, address).QueryRow(ctx, `SELECT string_agg(sop_instance_uid || ':' || storage_key || ':' ||
		(archived_sha256 IS NULL), ' ' ORDER BY sop_instance_uid) FROM instances`).Scan(&kept); err != nil {
		t.Fatal(err)
	}
	if want := tenanttest.LeanLayoutUID + "::true " + tenanttest.LeanKeptUID + ":" + tenanttest.LeanKeptKey + ":false"; kept != want {
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
	address := newSchema(t)
	s, err := OpenTenant(ctx, address, "t1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if _, err := s.migrate(ctx, tenantMigrations[:8]); err != nil {
		t.Fatal(err)
	}
	conn := admin(t, address)
	mustExec(t, conn,
		`INSERT INTO subjects (id, label, folded, created) OVERRIDING SYSTEM VALUE VALUES (1, 'sub-a', 'sub-a', now())`,
		`INSERT INTO sessions (id, study_uid, subject_id, label, folded, storage, created) OVERRIDING SYSTEM VALUE
			VALUES (1, '2.25.1.1', 1, 'ses-1', 'ses-1', 'store', now())`,
		`INSERT INTO scans (id, session_id, series_uid, label, folded, instances, bytes, manifest_sha256, created, updated, storage)
			OVERRIDING SYSTEM VALUE VALUES (1, 1, '2.25.2.1', 'scan-1', 'scan-1', 1, 10, NULL, now(), now(), 'store')`,
		`INSERT INTO instances VALUES ('2.25.3.1', '2.25.1.1', '2.25.2.1', 'held', sha256('a'::bytea), 'rc', 1, 'elsewhere', 'k', 0, 10, 10,
			sha256('a'::bytea), now(), now())`)
	err = s.Migrate(ctx)
	if err == nil || !strings.Contains(err.Error(), "a held instance is on a different storage than its scan") {
		t.Fatalf("migrating with a mismatched storage: %v, want a refusal that names it", err)
	}
	var version, held int
	if err := conn.QueryRow(ctx, `SELECT (SELECT max(version) FROM schema_migrations),
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
	for _, q := range []string{`CREATE TABLE import_arrivals (reception_id text)`, `CREATE TABLE import_jobs (receiver text)`} {
		if _, err := s.pool.Exec(context.Background(), q); err != nil {
			t.Fatal(err)
		}
	}
}

// takeBackImportArrivals drops the tables lendImportArrivals lent, before
// the migrations run.
func takeBackImportArrivals(t *testing.T, s *Tenant) {
	t.Helper()
	for _, q := range []string{`DROP TABLE import_arrivals`, `DROP TABLE import_jobs`} {
		if _, err := s.pool.Exec(context.Background(), q); err != nil {
			t.Fatal(err)
		}
	}
}

// correctionHoldTables are, empty, the tables a scan's commit looks in for
// labels and UIDs a correction holds, in the columns it reads.
var correctionHoldTables = []string{
	`hidden_subjects (correction_id text, folded text)`,
	`hidden_sessions (correction_id text, study_uid text, subject_folded text, folded text)`,
	`hidden_scans (correction_id text, series_uid text, subject_folded text, session_folded text, folded text)`,
	`hidden_instances (correction_id text, sop_instance_uid text)`,
	`correction_scans (correction_id text, subject_label text, session_label text, scan_label text, new_subject text, new_session text, new_scan text)`,
	`corrections (id text, kind text, level text, state text)`,
}

// lendCorrectionHolds lends a schema before the corrections' the tables a
// scan's commit looks in for what a correction holds (empty, so it finds
// none), for a fill that commits through the port.
func lendCorrectionHolds(t *testing.T, s *Tenant) {
	t.Helper()
	for _, q := range correctionHoldTables {
		if _, err := s.pool.Exec(context.Background(), `CREATE TABLE `+q); err != nil {
			t.Fatal(err)
		}
	}
}

// takeBackCorrectionHolds drops the tables lendCorrectionHolds lent, before
// the migrations run.
func takeBackCorrectionHolds(t *testing.T, s *Tenant) {
	t.Helper()
	for _, q := range correctionHoldTables {
		if _, err := s.pool.Exec(context.Background(), `DROP TABLE `+strings.Fields(q)[0]); err != nil {
			t.Fatal(err)
		}
	}
}

// lendInstanceDamage lends a schema before the scrub's the table every read
// of a job's stream probes for a scrub's marks (empty, so it finds none),
// for a fill that reads through the port.
func lendInstanceDamage(t *testing.T, s *Tenant) {
	t.Helper()
	if _, err := s.pool.Exec(context.Background(), `CREATE TABLE instance_damage (sop_instance_uid text, finding_id text)`); err != nil {
		t.Fatal(err)
	}
}

// takeBackInstanceDamage drops the table lendInstanceDamage lent, before
// the migrations run.
func takeBackInstanceDamage(t *testing.T, s *Tenant) {
	t.Helper()
	if _, err := s.pool.Exec(context.Background(), `DROP TABLE instance_damage`); err != nil {
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
	s, err := OpenTenant(ctx, newSchema(t), "t1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if _, err := s.migrate(ctx, tenantMigrations[:10]); err != nil {
		t.Fatal(err)
	}
	// The port of this version reads and writes columns a later migration
	// adds (a reception's and a session's route, a release's storage);
	// they are lent for the fill, which writes through the port, and taken
	// back before the migrations run.
	lent := []string{"receptions ADD COLUMN route", "sessions ADD COLUMN route", "quarantine ADD COLUMN release_storage",
		"quarantine ADD COLUMN release_profile", "quarantine ADD COLUMN import_job", "quarantine ADD COLUMN import_path"}
	for _, c := range lent {
		if _, err := s.pool.Exec(ctx, `ALTER TABLE `+c+` text NOT NULL DEFAULT ''`); err != nil {
			t.Fatal(err)
		}
	}
	lendImportArrivals(t, s)
	lendCorrectionHolds(t, s)
	tenanttest.FillBeforeSending(t, s)
	for _, c := range lent {
		if _, err := s.pool.Exec(ctx, `ALTER TABLE `+strings.Replace(c, "ADD", "DROP", 1)); err != nil {
			t.Fatal(err)
		}
	}
	takeBackCorrectionHolds(t, s)
	takeBackImportArrivals(t, s)
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	// The pool's connections keep the plans of statements the fill ran on
	// the lent table and columns; a server migrates before it serves, so
	// they are dropped here as a new start would have none.
	s.pool.Reset()
	if v, err := s.Version(ctx); err != nil || v != len(tenantMigrations) {
		t.Fatalf("the version after the migration = %d, %v", v, err)
	}
	tenanttest.CheckAfterSending(t, s)
}

// TestDeidJobsMigrationKeepsJobs guards the migration that lets a job be
// of the kind deid: send jobs written through the port at the schema
// before it (tenanttest.FillBeforeDeidJobs, the new columns lent for the
// port and taken back) read exactly as before and go on working after it
// (tenanttest.CheckAfterDeidJobs); the indexes of jobs are as they were;
// and the checks hold: a kind other than send or deid, and a send job with
// a mode, are refused.
func TestDeidJobsMigrationKeepsJobs(t *testing.T) {
	ctx := context.Background()
	s, err := OpenTenant(ctx, newSchema(t), "t1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if _, err := s.migrate(ctx, tenantMigrations[:14]); err != nil {
		t.Fatal(err)
	}
	lent := []string{"deid_mode text NOT NULL DEFAULT ''", "profile_name text NOT NULL DEFAULT ''", "profile_version integer NOT NULL DEFAULT 0"}
	for _, c := range lent {
		if _, err := s.pool.Exec(ctx, `ALTER TABLE jobs ADD COLUMN `+c); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.pool.Exec(ctx, `CREATE TABLE job_tallies (job_id text, tally text, count bigint)`); err != nil {
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
		if _, err := s.pool.Exec(ctx, `ALTER TABLE jobs DROP COLUMN `+strings.Fields(c)[0]); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.pool.Exec(ctx, `DROP TABLE job_tallies`); err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	// The pool's connections keep the plans of statements the fill ran on
	// the lent columns; a server migrates before it serves, so they are
	// dropped here as a new start would have none.
	s.pool.Reset()
	var indexes string
	if err := s.pool.QueryRow(ctx, `SELECT string_agg(indexname, ',' ORDER BY indexname) FROM pg_indexes
		WHERE schemaname = current_schema() AND tablename = 'jobs'`).Scan(&indexes); err != nil {
		t.Fatal(err)
	}
	if indexes != "jobs_by_created,jobs_by_sender,jobs_by_state,jobs_in_progress,jobs_key,jobs_of_sender_in_progress,jobs_pkey" {
		t.Errorf("the indexes of jobs after the migration: %s", indexes)
	}
	for _, stmt := range []string{
		`UPDATE jobs SET kind = 'other' WHERE id = 'j-queued'`,
		`UPDATE jobs SET deid_mode = 'check' WHERE id = 'j-queued'`,
	} {
		if _, err := s.pool.Exec(ctx, stmt); err == nil {
			t.Errorf("the migrated jobs take %s", stmt)
		}
	}
	tenanttest.CheckAfterDeidJobs(t, s, before)
}
