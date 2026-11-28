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
)

// testMigrations are migrations that fail if run twice: the first makes a
// table without IF NOT EXISTS, and each logs its version into it, so a file
// applied twice either fails or leaves two log rows. The first three spend a
// moment computing inside their transaction, holding the lock, so that
// processes that start together are inside each other's windows. change is
// added to the first file, first and second to the second.
func testMigrations(t *testing.T, first, second string) []sqlkit.Migration {
	t.Helper()
	fsys := fstest.MapFS{
		"m/0001_log.sql":   {Data: []byte("CREATE TABLE log (version INTEGER NOT NULL) STRICT;\n" + slowInsert(1) + first)},
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
	p, err := openPools(ctx, path, masterDB, 0)
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
	p, err := openPools(context.Background(), path, masterDB, 0)
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
	ms := testMigrations(t, "", "")
	for run := range 2 {
		v, err := migrateFile(ctx, path, ms)
		if err != nil || v != 5 {
			t.Fatalf("run %d: Migrate = %d, %v; want 5", run, v, err)
		}
	}
	if got := fmt.Sprint(logRows(t, path)); got != "[1 2 3 5]" {
		t.Errorf("log = %s, want [1 2 3 5]", got)
	}
	p, err := openPools(ctx, path, masterDB, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.close() }()
	if v, err := sqlkit.Version(ctx, p.read, dialect); err != nil || v != 5 {
		t.Errorf("Version = %d, %v; want 5", v, err)
	}
}

// TestMigrateRefusesChangedFile guards the checksum of a migration after the
// baseline: a database that applied a file refuses to start with a binary
// whose copy of that file differs, names the file, and applies nothing
// further.
func TestMigrateRefusesChangedFile(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "m.db")
	if _, err := migrateFile(ctx, path, testMigrations(t, "", "")[:2]); err != nil {
		t.Fatal(err)
	}
	_, err := migrateFile(ctx, path, testMigrations(t, "", "-- edited after release\n"))
	if !errors.Is(err, sqlkit.ErrChecksum) {
		t.Fatalf("Migrate with a changed file = %v, want ErrChecksum", err)
	}
	if !strings.Contains(fault.Message(err), "0002_more.sql") {
		t.Errorf("message %q does not name the file", fault.Message(err))
	}
	t.Logf("refused: %s", fault.Message(err))
	if got := fmt.Sprint(logRows(t, path)); got != "[1 2]" {
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
	path := filepath.Join(t.TempDir(), "m.db")
	if _, err := migrateFile(ctx, path, testMigrations(t, "", "")); err != nil {
		t.Fatal(err)
	}
	_, err := migrateFile(ctx, path, testMigrations(t, "-- the baseline edited\n", "")[:1])
	if !errors.Is(err, sqlkit.ErrEarlierBuild) || fault.KindOf(err) != fault.Conflict {
		t.Fatalf("Migrate with another baseline = %v, want ErrEarlierBuild as Conflict", err)
	}
	for _, want := range []string{"earlier development build", "0001_log.sql", "make it again"} {
		if !strings.Contains(fault.Message(err), want) {
			t.Errorf("message %q does not say %q", fault.Message(err), want)
		}
	}
	t.Logf("refused: %s", fault.Message(err))
	if got := fmt.Sprint(logRows(t, path)); got != "[1 2 3 5]" {
		t.Errorf("log = %s after the refusal, want [1 2 3 5]: nothing is applied", got)
	}
}

// TestMigrateRefusesNewerDatabase guards that a database a newer binary
// upgraded, recording a migration this binary has no file for, is refused
// rather than used with queries written for an older schema.
func TestMigrateRefusesNewerDatabase(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "m.db")
	ms := testMigrations(t, "", "")
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

// earlierBuilds are the records of schema_migrations an earlier development
// build left: its baseline of another SHA-256, and the chain of files the
// baseline replaced, whose first was named 0001_init.sql.
var earlierBuilds = map[string]string{
	"another baseline":   `UPDATE schema_migrations SET checksum = zeroblob(32) WHERE version = 1`,
	"the replaced chain": `UPDATE schema_migrations SET name = '0001_init.sql' WHERE version = 1`,
}

// TestStoresRefuseEarlierBuilds guards the baseline's fingerprint through
// both ports, with their real baselines: a master or a tenant database whose
// recorded baseline is not this binary's (another SHA-256, or the first file
// of the chain the baseline replaced, with later files recorded after it)
// is refused by Migrate with the plain message, and is left as it was.
func TestStoresRefuseEarlierBuilds(t *testing.T) {
	ctx := context.Background()
	type store interface {
		Migrate(context.Context) error
		Close() error
	}
	open := map[string]func(path string) (store, *pools){
		"master": func(path string) (store, *pools) {
			m, err := OpenMaster(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			return m, m.p
		},
		"tenant": func(path string) (store, *pools) {
			s, err := OpenTenant(ctx, path, "t1")
			if err != nil {
				t.Fatal(err)
			}
			return s, s.p
		},
	}
	for kind, opener := range open {
		for name, change := range earlierBuilds {
			t.Run(kind+"/"+name, func(t *testing.T) {
				s, p := opener(filepath.Join(t.TempDir(), kind+".db"))
				t.Cleanup(func() { _ = s.Close() })
				if err := s.Migrate(ctx); err != nil {
					t.Fatal(err)
				}
				for _, q := range []string{change, `INSERT INTO schema_migrations VALUES (2, '0002_pipeline.sql', zeroblob(32), 1)`} {
					if _, err := p.write.ExecContext(ctx, q); err != nil {
						t.Fatal(err)
					}
				}
				err := s.Migrate(ctx)
				if !errors.Is(err, sqlkit.ErrEarlierBuild) || !strings.Contains(fault.Message(err), "made by an earlier development build") ||
					!strings.Contains(fault.Message(err), "make it again") {
					t.Fatalf("Migrate of an earlier build's %s = %v, want ErrEarlierBuild saying to make it again", kind, err)
				}
				t.Logf("refused: %s", fault.Message(err))
				var rows int
				if err := p.read.QueryRowContext(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&rows); err != nil || rows != 2 {
					t.Errorf("schema_migrations holds %d rows after the refusal (%v), want the 2 it had", rows, err)
				}
			})
		}
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
	ms := testMigrations(t, "", "")
	mine, err := openPools(ctx, path, masterDB, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = mine.close() }()
	other, err := openPools(ctx, path, masterDB, 0)
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
	a, err := openPools(ctx, path, masterDB, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.close() }()
	b, err := openPools(ctx, path, masterDB, 0)
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
	v, err := migrateFileAt(context.Background(), path, testMigrations(t, "", ""), time.Unix(0, startNano))
	if err != nil || v != 5 {
		t.Fatalf("Migrate = %d, %v; want 5", v, err)
	}
}
