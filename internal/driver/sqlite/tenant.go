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
	"github.com/amrshadid/nopacs/internal/tenant"
)

// tenantFiles holds a tenant database's migrations, embedded so the binary
// upgrades every tenant's database with no file beside it.
//
//go:embed migrations/tenant/*.sql
var tenantFiles embed.FS

// tenantMigrations are a tenant's migrations, read once. A broken embedded
// directory is a build mistake that every test of this package shows.
var tenantMigrations, tenantMigrationsErr = sqlkit.Load(tenantFiles, "migrations/tenant")

// tenantLists names a tenant's settings and audit lists in their cursors,
// apart from the master's, so a cursor from the master's audit log is
// refused by a tenant's and the other way round. Each Tenant binds them to
// its own id as well (lists.owner).
var tenantLists = lists{settings: "tenant-settings", audit: "tenant-audit", missing: "the tenant has no override of this setting"}

// Tenant is one tenant's database on a SQLite file. It reaches the file as
// the master does (two pools, one writer with BEGIN IMMEDIATE, write-ahead
// mode, full sync, a file private to its owner), shares the master's code
// for settings and the audit log (shared.go), and classifies failures the
// same way (base). It is safe for concurrent use.
type Tenant struct {
	base
	lists lists

	// The prepared statements, made on first use after the schema exists,
	// since a statement cannot be prepared against tables not yet made.
	mu    sync.Mutex
	stmts atomic.Pointer[tenantStatements]

	seams tenantSeams

	// path is the database file's path, beside which a rebuild's shadow
	// is made (tenant_rebuild.go); shadowMu serializes the shadow's
	// attaching and detaching on the write connection.
	path     string
	shadowMu sync.Mutex
}

// tenantSeams are the points where a test changes what a Tenant does, all
// nil in a running server. They are set only through export_test.go.
type tenantSeams struct {
	// betweenReads, when set, runs inside the read transaction of
	// ReadEvents and StatusSnapshot between their reads, so a test can
	// commit a change there and show that the answer is of one snapshot.
	betweenReads func()
}

var _ tenant.Store = (*Tenant)(nil)

// OpenTenant opens the database of the tenant whose id is tenantID in the
// SQLite file at path, creating
// the file, readable by its owner only, when it does not exist. On Unix an
// existing file, its -wal and -shm files, or its directory that someone other
// than the owner can read or write is refused as Invalid, as the master's
// are. The id binds every cursor the store makes, so a page token from one
// tenant is refused by another. It does not migrate: the caller (package
// registry) calls Migrate, so that opening a database never changes it
// unasked.
func OpenTenant(ctx context.Context, path, tenantID string) (*Tenant, error) {
	if err := master.ValidateID("tenant id", tenantID); err != nil {
		return nil, err
	}
	if tenantMigrationsErr != nil {
		return nil, fault.Wrap(fault.Internal, tenantMigrationsErr, "a tenant's migrations cannot be read")
	}
	p, err := openPools(ctx, path, tenantDB)
	if err != nil {
		return nil, err
	}
	t := &Tenant{lists: tenantLists, path: path}
	t.lists.owner = tenantID
	t.p, t.name = p, tenantDB
	return t, nil
}

// Migrate implements tenant.Store.
func (t *Tenant) Migrate(ctx context.Context) error {
	if err := t.open(); err != nil {
		return err
	}
	_, err := sqlkit.Migrate(ctx, t.p.write, dialect, tenantMigrations)
	return t.fail(ctx, err, "the tenant database cannot be migrated")
}

// Version implements tenant.Store.
func (t *Tenant) Version(ctx context.Context) (int, error) {
	if err := t.open(); err != nil {
		return 0, err
	}
	v, err := sqlkit.Version(ctx, t.p.read, dialect)
	return v, t.fail(ctx, err, "the tenant's schema version cannot be read")
}

// Ping implements tenant.Store. It reaches both pools, since either failing
// fails requests.
func (t *Tenant) Ping(ctx context.Context) error { return t.ping(ctx) }

// Close implements tenant.Store.
func (t *Tenant) Close() error {
	return t.shutdown(func() {
		if s := t.stmts.Load(); s != nil {
			closeAll(s.all)
		}
	})
}

// tenantStatements are a tenant's prepared statements: the ones it shares
// with the master, and the pipeline's (pipeline.go).
type tenantStatements struct {
	all []*sql.Stmt
	sharedStatements
	pipelineStatements
}

// ready returns the prepared statements, preparing them on first use.
func (t *Tenant) ready(ctx context.Context) (*tenantStatements, error) {
	if err := t.open(); err != nil {
		return nil, err
	}
	if s := t.stmts.Load(); s != nil {
		return s, nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if s := t.stmts.Load(); s != nil {
		return s, nil
	}
	s := &tenantStatements{}
	all, err := prepareAll(ctx, append(sharedEntries(&s.sharedStatements, t.p), pipelineEntries(&s.pipelineStatements, t.p)...))
	if err != nil {
		return nil, t.notReady(ctx, err)
	}
	s.all = all
	t.stmts.Store(s)
	return s, nil
}

// b returns the tenant's base, for the shared operations.
func (t *Tenant) b() *base { return &t.base }

// shared returns the tenant's prepared settings and audit statements.
func (t *Tenant) shared(ctx context.Context) (*sharedStatements, error) {
	s, err := t.ready(ctx)
	if err != nil {
		return nil, err
	}
	return &s.sharedStatements, nil
}

// writeShared runs fn in one write transaction (base.writeTx) with the
// shared statements; a change and its audit entry are written both or
// neither through here. fn may run more than once and must only assign its
// results, as Master.write says.
func (t *Tenant) writeShared(ctx context.Context, message string, fn func(tx *sql.Tx, s *sharedStatements) error) error {
	s, err := t.shared(ctx)
	if err != nil {
		return err
	}
	return t.writeTx(ctx, message, func(tx *sql.Tx) error { return fn(tx, s) })
}

// Setting implements tenant.Store.
func (t *Tenant) Setting(ctx context.Context, name string) (string, error) {
	return getSetting(ctx, t, t.lists, name)
}

// SetSetting implements tenant.Store. The guard runs last in the
// transaction, before the commit.
func (t *Tenant) SetSetting(ctx context.Context, name, value string, audit master.AuditEntry, guard tenant.Guard) error {
	return setSetting(ctx, t, name, value, audit, guarded(guard))
}

// UnsetSetting implements tenant.Store. The guard runs last in the
// transaction, before the commit.
func (t *Tenant) UnsetSetting(ctx context.Context, name string, audit master.AuditEntry, guard tenant.Guard) (bool, error) {
	return unsetSetting(ctx, t, name, audit, guarded(guard))
}

// guarded returns a change's Guard as the last step of its transaction, or
// nil for none.
func guarded(guard tenant.Guard) func(context.Context, *sql.Tx) error {
	if guard == nil {
		return nil
	}
	return func(ctx context.Context, _ *sql.Tx) error { return guard(ctx) }
}

// ListSettings implements tenant.Store.
func (t *Tenant) ListSettings(ctx context.Context, page master.Page) ([]master.Setting, string, error) {
	return listSettingsPage(ctx, t, t.lists, page)
}

// AppendAudit implements tenant.Store.
func (t *Tenant) AppendAudit(ctx context.Context, entries ...master.AuditEntry) error {
	return appendAudit(ctx, t, entries)
}

// ListAudit implements tenant.Store. The cursor is bound to the whole filter.
func (t *Tenant) ListAudit(ctx context.Context, filter master.AuditFilter, page master.Page) ([]master.AuditEntry, string, error) {
	return listAuditPage(ctx, t, t.lists, filter, page)
}
