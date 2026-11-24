package postgres

import (
	"context"
	"embed"

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

// Master is the master database on Postgres. It shares its pool handling,
// its failure classification and its write transactions with Tenant (base),
// and its settings and audit log with it (shared.go). It is safe for
// concurrent use.
type Master struct {
	base
}

var _ master.Store = (*Master)(nil)

// OpenMaster opens the master database at address, a postgres:// URL. It does
// not migrate: the caller calls Migrate, so that opening a database never
// changes it unasked. It does reach the server, so an address that cannot be
// used fails here, as Unavailable, with the address not repeated.
func OpenMaster(ctx context.Context, address string) (*Master, error) {
	if masterMigrationsErr != nil {
		return nil, fault.Wrap(fault.Internal, masterMigrationsErr, "the master's migrations cannot be read")
	}
	m := &Master{}
	if err := m.connect(ctx, address, "master database", "master", poolOptions{}); err != nil {
		return nil, err
	}
	return m, nil
}

// Migrate implements master.Store.
func (m *Master) Migrate(ctx context.Context) error {
	_, err := m.migrate(ctx, masterMigrations)
	return err
}

// Version implements master.Store.
func (m *Master) Version(ctx context.Context) (int, error) { return m.version(ctx) }

// Ping implements master.Store.
func (m *Master) Ping(ctx context.Context) error { return m.ping(ctx) }

// Close implements master.Store. It waits for the calls in progress to give
// their connections back, then closes every connection.
func (m *Master) Close() error { return m.shutdown() }
