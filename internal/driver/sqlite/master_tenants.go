package sqlite

import (
	"context"
	"database/sql"
	"errors"

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
	return m.write(ctx, "the tenant cannot be created", func(tx *sql.Tx, s *statements) error {
		if err := checkActor(ctx, tx, s, by); err != nil {
			return err
		}
		// The name is looked up first to say which of name and id is
		// taken. Inside one transaction begun with BEGIN IMMEDIATE no other
		// writer, in this process or another, runs between the look-up and
		// the insert; the UNIQUE constraint holds the rule even so.
		var one int
		err := tx.StmtContext(ctx, s.tenantNameTaken).QueryRowContext(ctx, t.Name).Scan(&one)
		if err == nil {
			return fault.New(fault.Conflict, "a tenant named "+t.Name+" exists")
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		_, err = tx.StmtContext(ctx, s.insertTenant).ExecContext(ctx,
			t.ID, t.Name, t.Driver, t.SealedAddress, string(t.State), t.StateReason, micros(t.Created))
		if isUniqueViolation(err) {
			return fault.Wrap(fault.Conflict, err, "a tenant with this id or name exists")
		}
		if err != nil {
			return err
		}
		return insertAudit(ctx, tx, s, audit)
	})
}

const tenantColumns = `id, name, driver, address, state, state_reason, created`

// scanTenant reads one row of tenantColumns.
func scanTenant(row interface{ Scan(...any) error }) (master.Tenant, error) {
	var t master.Tenant
	var state string
	var created int64
	err := row.Scan(&t.ID, &t.Name, &t.Driver, &t.SealedAddress, &state, &t.StateReason, &created)
	t.State = master.TenantState(state)
	t.Created = fromMicros(created)
	return t, err
}

// Tenant implements master.Store.
func (m *Master) Tenant(ctx context.Context, id string) (master.Tenant, error) {
	if err := master.ValidateID("tenant id", id); err != nil {
		return master.Tenant{}, err
	}
	s, err := m.ready(ctx)
	if err != nil {
		return master.Tenant{}, err
	}
	t, err := scanTenant(s.tenantByID.QueryRowContext(ctx, id))
	return t, m.notFound(ctx, err, "no such tenant", "the tenant cannot be read")
}

// TenantByName implements master.Store.
func (m *Master) TenantByName(ctx context.Context, name string) (master.Tenant, error) {
	if err := master.ValidateTenantName(name); err != nil {
		return master.Tenant{}, err
	}
	s, err := m.ready(ctx)
	if err != nil {
		return master.Tenant{}, err
	}
	t, err := scanTenant(s.tenantByName.QueryRowContext(ctx, name))
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
	s, err := m.ready(ctx)
	if err != nil {
		return nil, "", err
	}
	// Names are never empty, so "after the empty name" is the first page.
	rows, err := s.listTenants.QueryContext(ctx, after, page.Limit+1)
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

// SetTenantState implements master.Store. The old state is read and the new
// one written in one write transaction, so the audit entry says what this
// call replaced even when another call changes the same tenant at once.
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
	return m.write(ctx, "the tenant's state cannot be set", func(tx *sql.Tx, s *statements) error {
		if err := checkActor(ctx, tx, s, by); err != nil {
			return err
		}
		var old string
		err := tx.StmtContext(ctx, s.tenantState).QueryRowContext(ctx, id).Scan(&old)
		if errors.Is(err, sql.ErrNoRows) {
			return fault.New(fault.NotFound, "no such tenant")
		}
		if err != nil {
			return err
		}
		if err := master.CheckStateChange(master.TenantState(old), state, by); err != nil {
			return err
		}
		res, err := tx.StmtContext(ctx, s.setTenantState).ExecContext(ctx, string(state), reason, id)
		if err := oneRow(res, err, "no such tenant"); err != nil {
			return err
		}
		return insertAudit(ctx, tx, s, audit.WithStateChange(master.TenantState(old), state))
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
	return m.write(ctx, "the tenant cannot be removed", func(tx *sql.Tx, s *statements) error {
		if err := checkActor(ctx, tx, s, by); err != nil {
			return err
		}
		// The grants go first, in the same transaction: the foreign key
		// refuses a tenant removed under its grants, so a forgotten step
		// fails loudly instead of leaving grants on nothing.
		if _, err := tx.StmtContext(ctx, s.deleteTenantGrants).ExecContext(ctx, id); err != nil {
			return err
		}
		res, err := tx.StmtContext(ctx, s.deleteTenant).ExecContext(ctx, id)
		if err := oneRow(res, err, "no such tenant"); err != nil {
			return err
		}
		return insertAudit(ctx, tx, s, audit)
	})
}
