package sqlite

import (
	"context"
	"database/sql"
	"errors"

	"github.com/amrshadid/nopacs/internal/driver/sqlkit"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
)

// SetGrant implements master.Store. The key's revocation and the old role are
// read and the new role written in one write transaction, and SQLite has one
// writer at a time, so a set racing a revocation or another set is ordered
// with it: no grant is written after the key is revoked, and each set reads
// what the others left.
func (m *Master) SetGrant(ctx context.Context, g master.Grant, audit master.AuditEntry, by master.Actor) (master.Role, error) {
	if err := g.Validate(); err != nil {
		return "", err
	}
	if err := audit.ValidateRoleChange(); err != nil {
		return "", err
	}
	if err := by.Validate(); err != nil {
		return "", err
	}
	var previous master.Role
	err := m.write(ctx, "the grant cannot be set", func(tx *sql.Tx, s *statements) error {
		if err := checkActor(ctx, tx, s, by); err != nil {
			return err
		}
		revoked, err := m.grantTargetsExist(ctx, tx, s, g.KeyID, g.Scope)
		if err != nil {
			return err
		}
		if revoked {
			return fault.New(fault.Conflict, "the key is revoked")
		}
		var old string
		err = tx.StmtContext(ctx, s.grantRole).QueryRowContext(ctx, g.KeyID, g.Scope.TenantID).Scan(&old)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		previous = master.Role(old)
		if _, err := tx.StmtContext(ctx, s.upsertGrant).ExecContext(ctx,
			g.KeyID, g.Scope.TenantID, sqlkit.NullText(g.Scope.TenantID), string(g.Role)); err != nil {
			return err
		}
		return insertAudit(ctx, tx, s, audit.WithRoleChange(previous, g.Role))
	})
	if err != nil {
		return "", err
	}
	return previous, nil
}

// grantTargetsExist returns NotFound unless the key, and the tenant of a
// tenant scope, exist, and reports whether the key is revoked. The foreign
// keys would refuse a missing one too, but could not say which.
func (m *Master) grantTargetsExist(ctx context.Context, tx *sql.Tx, s *statements, keyID string, scope master.Scope) (revoked bool, err error) {
	var at sql.NullInt64
	err = tx.StmtContext(ctx, s.keyRevoked).QueryRowContext(ctx, keyID).Scan(&at)
	if errors.Is(err, sql.ErrNoRows) {
		return false, fault.New(fault.NotFound, "no such key")
	}
	if err != nil {
		return false, err
	}
	if scope.IsInstallation() {
		return at.Valid, nil
	}
	var one int
	err = tx.StmtContext(ctx, s.tenantExists).QueryRowContext(ctx, scope.TenantID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, fault.New(fault.NotFound, "no such tenant")
	}
	return at.Valid, err
}

// grantColumns are a grant's columns with its tenant's name, joined.
const grantColumns = `g.key_id, g.scope, g.role, coalesce(t.name, '')`

// grantFrom joins each grant to its tenant, for the name.
const grantFrom = ` FROM grants g LEFT JOIN tenants t ON t.id = g.tenant_id`

// scanGrant reads one row of grantColumns.
func scanGrant(row interface{ Scan(...any) error }) (master.Grant, error) {
	var g master.Grant
	var role string
	err := row.Scan(&g.KeyID, &g.Scope.TenantID, &role, &g.TenantName)
	g.Role = master.Role(role)
	return g, err
}

// GrantsByKey implements master.Store: a range of the primary key's index,
// whose order (key, scope) puts the installation's empty scope first, with
// one lookup of the tenants' primary key per grant for the name. The cursor
// is bound to the key.
func (m *Master) GrantsByKey(ctx context.Context, keyID string, page master.Page) ([]master.Grant, string, error) {
	if err := master.ValidateID("key id", keyID); err != nil {
		return nil, "", err
	}
	if err := page.Validate(); err != nil {
		return nil, "", err
	}
	var after []string
	if page.Cursor != "" {
		f, err := sqlkit.DecodeCursor(page.Cursor, listGrantsByKey, keyID, 1)
		if err != nil {
			return nil, "", err
		}
		after = f
	}
	s, err := m.ready(ctx)
	if err != nil {
		return nil, "", err
	}
	var rows *sql.Rows
	if after == nil {
		// The first page has its own statement: the installation's scope
		// is the empty string, so no "scope > ?" includes it.
		rows, err = s.grantsByKeyFirst.QueryContext(ctx, keyID, page.Limit+1)
	} else {
		rows, err = s.grantsByKeyNext.QueryContext(ctx, keyID, after[0], page.Limit+1)
	}
	out, err := collect(rows, err, scanGrant)
	if err != nil {
		return nil, "", m.fail(ctx, err, "the key's grants cannot be listed")
	}
	out, last, more := sqlkit.Cut(out, page.Limit)
	if !more {
		return out, "", nil
	}
	return out, sqlkit.EncodeCursor(listGrantsByKey, keyID, last.Scope.TenantID), nil
}

// scopeBinding is what a GrantsByScope cursor is bound to. The installation
// and a tenant can never give the same text, since a tenant id is not empty.
func scopeBinding(scope master.Scope) string {
	return "scope:" + scope.TenantID
}

// GrantsByScope implements master.Store. The cursor is bound to the scope.
func (m *Master) GrantsByScope(ctx context.Context, scope master.Scope, page master.Page) ([]master.Grant, string, error) {
	if err := scope.Validate(); err != nil {
		return nil, "", err
	}
	if err := page.Validate(); err != nil {
		return nil, "", err
	}
	after := ""
	if page.Cursor != "" {
		f, err := sqlkit.DecodeCursor(page.Cursor, listGrantsByScope, scopeBinding(scope), 1)
		if err != nil {
			return nil, "", err
		}
		after = f[0]
	}
	s, err := m.ready(ctx)
	if err != nil {
		return nil, "", err
	}
	// Key ids are never empty, so "after the empty id" is the first page.
	rows, err := s.grantsByScope.QueryContext(ctx, scope.TenantID, after, page.Limit+1)
	out, err := collect(rows, err, scanGrant)
	if err != nil {
		return nil, "", m.fail(ctx, err, "the grants cannot be listed")
	}
	out, last, more := sqlkit.Cut(out, page.Limit)
	if !more {
		return out, "", nil
	}
	return out, sqlkit.EncodeCursor(listGrantsByScope, scopeBinding(scope), last.KeyID), nil
}

// RolesFor implements master.Store with one statement (rolesForSQL). A name
// that is not a valid tenant name is asked as the empty name, which no
// tenant has, so the statement runs all the same and finds no tenant.
func (m *Master) RolesFor(ctx context.Context, keyID, tenantName string) (master.Roles, error) {
	if err := master.ValidateID("key id", keyID); err != nil {
		return master.Roles{}, err
	}
	if master.ValidateTenantName(tenantName) != nil {
		tenantName = ""
	}
	s, err := m.ready(ctx)
	if err != nil {
		return master.Roles{}, err
	}
	var tenantID, inst, ten sql.NullString
	err = s.rolesFor.QueryRowContext(ctx, keyID, tenantName).Scan(&tenantID, &inst, &ten)
	if err != nil {
		return master.Roles{}, m.fail(ctx, err, "the key's roles cannot be read")
	}
	return master.Roles{
		Installation: master.Role(inst.String),
		Tenant:       master.Role(ten.String),
		TenantID:     tenantID.String,
		TenantFound:  tenantID.Valid,
	}, nil
}

// RemoveGrant implements master.Store. One DELETE ... RETURNING removes the
// grant and gives the role it had; only when it removed nothing are the key
// and tenant looked up, to tell "no grant" (success) from NotFound.
func (m *Master) RemoveGrant(ctx context.Context, keyID string, scope master.Scope, audit master.AuditEntry, by master.Actor) (master.Role, error) {
	if err := master.ValidateID("key id", keyID); err != nil {
		return "", err
	}
	if err := scope.Validate(); err != nil {
		return "", err
	}
	if err := audit.ValidateRoleChange(); err != nil {
		return "", err
	}
	if err := by.Validate(); err != nil {
		return "", err
	}
	var previous master.Role
	err := m.write(ctx, "the grant cannot be removed", func(tx *sql.Tx, s *statements) error {
		if err := checkActor(ctx, tx, s, by); err != nil {
			return err
		}
		var old string
		err := tx.StmtContext(ctx, s.deleteGrant).QueryRowContext(ctx, keyID, scope.TenantID).Scan(&old)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			if _, err := m.grantTargetsExist(ctx, tx, s, keyID, scope); err != nil {
				return err
			}
		case err != nil:
			return err
		}
		previous = master.Role(old)
		return insertAudit(ctx, tx, s, audit.WithRoleChange(previous, ""))
	})
	if err != nil {
		return "", err
	}
	return previous, nil
}
