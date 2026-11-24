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
)

// testMigrations are migrations that fail if run twice: the first makes a
// table without IF NOT EXISTS, and each logs its version into it, so a file
// applied twice either fails or leaves two log rows. The first three sleep
// inside their transaction, holding what they hold, so that processes that
// start together are inside each other's windows. first is added to the
// first file, second to the second.
func testMigrations(t *testing.T, first, second string) []sqlkit.Migration {
	t.Helper()
	fsys := fstest.MapFS{
		"m/0001_log.sql":   {Data: []byte("CREATE TABLE log (version integer NOT NULL);\n" + slowInsert(1) + first)},
		"m/0002_more.sql":  {Data: []byte(slowInsert(2) + second)},
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
	ms := testMigrations(t, "", "")
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

// TestMigrateRefusesChangedFile guards the checksum of a migration after the
// baseline: a database that applied a file refuses to start with a binary
// whose copy of that file differs, names the file, and applies nothing
// further.
func TestMigrateRefusesChangedFile(t *testing.T) {
	ctx := context.Background()
	address := newSchema(t)
	if _, err := migrateAt(ctx, address, testMigrations(t, "", "")[:2], time.Time{}); err != nil {
		t.Fatal(err)
	}
	_, err := migrateAt(ctx, address, testMigrations(t, "", "-- edited after release\n"), time.Time{})
	if !errors.Is(err, sqlkit.ErrChecksum) {
		t.Fatalf("Migrate with a changed file = %v, want ErrChecksum", err)
	}
	if !strings.Contains(fault.Message(err), "0002_more.sql") {
		t.Errorf("message %q does not name the file", fault.Message(err))
	}
	t.Logf("refused: %s", fault.Message(err))
	if got := fmt.Sprint(logRows(t, address)); got != "[1 2]" {
		t.Errorf("log = %s after the refusal, want [1 2]: nothing more is applied", got)
	}
}

// TestMigrateRefusesEarlierBaseline guards the baseline's fingerprint: a
// database made from another version of the first file (the baseline
// schema, edited in place until a first release) is refused as made by an
// earlier development build, with a message that says to make it again,
// before anything is applied, even when it records files this binary has
// no more.
func TestMigrateRefusesEarlierBaseline(t *testing.T) {
	ctx := context.Background()
	address := newSchema(t)
	if _, err := migrateAt(ctx, address, testMigrations(t, "", ""), time.Time{}); err != nil {
		t.Fatal(err)
	}
	_, err := migrateAt(ctx, address, testMigrations(t, "-- the baseline edited\n", "")[:1], time.Time{})
	if !errors.Is(err, sqlkit.ErrEarlierBuild) || fault.KindOf(err) != fault.Conflict {
		t.Fatalf("Migrate with another baseline = %v, want ErrEarlierBuild as Conflict", err)
	}
	for _, want := range []string{"earlier development build", "0001_log.sql", "make it again"} {
		if !strings.Contains(fault.Message(err), want) {
			t.Errorf("message %q does not say %q", fault.Message(err), want)
		}
	}
	t.Logf("refused: %s", fault.Message(err))
	if got := fmt.Sprint(logRows(t, address)); got != "[1 2 3 5]" {
		t.Errorf("log = %s after the refusal, want [1 2 3 5]: nothing is applied", got)
	}
}

// TestMigrateRefusesNewerDatabase guards that a database a newer binary
// upgraded, recording a migration this binary has no file for, is refused
// rather than used with queries written for an older schema.
func TestMigrateRefusesNewerDatabase(t *testing.T) {
	ctx := context.Background()
	address := newSchema(t)
	ms := testMigrations(t, "", "")
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

// earlierBuilds are the records of schema_migrations an earlier development
// build left: its baseline of another SHA-256, and the chain of files the
// baseline replaced, whose first was named 0001_init.sql.
var earlierBuilds = map[string]string{
	"another baseline":   `UPDATE schema_migrations SET checksum = '\x00'::bytea WHERE version = 1`,
	"the replaced chain": `UPDATE schema_migrations SET name = '0001_init.sql' WHERE version = 1`,
}

// TestStoresRefuseEarlierBuilds guards the baseline's fingerprint through
// both ports, with their real baselines: a master or a tenant schema whose
// recorded baseline is not this binary's (another SHA-256, or the first file
// of the chain the baseline replaced, with later files recorded after it)
// is refused by Migrate with the plain message, and is left as it was.
func TestStoresRefuseEarlierBuilds(t *testing.T) {
	ctx := context.Background()
	type store interface {
		Migrate(context.Context) error
		Close() error
	}
	open := map[string]func(address string) store{
		"master": func(address string) store { return openAt(t, address) },
		"tenant": func(address string) store {
			s, err := OpenTenant(ctx, address, "t1")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Close() })
			return s
		},
	}
	for kind, opener := range open {
		for name, change := range earlierBuilds {
			t.Run(kind+"/"+name, func(t *testing.T) {
				address := newSchema(t)
				s := opener(address)
				if err := s.Migrate(ctx); err != nil {
					t.Fatal(err)
				}
				conn := admin(t, address)
				mustExec(t, conn, change, `INSERT INTO schema_migrations VALUES (2, '0002_pipeline.sql', '\x00'::bytea, now())`)
				err := s.Migrate(ctx)
				if !errors.Is(err, sqlkit.ErrEarlierBuild) || !strings.Contains(fault.Message(err), "made by an earlier development build") ||
					!strings.Contains(fault.Message(err), "make it again") {
					t.Fatalf("Migrate of an earlier build's %s = %v, want ErrEarlierBuild saying to make it again", kind, err)
				}
				t.Logf("refused: %s", fault.Message(err))
				var rows int
				if err := conn.QueryRow(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&rows); err != nil || rows != 2 {
					t.Errorf("schema_migrations holds %d rows after the refusal (%v), want the 2 it had", rows, err)
				}
			})
		}
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
// success, a failed file, a refused baseline, and a request that ends while
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
	if _, err := m.migrate(ctx, changedMigrations); !errors.Is(err, sqlkit.ErrEarlierBuild) {
		t.Fatalf("another baseline: %v, want ErrEarlierBuild", err)
	}
	check("a refused baseline")

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
		ms := testMigrations(t, "", "")
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
	v, err := migrateAt(context.Background(), address, testMigrations(t, "", ""), time.Unix(0, startNano))
	if err != nil || v != 5 {
		t.Fatalf("Migrate = %d, %v; want 5", v, err)
	}
}
