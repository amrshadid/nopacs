// Package sqlite is the SQLite database type: it implements master.Store and
// tenant.Store, each on a SQLite file of its own, through modernc.org/sqlite,
// which is SQLite translated to Go, so the binary stays pure Go and static
// (CGO_ENABLED=0) on every target it ships for, linux/386 and windows/amd64
// included.
//
// # How it reaches the file
//
// Two pools of connections to one file. The write pool has exactly one
// connection, and its transactions begin with BEGIN IMMEDIATE: inside the
// process, writers queue for that connection instead of failing with
// SQLITE_BUSY, and across processes the write lock is taken at the start of a
// transaction, where the busy timeout applies, rather than on its first
// write, where SQLite fails at once without waiting
// (sqlite.org/lang_transaction.html). The read pool has a few connections
// that cannot write (query_only), and in write-ahead mode they read a
// snapshot while the writer works.
//
// # Several changes, one commit
//
// Every commit is synced to the disk (synchronous=FULL, and on macOS
// fullfsync), and the sync, not SQLite's work, is what a change costs: one
// commit per change would hold a store to about as many changes a second as
// the disk takes syncs, a few hundred, however many callers wait. So
// changes are committed in groups (write.go). A change that finds the writer
// idle is written at once by its caller, alone, as a transaction of its own;
// changes that arrive while a transaction is running or syncing queue, and
// the next transaction writes them all, up to a bound, each in a savepoint of
// its own (sqlite.org/lang_savepoint.html). There is no timer: a group forms
// only from callers already waiting.
//
// Each caller keeps what a transaction of its own would give it. Its change
// and its audit entry are written both or neither. An answer, success or
// failure, describes the database as committed. A change that fails or
// panics is rolled back to its savepoint, which undoes it alone, and the
// others are committed. Since a change sees the uncommitted writes of the
// changes before it in the transaction, a failure that came while such a
// write was there (a name taken by a neighbor, say) is answered only after
// the commit, like a success; a failure with no neighbor's write before it
// saw only committed rows and is answered at once. When the transaction is
// undone instead, because SQLite rolled it back itself, as it does after an
// interrupted statement or an I/O error (sqlite.org/lang_transaction.html),
// or because COMMIT failed, the changes whose answers waited for it and
// whose failure may have come from a write now gone run again in a new
// transaction. A caller whose context ends while its change waits in the
// queue is answered at once, and the change is never written; once the
// change has started, it is written whole or not at all, and the caller is
// answered with which, so an error always means nothing was written.
//
// Running a change again is safe because every change function in this
// package only reads, writes and assigns its results to variables it sets
// afresh on each run: it never appends to, counts into or sends from
// anything that outlives the run. A new change method must keep to that
// (see Master.write).
//
// The migration lock is BEGIN IMMEDIATE too: a second process that starts
// migrating waits for the first, under the busy timeout, then finds the files
// applied.
//
// # One process writes a SQLite database
//
// Inside one process a database is one store with one writing connection,
// and writers queue for it in the order they came. A second process with
// its own store on the same file is safe for the data: SQLite's locking lets
// one writer at a time commit, and every change is whole or absent
// (sqlite.org/lockingv3.html, sqlite.org/wal.html). It is not fair. A writer
// that finds the file locked by another process sleeps and tries again, in
// steps that grow to 100 milliseconds, until the busy timeout
// (sqlite.org/c3ref/busy_handler.html, sqlite.org/c3ref/busy_timeout.html);
// a process that keeps writing gives the lock up only for the moment between
// two of its transactions, which the sleeper rarely catches. So a writer in
// one process can wait for as long as another keeps writing, and fails with
// the busy error once that passes the busy timeout. Measured: a store whose
// first write began while another store on the same file made 200 writes
// back to back waited until all 200 were done (1.05 s for 1.03 s of writes).
//
// That is why one server serves a data directory: the server's lock on the
// directory refuses a second one before it opens any database, and the
// server runs every migration itself at start. A tool that writes a SQLite
// database while the server runs, or several servers on one database, is not
// supported. Postgres is the database for anything that needs several
// writing processes.
//
// A SQLite database must be on a local disk. Its write-ahead log does not
// work on a network file system (sqlite.org/wal.html), and that cannot be
// detected reliably, so it is documented, not enforced.
//
// # What this package must never do
//
// It must never be imported by anything but package driver, its own tests,
// and the tests of the few packages that open a real SQLite master: nothing
// else may know that a database is SQLite (a lint rule holds it). It never puts the file's path in an error, a log line or an audit
// entry.
package sqlite
