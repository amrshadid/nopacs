package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/amrshadid/nopacs/internal/fault"
)

// busyTimeout is how long a connection waits for a lock another process holds
// before it fails with SQLITE_BUSY: another nopacs starting and migrating,
// a backup made with the sqlite3 tool, a checkpoint of the write-ahead log.
// Inside the process writers never meet it, since they queue for the one
// write connection. SQLite waits up to this long for the lock and then
// returns SQLITE_BUSY (sqlite.org/pragma.html#pragma_busy_timeout); in
// write-ahead mode a checkpoint or another writer holds it only briefly
// (sqlite.org/wal.html). So it is set well above a moment, long enough that
// a lock held by a short job is waited for rather than failed on.
const busyTimeout = 10 * time.Second

// readConnections is the size of the read pool. The master is small and its
// reads are single indexed lookups, so a few connections serve many
// concurrent requests; each one holds a page cache, so not more.
const readConnections = 4

// dbName names a database in the messages of its errors, so an operator
// told that "the tenant database is read-only" knows which file to look at.
// The file's path is never in a message: a database address never appears
// in an error.
type dbName string

// The two databases this driver opens.
const (
	masterDB dbName = "master database"
	tenantDB dbName = "tenant database"
)

// pools are the two pools of connections to one SQLite file.
type pools struct {
	write *sql.DB
	read  *sql.DB
	// idle is how long a connection may stand unused before it is closed
	// (0 never), kept because database/sql has no getter for it.
	idle time.Duration
}

// openPools opens the write and read pools on the file at path, creating it
// when it does not exist, and checks that it is in write-ahead mode. name is
// the database's name in the messages of its errors.
//
// idle is how long a connection that nothing uses may stay open before it is
// closed, 0 for never. A closed connection is opened again, with its pragmas,
// the next time a pool is asked for one; database/sql never closes a
// connection in use, and the statements the stores prepare on a pool are
// prepared again on a new connection by database/sql itself.
func openPools(ctx context.Context, path string, name dbName, idle time.Duration) (*pools, error) {
	if err := checkPath(path); err != nil {
		return nil, err
	}
	// Before the file is made or opened: an existing file others can read
	// would stay readable, and SQLite would give its -wal and -shm files
	// the same mode.
	if err := checkPrivate(path, name); err != nil {
		return nil, err
	}
	if err := createPrivate(path, name); err != nil {
		return nil, err
	}

	// Every connection, read or write, gets these. modernc.org/sqlite
	// applies each key as a PRAGMA on every new connection, busy_timeout
	// first (driver.go and sqlite.go in v1.62.0), so they hold for every
	// connection a pool opens, not only the first.
	common := url.Values{}
	// busy_timeout: see busyTimeout.
	common.Set("_busy_timeout", strconv.FormatInt(busyTimeout.Milliseconds(), 10))
	// foreign_keys: SQLite leaves them off on each new connection unless
	// asked, and a grant pointing at a removed tenant must be refused.
	common.Set("_foreign_keys", "on")
	// synchronous=FULL: in write-ahead mode NORMAL is safe from corruption
	// but "a transaction committed ... might roll back following a power
	// loss" (sqlite.org/pragma.html). The master's rows are few and writes
	// rare, and a revoked key that comes back after a power cut is a security
	// failure, so every commit is synced.
	common.Set("_synchronous", "FULL")
	// fullfsync: on macOS fsync() does not reach the disk's own cache, and
	// only fcntl(F_FULLFSYNC) does, which SQLite uses only when asked
	// (sqlite.org/pragma.html#pragma_fullfsync). Without it FULL above would
	// not keep its promise there. Other systems ignore it. The checkpoint
	// copies the log into the database, so it is synced the same way.
	common.Add("_pragma", "fullfsync = 1")
	common.Add("_pragma", "checkpoint_fullfsync = 1")

	w := cloneValues(common)
	// journal_mode=WAL: readers do not block the writer nor the writer the
	// readers (sqlite.org/wal.html). The mode is kept in the file, so
	// setting it on the write connection sets it for every connection.
	w.Set("_journal_mode", "WAL")
	// _txlock=immediate: every transaction on this pool begins with BEGIN
	// IMMEDIATE, taking the write lock where the busy timeout applies; a
	// deferred transaction that reads first and writes later fails at once
	// with SQLITE_BUSY when another process wrote meanwhile
	// (sqlite.org/lang_transaction.html).
	w.Set("_txlock", "immediate")

	r := cloneValues(common)
	// query_only: a read connection that tries to write fails, so a write
	// can never bypass the one write connection and its lock order.
	r.Set("_query_only", "on")

	write, err := sql.Open("sqlite", path+"?"+w.Encode())
	if err != nil {
		return nil, fault.Wrap(fault.Internal, err, "the SQLite driver cannot be set up")
	}
	// One connection writes; database/sql queues the others for it. It is
	// closed only when idle (see idle), never while a transaction or a
	// statement runs on it.
	write.SetMaxOpenConns(1)
	write.SetMaxIdleConns(1)
	write.SetConnMaxLifetime(0)
	write.SetConnMaxIdleTime(idle)

	mode, err := journalMode(ctx, write)
	if err != nil {
		_ = write.Close() // The open failure is the one reported.
		return nil, classify(err, name, "the "+string(name)+" cannot be opened")
	}
	if !strings.EqualFold(mode, "wal") {
		_ = write.Close() // The mode is the failure reported.
		return nil, fault.New(fault.Unavailable, "the "+string(name)+" cannot use write-ahead mode; is it on a local disk?")
	}

	read, err := sql.Open("sqlite", path+"?"+r.Encode())
	if err != nil {
		_ = write.Close() // The open failure is the one reported.
		return nil, fault.Wrap(fault.Internal, err, "the SQLite driver cannot be set up")
	}
	read.SetMaxOpenConns(readConnections)
	read.SetMaxIdleConns(readConnections)
	read.SetConnMaxLifetime(0)
	read.SetConnMaxIdleTime(idle)
	if err := read.PingContext(ctx); err != nil {
		_ = write.Close() // The ping failure is the one reported.
		_ = read.Close()
		return nil, classify(err, name, "the "+string(name)+" cannot be opened")
	}
	return &pools{write: write, read: read, idle: idle}, nil
}

// journalMode opens the first write connection, which switches the file to
// write-ahead mode, and returns the mode the file is then in.
//
// Several processes opening one new file at the same moment each try that
// switch, which needs the file to themselves, and SQLite can answer
// SQLITE_BUSY at once, without waiting the busy timeout, to the ones that
// lose (seen in TestMigrateAcrossProcesses: one process in a few rounds).
// So a busy open is tried again, for as long as the busy timeout would have
// waited.
func journalMode(ctx context.Context, write *sql.DB) (string, error) {
	return retryBusy(ctx, busyTimeout, func() (string, error) {
		var mode string
		err := write.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode)
		return mode, err
	})
}

// retryBusy calls try until it returns anything but SQLITE_BUSY, waiting a
// moment between calls, for at most wait. It is apart from journalMode so a
// test can hand it a try that is busy a given number of times, since the race
// it handles cannot be made to happen on demand.
func retryBusy(ctx context.Context, wait time.Duration, try func() (string, error)) (string, error) {
	deadline := time.Now().Add(wait)
	for {
		v, err := try()
		if err == nil || !isBusy(err) || time.Now().After(deadline) {
			return v, err
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// isBusy reports whether err is SQLite's "database is locked".
func isBusy(err error) bool {
	var se *sqlite.Error
	return errors.As(err, &se) && se.Code()&0xff == sqlite3.SQLITE_BUSY
}

// close closes both pools and returns the first error.
func (p *pools) close() error {
	werr := p.write.Close()
	rerr := p.read.Close()
	if werr != nil {
		return werr
	}
	return rerr
}

// checkPath refuses a path the driver would misread. The driver takes what
// follows a "?" as its own parameters, a "file:" path as a URI with SQLite's
// own parameters, and ":memory:" or an empty path as a database private to
// each connection, which the two pools would not share. The message does
// not repeat the path: a database address never appears in an error.
func checkPath(path string) error {
	switch {
	case path == "", path == ":memory:":
		return fault.New(fault.Invalid, "a SQLite database is a file; give its path")
	case strings.ContainsAny(path, "?\x00"):
		return fault.New(fault.Invalid, "a SQLite database's path may not hold ? or a NUL byte")
	case strings.HasPrefix(path, "file:"):
		return fault.New(fault.Invalid, "a SQLite database's path is a plain path, not a file: URI")
	}
	return nil
}

// createPrivate makes the database file, empty and readable by its owner
// only, when it does not exist. SQLite would make it with the process's
// umask, often readable by everyone, and the master holds key hashes and
// sealed addresses, a tenant's database its catalog. SQLite gives the -wal and -shm files it makes the mode
// of the database file (os_unix.c, findCreateFileMode), so they are private
// too. On Windows the mode is ignored and the directory's access list
// decides.
func createPrivate(path string, name dbName) error {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600) // #nosec G304 G703 -- the path is the operator's own choice of database file.
	if err != nil {
		// The cause may name the path, so it is not wrapped: the kind
		// and a fixed message go to the caller, and the operator sees the
		// path in the command they typed.
		var pathErr *os.PathError
		if errors.As(err, &pathErr) {
			return fault.New(fault.Unavailable, "the "+string(name)+" file cannot be opened for reading and writing: "+pathErr.Err.Error())
		}
		return fault.New(fault.Unavailable, "the "+string(name)+" file cannot be opened")
	}
	if err := f.Close(); err != nil {
		return fault.New(fault.Unavailable, "the "+string(name)+" file cannot be opened")
	}
	return nil
}

// cloneValues returns a copy of v.
func cloneValues(v url.Values) url.Values {
	out := make(url.Values, len(v))
	for k, vs := range v {
		out[k] = append([]string(nil), vs...)
	}
	return out
}

// classify turns an error from the driver into a fault with a kind. An error
// that already has one keeps it. message is what the caller is shown; the
// driver's own text, which may say more about the database than a caller
// should learn, stays in the chain for the log. name is the database's name
// in the messages that say what is wrong with its file.
func classify(err error, name dbName, message string) error {
	if err == nil {
		return nil
	}
	var fe *fault.Error
	if errors.As(err, &fe) {
		return err
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, sql.ErrConnDone) || errors.Is(err, sql.ErrTxDone) {
		// A transaction or connection that database/sql ended under the
		// call, which it does when the call's context ends.
		return fault.Wrap(fault.Unavailable, err, message)
	}
	var se *sqlite.Error
	if errors.As(err, &se) {
		// The messages name no path: a database address never appears in
		// an error.
		switch se.Code() & 0xff {
		case sqlite3.SQLITE_READONLY:
			return fault.Wrap(fault.Unavailable, err, "the "+string(name)+" is read-only: its file, or the directory for its -wal and -shm files, cannot be written")
		case sqlite3.SQLITE_NOTADB:
			return fault.Wrap(fault.Unavailable, err, "the "+string(name)+" file is not a SQLite database")
		case sqlite3.SQLITE_CORRUPT:
			return fault.Wrap(fault.Unavailable, err, "the "+string(name)+" file is damaged; restore it from a backup")
		case sqlite3.SQLITE_PERM, sqlite3.SQLITE_AUTH:
			return fault.Wrap(fault.Unavailable, err, "the "+string(name)+" may not be opened by this user")
		case sqlite3.SQLITE_BUSY, sqlite3.SQLITE_LOCKED, sqlite3.SQLITE_IOERR,
			sqlite3.SQLITE_CANTOPEN, sqlite3.SQLITE_FULL, sqlite3.SQLITE_INTERRUPT:
			return fault.Wrap(fault.Unavailable, err, message)
		}
	}
	return fault.Wrap(fault.Internal, err, message)
}

// isUniqueViolation reports whether err is a unique or primary key
// constraint failing.
func isUniqueViolation(err error) bool {
	var se *sqlite.Error
	if !errors.As(err, &se) {
		return false
	}
	code := se.Code()
	return code == sqlite3.SQLITE_CONSTRAINT_UNIQUE || code == sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY
}
