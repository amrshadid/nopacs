package sqlite

import (
	"context"
	"database/sql"
	"embed"
	"sync"
	"sync/atomic"

	"github.com/amrshadid/nopacs/internal/driver/sqlkit"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
)

// masterFiles holds the master database's migrations, embedded so the binary
// upgrades its own database with no file beside it.
//
//go:embed migrations/master/*.sql
var masterFiles embed.FS

// masterMigrations are the master's migrations, read once. A broken embedded
// directory is a build mistake that every test of this package shows.
var masterMigrations, masterMigrationsErr = sqlkit.Load(masterFiles, "migrations/master")

// Master is the master database on a SQLite file. It is safe for concurrent
// use.
type Master struct {
	base

	// The prepared statements, made on first use after the schema exists,
	// since a statement cannot be prepared against tables not yet made.
	mu    sync.Mutex
	stmts atomic.Pointer[statements]
}

var _ master.Store = (*Master)(nil)

// OpenMaster opens the master database in the SQLite file at path, creating
// the file, readable by its owner only, when it does not exist. On Unix an
// existing file, its -wal and -shm files, or its directory that someone other
// than the owner can read or write is refused (see checkPrivate). It does
// not migrate: the caller calls Migrate, so that opening a database never
// changes it unasked.
func OpenMaster(ctx context.Context, path string) (*Master, error) {
	if masterMigrationsErr != nil {
		return nil, fault.Wrap(fault.Internal, masterMigrationsErr, "the master's migrations cannot be read")
	}
	p, err := openPools(ctx, path, masterDB)
	if err != nil {
		return nil, err
	}
	m := &Master{}
	m.p, m.name = p, masterDB
	return m, nil
}

// Migrate implements master.Store.
func (m *Master) Migrate(ctx context.Context) error {
	if err := m.open(); err != nil {
		return err
	}
	_, err := sqlkit.Migrate(ctx, m.p.write, dialect, masterMigrations)
	return m.fail(ctx, err, "the master database cannot be migrated")
}

// Version implements master.Store.
func (m *Master) Version(ctx context.Context) (int, error) {
	if err := m.open(); err != nil {
		return 0, err
	}
	v, err := sqlkit.Version(ctx, m.p.read, dialect)
	return v, m.fail(ctx, err, "the master's schema version cannot be read")
}

// Ping implements master.Store. It reaches both pools, since either failing
// fails requests.
func (m *Master) Ping(ctx context.Context) error { return m.ping(ctx) }

// Close implements master.Store.
func (m *Master) Close() error {
	return m.shutdown(func() {
		if s := m.stmts.Load(); s != nil {
			s.close()
		}
	})
}

// ready returns the prepared statements, preparing them on first use.
func (m *Master) ready(ctx context.Context) (*statements, error) {
	if err := m.open(); err != nil {
		return nil, err
	}
	if s := m.stmts.Load(); s != nil {
		return s, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if s := m.stmts.Load(); s != nil {
		return s, nil
	}
	s, err := prepare(ctx, m.p)
	if err != nil {
		return nil, m.notReady(ctx, err)
	}
	m.stmts.Store(s)
	return s, nil
}

// write runs fn in one write transaction (base.writeTx) with the prepared
// statements. Every changing operation and its audit entry go through here
// together, which is what makes them both or neither.
//
// fn may run more than once, each time in a new transaction after the last
// was undone whole (base.writeTx), and fn's caller sees only the last run.
// So fn must only read, write and assign its results to variables it sets on
// every run; it must never append to, count into or send from anything that
// outlives the run, or a result of an undone run would leak into the answer.
// The same holds for a tenant's writeShared.
func (m *Master) write(ctx context.Context, message string, fn func(tx *sql.Tx, s *statements) error) error {
	s, err := m.ready(ctx)
	if err != nil {
		return err
	}
	return m.writeTx(ctx, message, func(tx *sql.Tx) error { return fn(tx, s) })
}

// b, shared and writeShared give the operations the master shares with a
// tenant's database (shared.go) what they need.
func (m *Master) b() *base { return &m.base }

// shared returns the master's prepared settings and audit statements.
func (m *Master) shared(ctx context.Context) (*sharedStatements, error) {
	s, err := m.ready(ctx)
	if err != nil {
		return nil, err
	}
	return &s.sharedStatements, nil
}

// writeShared runs fn in a write transaction with those statements.
func (m *Master) writeShared(ctx context.Context, message string, fn func(tx *sql.Tx, s *sharedStatements) error) error {
	return m.write(ctx, message, func(tx *sql.Tx, s *statements) error { return fn(tx, &s.sharedStatements) })
}
