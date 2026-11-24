package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/master/mastertest"
)

// openTest opens a fresh, migrated master in a new file under tb's
// temporary directory, closed when tb ends.
func openTest(tb testing.TB) master.Store {
	tb.Helper()
	ctx := context.Background()
	m, err := OpenMaster(ctx, filepath.Join(tb.TempDir(), "master.db"))
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { _ = m.Close() })
	if err := m.Migrate(ctx); err != nil {
		tb.Fatal(err)
	}
	return m
}

// TestMasterContract runs the master port's contract suite on SQLite.
func TestMasterContract(t *testing.T) {
	mastertest.Run(t, openTest)
}

// TestShippedMigrationsUnchanged guards the master's migration files against
// an edit nobody meant: a changed baseline makes every database made from it
// refuse to open (sqlkit.ErrEarlierBuild), a changed later file every
// database that applied it (sqlkit.ErrChecksum). Until a first release the
// baseline is edited in place and its checksum here with it, on purpose,
// since every database made before is then made again; from the first
// release on a listed checksum is never changed, and a new file is added
// to the list.
func TestShippedMigrationsUnchanged(t *testing.T) {
	want := []string{
		"0001_baseline.sql " + shippedBaseline,
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

// shippedBaseline is the SHA-256 of migrations/master/0001_baseline.sql as
// this change ships it.
const shippedBaseline = "ab555b2ae64feac32d21248729a69d4e9b86c13b9e2be48dad04827e77836925"

// openPair opens two stores on one fresh, migrated file, as two processes
// would, for the suite's tests across instances.
func openPair(tb testing.TB) (master.Store, master.Store) {
	tb.Helper()
	ctx := context.Background()
	path := filepath.Join(tb.TempDir(), "master.db")
	var out [2]master.Store
	for i := range out {
		m, err := OpenMaster(ctx, path)
		if err != nil {
			tb.Fatal(err)
		}
		tb.Cleanup(func() { _ = m.Close() })
		if err := m.Migrate(ctx); err != nil {
			tb.Fatal(err)
		}
		out[i] = m
	}
	return out[0], out[1]
}

// TestMasterContractShared runs the suite's tests that need two instances on
// one database.
func TestMasterContractShared(t *testing.T) {
	mastertest.RunShared(t, openPair)
}

// TestPragmas guards the settings every connection of each pool is opened
// with, read back from SQLite itself, since a DSN key the driver did not
// understand would be ignored silently. All the read pool's connections are
// held at once, so each is a different connection.
func TestPragmas(t *testing.T) {
	ctx := context.Background()
	p, err := openPools(ctx, filepath.Join(t.TempDir(), "master.db"), masterDB)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.close() })
	common := map[string]string{"journal_mode": "wal", "foreign_keys": "1", "synchronous": "2", "busy_timeout": "10000", "fullfsync": "1", "checkpoint_fullfsync": "1"}
	check := func(name string, conn *sql.Conn, queryOnly string) {
		t.Helper()
		want := maps.Clone(common)
		want["query_only"] = queryOnly
		for pragma, value := range want {
			var got string
			if err := conn.QueryRowContext(ctx, "PRAGMA "+pragma).Scan(&got); err != nil {
				t.Fatalf("%s PRAGMA %s: %v", name, pragma, err)
			}
			if got != value {
				t.Errorf("%s: PRAGMA %s = %s, want %s", name, pragma, got, value)
			}
		}
	}
	w, err := p.write.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	check("the write connection", w, "0")
	_ = w.Close()
	var held []*sql.Conn
	for i := range readConnections {
		c, err := p.read.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, c)
		check(fmt.Sprintf("read connection %d", i+1), c, "1")
		if _, err := c.ExecContext(ctx, "CREATE TABLE x (y INTEGER)"); err == nil {
			t.Errorf("read connection %d wrote", i+1)
		}
	}
	if got := p.read.Stats().OpenConnections; got != readConnections {
		t.Errorf("%d read connections open, want %d distinct ones held", got, readConnections)
	}
	for _, c := range held {
		_ = c.Close()
	}
}

// TestRetryBusy guards the retry of an open that SQLite answers with
// SQLITE_BUSY at once, which happens when processes switch a new file to
// write-ahead mode together and cannot be made to happen on demand. A real
// SQLITE_BUSY error is taken from a file locked by another connection, and
// handed back by a fake open a given number of times.
func TestRetryBusy(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "locked.db")
	holder, err := sql.Open("sqlite", path+"?_txlock=exclusive")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Close() }()
	tx, err := holder.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, "CREATE TABLE t (x INTEGER)"); err != nil {
		t.Fatal(err)
	}
	other, err := sql.Open("sqlite", path+"?_busy_timeout=0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = other.Close() }()
	_, busy := other.ExecContext(ctx, "CREATE TABLE u (x INTEGER)")
	if !isBusy(busy) {
		t.Fatalf("could not make a real SQLITE_BUSY: %v", busy)
	}

	calls := 0
	mode, err := retryBusy(ctx, busyTimeout, func() (string, error) {
		calls++
		if calls <= 3 {
			return "", busy
		}
		return "wal", nil
	})
	if err != nil || mode != "wal" || calls != 4 {
		t.Errorf("retryBusy = %q, %v after %d calls; want wal after 4", mode, err, calls)
	}
	calls = 0
	_, err = retryBusy(ctx, 50*time.Millisecond, func() (string, error) { calls++; return "", busy })
	if !isBusy(err) || calls < 2 {
		t.Errorf("retryBusy past its wait = %v after %d calls; want the busy error after several", err, calls)
	}
	calls = 0
	other2 := errors.New("not busy")
	if _, err := retryBusy(ctx, busyTimeout, func() (string, error) { calls++; return "", other2 }); !errors.Is(err, other2) || calls != 1 {
		t.Errorf("retryBusy on another error = %v after %d calls; want it at once", err, calls)
	}
}

// TestPanicInWriteReleasesTheWriter guards the deferred rollback: a panic
// inside a change must end its transaction, or the pool's only write
// connection would stay taken and every later write would wait forever.
func TestPanicInWriteReleasesTheWriter(t *testing.T) {
	ctx := context.Background()
	m := openTest(t).(*Master)
	func() {
		defer func() { _ = recover() }()
		_ = m.write(ctx, "test", func(tx *sql.Tx, s *statements) error {
			if _, err := tx.StmtContext(ctx, s.upsertSetting).ExecContext(ctx, "half", "done"); err != nil {
				return err
			}
			panic("inside a change")
		})
	}()
	deadline, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := m.SetSetting(deadline, "after", "v", master.AuditEntry{ID: "a1", Time: mastertest.T0, Method: "Test", Outcome: master.OutcomeDone}, master.LocalOperator()); err != nil {
		t.Fatalf("a write after a panic in a change: %v", err)
	}
	if _, err := m.Setting(ctx, "half"); fault.KindOf(err) != fault.NotFound {
		t.Errorf("the panicked change was committed (%v)", err)
	}
}

// TestFailingFiles guards the kind and message of a file the driver cannot
// use: one that is not a SQLite database, and one that cannot be written.
// Both are Unavailable, with a message that says what is wrong and names no
// path.
func TestFailingFiles(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	notDB := filepath.Join(dir, "notes.db")
	if err := os.WriteFile(notDB, []byte(strings.Repeat("this is not a database\n", 400)), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := OpenMaster(ctx, notDB)
	if fault.KindOf(err) != fault.Unavailable || !strings.Contains(fault.Message(err), "not a SQLite database") {
		t.Errorf("OpenMaster on a text file = %v, want Unavailable saying it is not a SQLite database", err)
	}
	if err != nil && strings.Contains(err.Error(), dir) {
		t.Errorf("the error names the path: %v", err)
	}

	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("file modes do not stop this user writing")
	}
	ro := filepath.Join(dir, "ro.db")
	m, err := OpenMaster(ctx, ro)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(ro, 0o400); err != nil {
		t.Fatal(err)
	}
	_, err = OpenMaster(ctx, ro)
	if fault.KindOf(err) != fault.Unavailable || !strings.Contains(fault.Message(err), "cannot be opened for reading and writing") {
		t.Errorf("OpenMaster on a read-only file = %v, want Unavailable saying it cannot be written", err)
	}
	if err != nil && strings.Contains(err.Error(), dir) {
		t.Errorf("the error names the path: %v", err)
	}
}

// TestFileIsPrivate guards that a new master database file is readable by
// its owner only, since it holds key hashes and sealed addresses. Windows
// ignores the mode; the directory's access list decides there.
func TestFileIsPrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file modes are not used on Windows")
	}
	path := filepath.Join(t.TempDir(), "master.db")
	m, err := OpenMaster(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	if err := m.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{path, path + "-wal", path + "-shm"} {
		info, err := os.Stat(f)
		if err != nil {
			t.Fatal(err)
		}
		if mode := info.Mode().Perm(); mode&0o077 != 0 {
			t.Errorf("%s has mode %v, want no access for group or others", filepath.Base(f), mode)
		}
	}
}

// TestOpenRefusesPaths guards the paths the driver would misread: empty,
// in memory, a file: URI and one with a "?", each Invalid, with the path
// not repeated in the error.
func TestOpenRefusesPaths(t *testing.T) {
	for _, path := range []string{"", ":memory:", "file:/tmp/x.db", "/tmp/a?_pragma=x"} {
		_, err := OpenMaster(context.Background(), path)
		if fault.KindOf(err) != fault.Invalid {
			t.Errorf("OpenMaster(%q) = %v, want Invalid", path, err)
		}
		if path != "" && err != nil && strings.Contains(err.Error(), path) {
			t.Errorf("the error repeats the path: %v", err)
		}
	}
	_, err := OpenMaster(context.Background(), filepath.Join(t.TempDir(), "no", "such", "dir", "m.db"))
	if fault.KindOf(err) != fault.Unavailable {
		t.Errorf("a path in no directory: %v, want Unavailable", err)
	}
}

// TestNotMigrated guards that a store used before Migrate fails as
// Unavailable with a message that says so, instead of a SQL error.
func TestNotMigrated(t *testing.T) {
	m, err := OpenMaster(context.Background(), filepath.Join(t.TempDir(), "master.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	if v, err := m.Version(context.Background()); err != nil || v != 0 {
		t.Errorf("Version before Migrate = %d, %v; want 0", v, err)
	}
	_, err = m.Key(context.Background(), "k")
	if fault.KindOf(err) != fault.Unavailable || !strings.Contains(fault.Message(err), "migrated") {
		t.Errorf("Key before Migrate = %v, want Unavailable saying it is not migrated", err)
	}
}

// TestWriteSample writes the contract suite's sample data into a database
// for the outside check (scripts/check/master-sqlite.sh), which reads it
// with the sqlite3 tool. It runs only when NOPACS_SAMPLE_DIR names a
// directory; there it leaves master.db and the sample key's hash in hex,
// and never the secret, which the script holds itself.
func TestWriteSample(t *testing.T) {
	dir := os.Getenv("NOPACS_SAMPLE_DIR")
	if dir == "" {
		t.Skip("NOPACS_SAMPLE_DIR is not set; scripts/check/master-sqlite.sh sets it")
	}
	path := filepath.Join(dir, "master.db")
	ctx := context.Background()
	m, err := OpenMaster(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	mastertest.WriteSample(t, m)
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(mastertest.SampleSecret))
	hashFile := filepath.Join(dir, "hash.hex")
	if err := os.WriteFile(hashFile, []byte(hex.EncodeToString(sum[:])+"\n"), 0o600); err != nil { // #nosec G703 -- the directory is the check script's own temporary one.
		t.Fatal(err)
	}
}
