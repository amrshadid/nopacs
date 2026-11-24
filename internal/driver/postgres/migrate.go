package postgres

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"time"

	"github.com/jackc/pgx/v5/stdlib"

	"github.com/amrshadid/nopacs/internal/driver/sqlkit"
	"github.com/amrshadid/nopacs/internal/fault"
)

// lockName is the name the migration advisory lock is made from: the
// schema's name is added in the database (current_schema()), because an
// advisory lock belongs to the whole database, and two installations in two
// schemas of one database must not wait for each other. hashtextextended
// turns the text into the bigint the lock functions take.
const lockName = `hashtextextended('nopacs/master-migration/' || current_schema(), 0)`

// unlockTimeout bounds how long the release of the migration lock waits.
const unlockTimeout = 10 * time.Second

// dialect is how the migration runner works on Postgres. BeginLocked only
// begins a transaction: the lock that makes it exclusive is the session
// advisory lock Migrate holds for the whole run (withMigrationLock), so every
// process that runs the migrations is already alone when it gets here.
//
// The applied column is the time the runner gives in microseconds since 1970;
// the interval arithmetic turns it into a timestamptz with no rounding.
var dialect = sqlkit.Dialect{
	HasTable: `SELECT count(*) FROM information_schema.tables WHERE table_schema = current_schema() AND table_name = 'schema_migrations'`,
	CreateTable: `CREATE TABLE IF NOT EXISTS schema_migrations (
		version  integer     NOT NULL PRIMARY KEY,
		name     text        NOT NULL,
		checksum bytea       NOT NULL,
		applied  timestamptz NOT NULL)`,
	SelectApplied: `SELECT version, name, checksum FROM schema_migrations ORDER BY version`,
	InsertApplied: `INSERT INTO schema_migrations (version, name, checksum, applied)
		VALUES ($1, $2, $3, timestamptz 'epoch' + $4::bigint * interval '1 microsecond')`,
	BeginLocked: func(ctx context.Context, db *sql.DB) (*sql.Tx, error) {
		return db.BeginTx(ctx, nil)
	},
}

// withMigrationLock runs fn while this process holds the migration lock: a
// session advisory lock taken on one dedicated connection, so a second
// process (or a second Migrate in this one) waits in pg_advisory_lock until
// the first has finished, however many statements and transactions fn uses.
// A session lock, not a transaction lock, because the runner uses a
// transaction for each file and the lock must cover the whole run.
//
// The lock is released on every path: when fn returns, when it fails, and
// when the context ends. It is released with pg_advisory_unlock on the
// connection that took it, under a context of its own so a canceled request
// still lets go. If the release cannot be confirmed, or the lock may have
// been granted while a canceled request was waiting for it, the connection
// is closed, which ends the session and with it every lock it holds, instead
// of going back to the pool with one.
func (b *base) withMigrationLock(ctx context.Context, fn func() error) (err error) {
	conn, err := b.mig.Conn(ctx)
	if err != nil {
		return fault.Wrap(fault.Unavailable, b.red.scrub(err), "the migration lock cannot be taken")
	}
	// Whatever happens, the connection is either unlocked and given back, or
	// destroyed.
	locked := false
	defer func() {
		if !locked {
			destroy(conn)
			return
		}
		uctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), unlockTimeout)
		defer cancel()
		var released bool
		if uerr := conn.QueryRowContext(uctx, `SELECT pg_advisory_unlock(`+lockName+`)`).Scan(&released); uerr != nil || !released {
			destroy(conn)
			return
		}
		if cerr := conn.Close(); cerr != nil && err == nil {
			err = fault.Wrap(fault.Internal, b.red.scrub(cerr), "the migration lock's connection did not close cleanly")
		}
	}()
	if _, lerr := conn.ExecContext(ctx, `SELECT pg_advisory_lock(`+lockName+`)`); lerr != nil {
		// The lock may have been granted just as the request ended, with no
		// way to see it; locked stays false so the connection is destroyed.
		return fault.Wrap(fault.Unavailable, b.red.scrub(lerr), "the migration lock cannot be taken")
	}
	locked = true
	return fn()
}

// destroy closes the connection for good: the underlying pgx connection is
// closed, which ends its session on the server, and database/sql is told the
// connection is bad so it is not reused.
func destroy(conn *sql.Conn) {
	_ = conn.Raw(func(dc any) error {
		if sc, ok := dc.(*stdlib.Conn); ok {
			ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), unlockTimeout)
			defer cancel()
			_ = sc.Conn().Close(ctx) // Closing a connection that is already broken has nothing more to report.
		}
		return driver.ErrBadConn
	})
	_ = conn.Close() // Reports ErrConnDone once Raw discarded it.
}

// migrate applies migrations under the migration lock and returns the schema
// version the database is then at. A store's Migrate passes its own files; a
// test passes others, so the lock and the runner are tried with files that
// fail when applied twice.
func (b *base) migrate(ctx context.Context, migrations []sqlkit.Migration) (int, error) {
	if err := b.open(); err != nil {
		return 0, err
	}
	var version int
	err := b.withMigrationLock(ctx, func() error {
		var err error
		version, err = sqlkit.Migrate(ctx, b.mig, dialect, migrations)
		return err
	})
	return version, b.runnerErr(ctx, err, "the "+b.name+" cannot be migrated")
}

// version returns the schema version the database is at.
func (b *base) version(ctx context.Context) (int, error) {
	if err := b.open(); err != nil {
		return 0, err
	}
	v, err := sqlkit.Version(ctx, b.mig, dialect)
	return v, b.runnerErr(ctx, err, "the "+b.short+"'s schema version cannot be read")
}
