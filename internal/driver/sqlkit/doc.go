// Package sqlkit holds what the SQL database drivers share: the migration
// runner, the opaque cursors their lists are paged by, the encodings of the
// values both keep alike (a checksum column, a JSON column, a receiver's
// configuration, the details a store adds to an audit entry), so a value
// written by one driver reads the same in the other; the catalog's reads;
// and the senders and jobs of a tenant (Sending), one implementation run in
// each driver's transactions.
//
// # Migrations
//
// A driver embeds its schema as forward-only SQL files named
// 0001_<what>.sql, 0002_<what>.sql and so on, numbered from 1 with no gap.
// Migrate applies the ones a database lacks, in order, each in its own
// transaction begun under a lock that holds across processes (the driver's
// Dialect says how its database takes that lock), and records each applied
// file with its SHA-256. Before it applies anything it refuses a database
// whose recorded checksum for a file no longer matches the file, or that
// records a version this binary has no file for: either means the schema is
// not what this binary's queries were written against, and going on would
// corrupt data or fail later in a place harder to understand.
//
// The first file, 0001_baseline.sql, is the whole schema of its database
// (the master, or a tenant), each table written once. Until a first
// release a schema change edits it in place, so the SHA-256 recorded for it
// is the fingerprint of the schema a database was made from: one that
// differs, by name or checksum, is refused as made by an earlier
// development build (ErrEarlierBuild), with a message saying to make it
// again, before the checks of later files; it is never migrated across.
// From a first release on the baseline is fixed and later changes are files
// after it. Fingerprint combines the drivers' baselines of one database, for
// the files that carry its rows between installations.
//
// A file whose first line is exactly
//
//	-- nopacs:no-transaction
//
// runs outside a transaction, for a statement a database refuses inside one
// (Postgres's CREATE INDEX CONCURRENTLY). Such a file is recorded only after
// it has run, so it must be written to be run again safely. No migration
// shipped so far needs one.
//
// The runner assumes two things of a database: that DDL (CREATE TABLE,
// CREATE INDEX) runs inside a transaction and is undone with it, and that one
// Exec may carry several statements separated by semicolons. SQLite and
// Postgres both do. A driver for a database without them (MySQL commits DDL
// at once; some drivers take one statement per call) must bring its own way
// to apply a file, and must not use Migrate as it is.
//
// # Cursors
//
// EncodeCursor and DecodeCursor make the page tokens of every SQL driver's
// lists, so all of them refuse a token the same way. A cursor names its
// format version, its list, and a binding of the arguments the list was
// called with (a key id, a scope, or Binding of a whole filter), and ends
// with a check of its bytes; DecodeCursor refuses any mismatch as Invalid.
//
// # What this package must never do
//
// It must never import a database driver: a driver passes in its *sql.DB and
// its Dialect, so this package compiles into a binary with no SQL database at
// all. It may use the ports' types (package master, package tenant), never
// their rules: a rule of the domain lives in its port, where a database
// with no SQL would find it too. It never puts a database address in an
// error.
package sqlkit
