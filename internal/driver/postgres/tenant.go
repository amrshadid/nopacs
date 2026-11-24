package postgres

import (
	"context"
	"embed"
	"time"

	"github.com/jackc/pgx/v5"

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

// DefaultTenantConnections is the size of a tenant's pool when its address
// does not give pool_max_conns. A tenant is one database that few requests
// reach at once, and a server may hold a thousand tenants: every connection is
// a process on the server and a socket here. Four is the two Migrate needs
// (the lock's connection and the runner's) and two for requests; an address
// that expects more says so (?pool_max_conns=16). An idle connection is let go
// after TenantIdleTime, so a tenant nobody uses holds one (the registry's
// check) and not four.
const DefaultTenantConnections = 4

// TenantIdleTime is how long a tenant's idle connection is kept when the
// address does not give pool_max_conn_idle_time.
const TenantIdleTime = time.Minute

// Tenant is one tenant's database on Postgres. It shares its pool handling,
// its failure classification and its write transactions with Master (base),
// and its settings and audit log with it (shared.go). It is safe for
// concurrent use.
type Tenant struct {
	base
	lists lists
	id    string
	// retried counts the pipeline's changes run again (Tenant.pipe), by
	// why, so a test can show a change that must never deadlock did not.
	retried retryCounts
	seams   tenantSeams
}

// tenantSeams are the points where a test changes what a Tenant does, all
// nil in a running server. They are set only through export_test.go.
type tenantSeams struct {
	// failAttempt, when set, is called after a pipeline change's function
	// returned nil and before its commit, with the attempt's number from 1;
	// an error it returns ends that attempt as if Postgres had raised it
	// there, so a test makes Tenant.pipe meet a deadlock or a serialization
	// failure on the attempt it chooses, after the change, its hook and its
	// guard ran.
	failAttempt func(attempt int) error
}

var _ tenant.Store = (*Tenant)(nil)

// OpenTenant opens the database of the tenant whose id is tenantID at address,
// a postgres:// URL, in the schema its search_path starts with. The id binds
// every cursor the store makes, so a page token from one tenant is refused by
// another.
//
// It reaches the server, so an address that cannot be used fails here, as
// Unavailable, with the address not repeated. It does not migrate: the
// caller (package registry) calls Migrate, so that opening a database never
// changes it unasked.
//
// A database already migrated for another tenant is refused as Invalid: the
// schema records the id of the tenant it was made for (tenant_identity), and
// that is compared here and again by Migrate, so two tenants cannot share one
// database and schema, and an address pasted for the wrong tenant is caught
// at once.
func OpenTenant(ctx context.Context, address, tenantID string) (*Tenant, error) {
	if err := master.ValidateID("tenant id", tenantID); err != nil {
		return nil, err
	}
	if tenantMigrationsErr != nil {
		return nil, fault.Wrap(fault.Internal, tenantMigrationsErr, "a tenant's migrations cannot be read")
	}
	t := &Tenant{lists: tenantLists, id: tenantID}
	t.lists.owner = tenantID
	if err := t.connect(ctx, address, "tenant database", "tenant", poolOptions{maxConns: DefaultTenantConnections, idle: TenantIdleTime}); err != nil {
		return nil, err
	}
	if err := t.checkIdentity(ctx); err != nil {
		_ = t.shutdown() // The refusal is the error reported.
		return nil, err
	}
	return t, nil
}

// errOtherTenant is the refusal of a database made for another tenant.
func errOtherTenant() error {
	return fault.New(fault.Invalid, "this database belongs to another tenant; give each tenant a database or a schema of its own")
}

// checkIdentity refuses the database when it records another tenant's id. A
// schema not yet migrated has no record and passes: Migrate writes it.
func (t *Tenant) checkIdentity(ctx context.Context) error {
	var recorded string
	err := t.pool.QueryRow(ctx, `SELECT id FROM tenant_identity WHERE only_row`).Scan(&recorded)
	switch {
	case err == nil:
		if recorded != t.id {
			return errOtherTenant()
		}
		return nil
	case isNoTable(err), isNoRow(err):
		return nil
	}
	return t.fail(ctx, err, "the tenant database cannot be checked")
}

// claim records this tenant's id in the schema when it has none, and refuses
// the schema when it holds another's. The insert does nothing when a row is
// there (two instances claiming at once: one inserts, the other waits for it
// and then reads it), so the one row is never replaced.
func (t *Tenant) claim(ctx context.Context) error {
	return t.write(ctx, "the tenant database cannot be claimed", func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO tenant_identity (id) VALUES ($1) ON CONFLICT (only_row) DO NOTHING`, t.id); err != nil {
			return err
		}
		var recorded string
		if err := tx.QueryRow(ctx, `SELECT id FROM tenant_identity WHERE only_row`).Scan(&recorded); err != nil {
			return err
		}
		if recorded != t.id {
			return errOtherTenant()
		}
		return nil
	})
}

// Migrate implements tenant.Store: the migrations, then the claim of the
// schema for this tenant.
func (t *Tenant) Migrate(ctx context.Context) error {
	if _, err := t.migrate(ctx, tenantMigrations); err != nil {
		return err
	}
	return t.claim(ctx)
}

// Version implements tenant.Store.
func (t *Tenant) Version(ctx context.Context) (int, error) { return t.version(ctx) }

// Ping implements tenant.Store.
func (t *Tenant) Ping(ctx context.Context) error { return t.ping(ctx) }

// Close implements tenant.Store.
func (t *Tenant) Close() error { return t.shutdown() }

// Setting implements tenant.Store.
func (t *Tenant) Setting(ctx context.Context, name string) (string, error) {
	return getSetting(ctx, &t.base, t.lists, name)
}

// SetSetting implements tenant.Store. The guard runs last in the
// transaction, before the commit.
func (t *Tenant) SetSetting(ctx context.Context, name, value string, audit master.AuditEntry, guard tenant.Guard) error {
	return setSetting(ctx, &t.base, name, value, audit, guarded(guard))
}

// UnsetSetting implements tenant.Store. The guard runs last in the
// transaction, before the commit.
func (t *Tenant) UnsetSetting(ctx context.Context, name string, audit master.AuditEntry, guard tenant.Guard) (bool, error) {
	return unsetSetting(ctx, &t.base, name, audit, guarded(guard))
}

// guarded returns a change's Guard as the last step of its transaction, or
// nil for none.
func guarded(guard tenant.Guard) func(context.Context, pgx.Tx) error {
	if guard == nil {
		return nil
	}
	return func(ctx context.Context, _ pgx.Tx) error { return guard(ctx) }
}

// ListSettings implements tenant.Store.
func (t *Tenant) ListSettings(ctx context.Context, page master.Page) ([]master.Setting, string, error) {
	return listSettingsPage(ctx, &t.base, t.lists, page)
}

// AppendAudit implements tenant.Store.
func (t *Tenant) AppendAudit(ctx context.Context, entries ...master.AuditEntry) error {
	return appendAudit(ctx, &t.base, entries)
}

// ListAudit implements tenant.Store. The cursor is bound to the whole filter
// and to the tenant's id.
func (t *Tenant) ListAudit(ctx context.Context, filter master.AuditFilter, page master.Page) ([]master.AuditEntry, string, error) {
	return listAuditPage(ctx, &t.base, t.lists, filter, page)
}
