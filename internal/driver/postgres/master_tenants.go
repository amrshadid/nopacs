package postgres

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/amrshadid/nopacs/internal/driver/sqlkit"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
)

// The names of the lists, as their cursors carry them, so a cursor from one
// list is refused by every other.
const (
	listTenants       = "tenants"
	listKeys          = "keys"
	listGrantsByKey   = "grants-by-key"
	listGrantsByScope = "grants-by-scope"
	listSettings      = "settings"
	listAudit         = "audit"
)

const tenantColumns = `id, name, driver, address, state, state_reason, created`

// scanner is what pgx.Row and pgx.CollectableRow share.
type scanner interface{ Scan(...any) error }

// scanTenant reads one row of tenantColumns.
func scanTenant(row scanner) (master.Tenant, error) {
	var t master.Tenant
	var state string
	var created time.Time
	err := row.Scan(&t.ID, &t.Name, &t.Driver, &t.SealedAddress, &state, &t.StateReason, &created)
	t.State = master.TenantState(state)
	t.Created = created.UTC()
	return t, err
}

// CreateTenant implements master.Store.
func (m *Master) CreateTenant(ctx context.Context, t master.Tenant, audit master.AuditEntry, by master.Actor) error {
	if err := t.Validate(); err != nil {
		return err
	}
	if err := audit.Validate(); err != nil {
		return err
	}
	if err := by.Validate(); err != nil {
		return err
	}
	return m.write(ctx, "the tenant cannot be created", func(tx pgx.Tx) error {
		if err := checkActor(ctx, tx, by, tenantLock{}); err != nil {
			return err
		}
		// No look-up first: the unique constraints decide, so two creations
		// racing in two processes cannot both succeed, and the constraint
		// that failed says whether the name or the id was taken.
		_, err := tx.Exec(ctx, `INSERT INTO tenants (`+tenantColumns+`) VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			t.ID, t.Name, t.Driver, t.SealedAddress, string(t.State), t.StateReason, micros(t.Created))
		switch {
		case constraintViolation(err, "tenants_name_key"):
			return fault.Wrap(fault.Conflict, m.red.scrub(err), "a tenant named "+t.Name+" exists")
		case isUniqueViolation(err):
			return fault.Wrap(fault.Conflict, m.red.scrub(err), "a tenant with this id exists")
		case err != nil:
			return err
		}
		return insertAudit(ctx, tx, m.red, audit)
	})
}

// Tenant implements master.Store.
func (m *Master) Tenant(ctx context.Context, id string) (master.Tenant, error) {
	if err := master.ValidateID("tenant id", id); err != nil {
		return master.Tenant{}, err
	}
	if err := m.open(); err != nil {
		return master.Tenant{}, err
	}
	t, err := scanTenant(m.pool.QueryRow(ctx, `SELECT `+tenantColumns+` FROM tenants WHERE id = $1`, id))
	return t, m.notFound(ctx, err, "no such tenant", "the tenant cannot be read")
}

// TenantByName implements master.Store: one read of the name's unique index.
func (m *Master) TenantByName(ctx context.Context, name string) (master.Tenant, error) {
	if err := master.ValidateTenantName(name); err != nil {
		return master.Tenant{}, err
	}
	if err := m.open(); err != nil {
		return master.Tenant{}, err
	}
	t, err := scanTenant(m.pool.QueryRow(ctx, `SELECT `+tenantColumns+` FROM tenants WHERE name = $1`, name))
	return t, m.notFound(ctx, err, "no such tenant", "the tenant cannot be read")
}

// ListTenants implements master.Store.
func (m *Master) ListTenants(ctx context.Context, page master.Page) ([]master.Tenant, string, error) {
	if err := page.Validate(); err != nil {
		return nil, "", err
	}
	after := ""
	if page.Cursor != "" {
		f, err := sqlkit.DecodeCursor(page.Cursor, listTenants, "", 1)
		if err != nil {
			return nil, "", err
		}
		after = f[0]
	}
	if err := m.open(); err != nil {
		return nil, "", err
	}
	// Names are never empty, so "after the empty name" is the first page.
	rows, err := m.pool.Query(ctx, `SELECT `+tenantColumns+` FROM tenants WHERE name > $1 ORDER BY name LIMIT $2`, after, page.Limit+1)
	out, err := collect(rows, err, scanTenant)
	if err != nil {
		return nil, "", m.fail(ctx, err, "the tenants cannot be listed")
	}
	out, last, more := sqlkit.Cut(out, page.Limit)
	if !more {
		return out, "", nil
	}
	return out, sqlkit.EncodeCursor(listTenants, "", last.Name), nil
}

// SetTenantState implements master.Store. The old state is read under a row
// lock and the new one written in one transaction.
func (m *Master) SetTenantState(ctx context.Context, id string, state master.TenantState, reason string, audit master.AuditEntry, by master.Actor) error {
	if err := master.ValidateID("tenant id", id); err != nil {
		return err
	}
	if err := master.ValidateTenantState(state, reason); err != nil {
		return err
	}
	if err := audit.ValidateWith(master.DetailOldState, master.DetailNewState); err != nil {
		return err
	}
	if err := by.Validate(); err != nil {
		return err
	}
	return m.write(ctx, "the tenant's state cannot be set", func(tx pgx.Tx) error {
		if err := checkActor(ctx, tx, by, tenantLock{id: id, clause: lockNoKeyUpdate}); err != nil {
			return err
		}
		// The row is locked as it is read, so the state in the audit entry
		// is the one this call replaced even when another call changes the
		// same tenant at once. The state is not a key, so the lock is FOR NO
		// KEY UPDATE, which an actor's check holding the tenant for key
		// share (checkActor) does not wait for: two such changes of one
		// tenant are ordered by this lock instead of each waiting for the
		// other's share.
		var old string
		if err := tx.QueryRow(ctx, `SELECT state FROM tenants WHERE id = $1 `+lockNoKeyUpdate, id).Scan(&old); err != nil {
			return notFoundIn(err, "no such tenant")
		}
		if err := master.CheckStateChange(master.TenantState(old), state, by); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE tenants SET state = $1, state_reason = $2 WHERE id = $3`, string(state), reason, id)
		if err := oneRow(tag, err, "no such tenant"); err != nil {
			return err
		}
		return insertAudit(ctx, tx, m.red, audit.WithStateChange(master.TenantState(old), state))
	})
}

// RemoveTenant implements master.Store.
func (m *Master) RemoveTenant(ctx context.Context, id string, audit master.AuditEntry, by master.Actor) error {
	if err := master.ValidateID("tenant id", id); err != nil {
		return err
	}
	if err := audit.Validate(); err != nil {
		return err
	}
	if err := by.Validate(); err != nil {
		return err
	}
	return m.write(ctx, "the tenant cannot be removed", func(tx pgx.Tx) error {
		if err := checkActor(ctx, tx, by, tenantLock{id: id, clause: lockUpdate}); err != nil {
			return err
		}
		// The tenant's row is locked first. A SetGrant on this tenant holds
		// a share lock on it until it commits, so this waits for that grant
		// to exist and then removes it with the others; one that starts
		// after this finds no tenant. Without the lock a grant added between
		// the two deletes would make the second fail on the foreign key.
		var one int
		if err := tx.QueryRow(ctx, `SELECT 1 FROM tenants WHERE id = $1 `+lockUpdate, id).Scan(&one); err != nil {
			return notFoundIn(err, "no such tenant")
		}
		if _, err := tx.Exec(ctx, `DELETE FROM grants WHERE scope = $1`, id); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `DELETE FROM tenants WHERE id = $1`, id)
		if err := oneRow(tag, err, "no such tenant"); err != nil {
			return err
		}
		return insertAudit(ctx, tx, m.red, audit)
	})
}

// tenantLock is the lock a change will take on one tenant's row: checkActor
// takes that lock, not its own weaker one, when the actor's role is on that
// tenant. The zero tenantLock is a change that locks no tenant.
type tenantLock struct {
	id     string
	clause string
}
