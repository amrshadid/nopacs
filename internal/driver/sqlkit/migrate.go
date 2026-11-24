package sqlkit

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"strconv"
	"time"

	"github.com/amrshadid/nopacs/internal/fault"
)

// NoTransactionMark is the first line of a migration file that must run
// outside a transaction.
const NoTransactionMark = "-- nopacs:no-transaction"

// ErrChecksum is in the chain of the error Migrate returns when an applied
// migration's recorded name or checksum differs from the file this binary
// carries.
var ErrChecksum = errors.New("an applied migration was changed")

// ErrNewerSchema is in the chain of the error Migrate returns when the
// database records a migration this binary does not have: a newer binary has
// upgraded it.
var ErrNewerSchema = errors.New("the database is at a newer schema version")

// ErrEarlierBuild is in the chain of the error Migrate returns when the
// database's first recorded migration, the baseline schema it was made
// from, differs from this binary's by name or SHA-256. The baseline is
// edited in place until a first release, so such a database was made by
// another development build: it is refused whole, never migrated across a
// schema no file describes, and is made again.
var ErrEarlierBuild = errors.New("the database was made by an earlier development build")

// Migration is one forward-only schema change.
type Migration struct {
	// Version is the file's number, from 1.
	Version int
	// Name is the file's name, such as "0001_baseline.sql".
	Name string
	// SQL is the file's text, with line endings as LF.
	SQL string
	// NoTransaction is set when the file's first line is NoTransactionMark.
	NoTransaction bool
	// Sum is the SHA-256 of SQL.
	Sum [sha256.Size]byte
}

// fileName is the form of a migration file's name.
var fileName = regexp.MustCompile(`^([0-9]{4})_[a-z0-9_]+\.sql$`)

// Load reads the migrations in directory dir of fsys, which must be named
// 0001_<what>.sql onward, numbered from 1 with no gap and no number twice.
// Any other entry in dir is an error, so a misnamed migration is never
// silently left out.
//
// CRLF line endings are read as LF before the checksum is taken, so a binary
// built from a Windows checkout, where git may have converted them, records
// and expects the same checksum as one built on Linux.
func Load(fsys fs.FS, dir string) ([]Migration, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, fmt.Errorf("reading migrations: %w", err)
	}
	var out []Migration
	for _, e := range entries {
		m := fileName.FindStringSubmatch(e.Name())
		if e.IsDir() || m == nil {
			return nil, fmt.Errorf("migration directory %s holds %s, which is not named NNNN_<what>.sql", dir, e.Name())
		}
		version, err := strconv.Atoi(m[1])
		if err != nil {
			return nil, fmt.Errorf("migration %s: %w", e.Name(), err)
		}
		raw, err := fs.ReadFile(fsys, path.Join(dir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("reading migration %s: %w", e.Name(), err)
		}
		text := bytes.ReplaceAll(raw, []byte("\r\n"), []byte("\n"))
		firstLine, _, _ := bytes.Cut(text, []byte("\n"))
		out = append(out, Migration{
			Version:       version,
			Name:          e.Name(),
			SQL:           string(text),
			NoTransaction: string(firstLine) == NoTransactionMark,
			Sum:           sha256.Sum256(text),
		})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no migrations in %s", dir)
	}
	// fs.ReadDir returns names sorted, and the four-digit prefix sorts as
	// its number, so position i must hold version i+1.
	for i, m := range out {
		if m.Version != i+1 {
			return nil, fmt.Errorf("migration %s is out of sequence: want version %04d", m.Name, i+1)
		}
	}
	return out, nil
}

// Fingerprint returns the fingerprint of the baseline schema of one
// database (the master, or a tenant) across database types: the SHA-256, in
// lowercase hexadecimal, of the SHA-256s of baselines, each driver's first
// migration of that database, in the order given. A file that carries a
// database's rows from one installation to another (a backup, a tenant's
// export) records it, so a reader whose baseline differs refuses the file
// before it writes anything, whichever database type either side runs.
func Fingerprint(baselines ...Migration) string {
	h := sha256.New()
	for _, b := range baselines {
		h.Write(b.Sum[:])
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Dialect is what Migrate needs to know about one database type.
type Dialect struct {
	// HasTable returns one row and column: 1 when the table of applied
	// migrations exists, 0 when it does not.
	HasTable string
	// CreateTable creates the table of applied migrations if it does not
	// exist, with the columns version (integer, primary key), name (text),
	// checksum (32 bytes) and applied (integer, microseconds since 1970).
	CreateTable string
	// SelectApplied returns version, name and checksum of every applied
	// migration, in order of version.
	SelectApplied string
	// InsertApplied records one applied migration; its parameters are
	// version, name, checksum and applied, in that order.
	InsertApplied string
	// BeginLocked begins a transaction that holds the database's migration
	// lock until it ends, waiting for another process that holds it. Every
	// read of the applied table that decides what to apply is made inside
	// one, so two processes cannot both decide to apply the same file.
	BeginLocked func(ctx context.Context, db *sql.DB) (*sql.Tx, error)
}

// applied is one row of the table of applied migrations.
type applied struct {
	version int
	name    string
	sum     []byte
}

// Migrate applies the migrations db lacks, in order, and returns the schema
// version db is then at. Each time, under the lock and before it applies
// anything, it checks that every migration db records matches one of
// migrations by version, name and checksum. It refuses with ErrEarlierBuild
// (Conflict: the database was made from another baseline schema),
// ErrChecksum (Internal: a later migration is not what it was) or
// ErrNewerSchema (Conflict: a newer binary was here) in the chain when one
// does not.
//
// Each file is applied in its own locked transaction that re-reads the
// applied table first: a process that waited for the lock while another
// applied the same file finds it recorded and moves on, so every file is
// applied once however many processes start at the same moment.
//
// The time a file was applied is recorded from the clock: it is a note for
// an operator about the schema, not data of the archive's.
func Migrate(ctx context.Context, db *sql.DB, d Dialect, migrations []Migration) (int, error) {
	if err := lockedExec(ctx, db, d, d.CreateTable); err != nil {
		return 0, unavailable(err, "the table of applied migrations cannot be made")
	}
	for {
		version, done, err := step(ctx, db, d, migrations)
		if err != nil || done {
			return version, err
		}
	}
}

// step applies the first missing migration, if any, and reports whether
// there was none left.
func step(ctx context.Context, db *sql.DB, d Dialect, migrations []Migration) (version int, done bool, err error) {
	tx, err := d.BeginLocked(ctx, db)
	if err != nil {
		return 0, false, unavailable(err, "the migration lock cannot be taken")
	}
	defer func() { _ = tx.Rollback() }() // A no-op after Commit; on failure the first error is the one returned.
	rows, err := readApplied(ctx, tx, d)
	if err != nil {
		return 0, false, err
	}
	if err := check(rows, migrations); err != nil {
		return 0, false, err
	}
	next := len(rows)
	if next == len(migrations) {
		if err := tx.Commit(); err != nil {
			return 0, false, unavailable(err, "the migration check cannot be ended")
		}
		return next, true, nil
	}
	m := migrations[next]
	if m.NoTransaction {
		// The statement must run outside any transaction, so the lock is
		// let go first; such a file is written to be run twice safely.
		if err := tx.Rollback(); err != nil {
			return 0, false, unavailable(err, "the migration lock cannot be released")
		}
		if _, err := db.ExecContext(ctx, m.SQL); err != nil {
			return 0, false, fault.Wrap(fault.Internal, err, "migration "+m.Name+" failed")
		}
		return m.Version, false, record(ctx, db, d, m)
	}
	if _, err := tx.ExecContext(ctx, m.SQL); err != nil {
		return 0, false, fault.Wrap(fault.Internal, err, "migration "+m.Name+" failed")
	}
	if _, err := tx.ExecContext(ctx, d.InsertApplied, m.Version, m.Name, m.Sum[:], time.Now().UnixMicro()); err != nil {
		return 0, false, fault.Wrap(fault.Internal, err, "migration "+m.Name+" cannot be recorded")
	}
	if err := tx.Commit(); err != nil {
		return 0, false, unavailable(err, "migration "+m.Name+" cannot be committed")
	}
	return m.Version, false, nil
}

// record writes the row of a migration that ran outside a transaction,
// unless another process recorded it meanwhile.
func record(ctx context.Context, db *sql.DB, d Dialect, m Migration) error {
	tx, err := d.BeginLocked(ctx, db)
	if err != nil {
		return unavailable(err, "the migration lock cannot be taken")
	}
	defer func() { _ = tx.Rollback() }() // A no-op after Commit.
	rows, err := readApplied(ctx, tx, d)
	if err != nil {
		return err
	}
	if err := check(rows, nil); err != nil {
		return err
	}
	switch {
	case len(rows) >= m.Version:
		// Another process ran and recorded it while the lock was free.
	case len(rows) == m.Version-1:
		if _, err := tx.ExecContext(ctx, d.InsertApplied, m.Version, m.Name, m.Sum[:], time.Now().UnixMicro()); err != nil {
			return fault.Wrap(fault.Internal, err, "migration "+m.Name+" cannot be recorded")
		}
	default:
		return fault.New(fault.Internal, "migration "+m.Name+" ran before the ones it follows were recorded")
	}
	if err := tx.Commit(); err != nil {
		return unavailable(err, "migration "+m.Name+" cannot be committed")
	}
	return nil
}

// Version returns the highest version recorded in db's table of applied
// migrations. A database without the table, which the first Migrate makes,
// is at version 0.
func Version(ctx context.Context, db *sql.DB, d Dialect) (int, error) {
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return 0, unavailable(err, "the schema version cannot be read")
	}
	defer func() { _ = tx.Rollback() }() // Read only: nothing to keep.
	var tables int
	if err := tx.QueryRowContext(ctx, d.HasTable).Scan(&tables); err != nil {
		return 0, unavailable(err, "the schema version cannot be read")
	}
	if tables == 0 {
		return 0, nil
	}
	rows, err := readApplied(ctx, tx, d)
	if err != nil {
		return 0, err
	}
	if err := check(rows, nil); err != nil {
		return 0, err
	}
	return len(rows), nil
}

// CheckApplied refuses db unless the migrations it records are a prefix of
// migrations, by version, name and checksum, as Migrate checks before it
// applies anything, without taking the migration lock or applying
// anything: a database made from another baseline (ErrEarlierBuild), one
// whose later file changed (ErrChecksum), one a newer binary upgraded
// (ErrNewerSchema), and one never migrated. What reads a database without
// migrating it first (a backup made with the server stopped) asks it, so
// it never copies rows of a schema this binary's files do not describe.
func CheckApplied(ctx context.Context, db *sql.DB, d Dialect, migrations []Migration) error {
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return unavailable(err, "the schema version cannot be read")
	}
	defer func() { _ = tx.Rollback() }() // Read only: nothing to keep.
	var tables int
	if err := tx.QueryRowContext(ctx, d.HasTable).Scan(&tables); err != nil {
		return unavailable(err, "the schema version cannot be read")
	}
	if tables == 0 {
		return fault.New(fault.Invalid, "the database records no migration: it was never made by this binary's migrations")
	}
	rows, err := readApplied(ctx, tx, d)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return fault.New(fault.Invalid, "the database records no migration: it was never made by this binary's migrations")
	}
	if len(migrations) == 0 {
		return fault.New(fault.Internal, "this binary has no migrations to check a database against")
	}
	return check(rows, migrations)
}

// readApplied reads the table of applied migrations.
func readApplied(ctx context.Context, tx *sql.Tx, d Dialect) ([]applied, error) {
	rs, err := tx.QueryContext(ctx, d.SelectApplied)
	if err != nil {
		return nil, unavailable(err, "the applied migrations cannot be read")
	}
	defer func() { _ = rs.Close() }() // Errors are reported by rs.Err.
	var out []applied
	for rs.Next() {
		var a applied
		if err := rs.Scan(&a.version, &a.name, &a.sum); err != nil {
			return nil, fault.Wrap(fault.Internal, err, "the applied migrations cannot be read")
		}
		out = append(out, a)
	}
	if err := rs.Err(); err != nil {
		return nil, unavailable(err, "the applied migrations cannot be read")
	}
	return out, nil
}

// check refuses a database whose applied migrations are not a prefix of
// migrations, by version, name and checksum. With migrations nil it checks
// only that the applied rows are numbered 1, 2, 3 with no gap.
//
// The first row is checked before any other and alone: a database whose
// baseline differs from this binary's is refused as made by an earlier
// build, whatever it records after it, so a database made by a chain of
// files the baseline has since replaced gets that one plain answer, not one
// about a later file.
func check(rows []applied, migrations []Migration) error {
	for i, a := range rows {
		if a.version != i+1 {
			return fault.New(fault.Internal, "the database records migration "+strconv.Itoa(a.version)+" out of sequence")
		}
		if migrations == nil {
			continue
		}
		if len(migrations) == 0 {
			// A binary with no files knows no schema: every recorded
			// migration is one a newer binary applied.
			return fault.Wrap(fault.Conflict, ErrNewerSchema, fmt.Sprintf(
				"the database is at schema version %d and this binary knows none", rows[len(rows)-1].version))
		}
		if i == 0 && (a.name != migrations[0].Name || !bytes.Equal(a.sum, migrations[0].Sum[:])) {
			return fault.Wrap(fault.Conflict, ErrEarlierBuild, fmt.Sprintf(
				"the database was made by an earlier development build (or is another kind of database): its schema began from %s (SHA-256 %x), and this binary's "+
					"begins from %s (SHA-256 %x); a database made from another baseline schema is never migrated: make it again",
				a.name, a.sum, migrations[0].Name, migrations[0].Sum[:]))
		}
		if i >= len(migrations) {
			return fault.Wrap(fault.Conflict, ErrNewerSchema, fmt.Sprintf(
				"the database is at schema version %d and this binary knows only up to %d; a newer binary has upgraded it",
				rows[len(rows)-1].version, len(migrations)))
		}
		m := migrations[i]
		if a.name != m.Name {
			return fault.Wrap(fault.Internal, ErrChecksum, fmt.Sprintf(
				"migration %d was applied as %s, and this binary has it as %s", m.Version, a.name, m.Name))
		}
		if !bytes.Equal(a.sum, m.Sum[:]) {
			return fault.Wrap(fault.Internal, ErrChecksum,
				"migration "+m.Name+" was changed after it was applied: its checksum differs, and an applied migration is never changed")
		}
	}
	return nil
}

// lockedExec runs one statement in a locked transaction.
func lockedExec(ctx context.Context, db *sql.DB, d Dialect, statement string) error {
	tx, err := d.BeginLocked(ctx, db)
	if err != nil {
		return err
	}
	// Deferred, so that a panic between Begin and Commit still ends the
	// transaction: otherwise it would hold the database's write lock, and
	// a pool's only write connection, for good. A no-op after Commit.
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, statement); err != nil {
		return err
	}
	return tx.Commit()
}

// unavailable wraps a failure to reach the database. A context that ended is
// kept in the chain, so errors.Is(err, context.Canceled) still answers.
func unavailable(err error, message string) error {
	return fault.Wrap(fault.Unavailable, err, message)
}
