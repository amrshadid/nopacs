package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/amrshadid/nopacs/internal/driver/sqlkit"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
)

// SetGrant implements master.Store. A key's grants are changed one writer at
// a time: the key's row is locked first (FOR NO KEY UPDATE, which the foreign
// key checks of other grants do not conflict with), so two concurrent sets on
// one key are ordered, and the one statement below, which reads the role
// there was and writes the new one, sees what the other left even under read
// committed. Without the lock two sets of a grant that does not exist yet
// would both read "none". The key's lock also answers whether it exists.
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
	err := m.write(ctx, "the grant cannot be set", func(tx pgx.Tx) error {
		if err := lockKeys(ctx, tx, by, g.KeyID); err != nil {
			return err
		}
		if err := checkActor(ctx, tx, by, tenantLock{id: g.Scope.TenantID, clause: lockKeyShare}); err != nil {
			return err
		}
		revoked, err := lockGrantTargets(ctx, tx, g.KeyID, g.Scope)
		if err != nil {
			return err
		}
		if revoked {
			// Decided under the key's row lock, which RevokeKey also takes,
			// so no grant is written after the key's revocation.
			return fault.New(fault.Conflict, "the key is revoked")
		}
		var old *string
		err = tx.QueryRow(ctx, `WITH previous AS (SELECT role FROM grants WHERE key_id = $1 AND scope = $2)
			INSERT INTO grants (key_id, scope, tenant_id, role) VALUES ($1, $2, $3, $4)
			ON CONFLICT (key_id, scope) DO UPDATE SET role = EXCLUDED.role
			RETURNING (SELECT role FROM previous)`,
			g.KeyID, g.Scope.TenantID, sqlkit.NullText(g.Scope.TenantID), string(g.Role)).Scan(&old)
		if isForeignKeyViolation(err) {
			// The tenant was removed between the check and the insert.
			return fault.Wrap(fault.NotFound, m.red.scrub(err), "no such tenant")
		}
		if err != nil {
			return err
		}
		if old != nil {
			previous = master.Role(*old)
		}
		// Judged after the upsert, inside its transaction under the key's
		// lock: a refusal rolls the upsert back.
		inst, err := installationRole(ctx, tx, g.KeyID)
		if err != nil {
			return err
		}
		if err := master.CheckGrantChange(by, g.KeyID, g.Scope, previous, g.Role, inst); err != nil {
			return err
		}
		return insertAudit(ctx, tx, m.red, audit.WithRoleChange(previous, g.Role))
	})
	if err != nil {
		return "", err
	}
	return previous, nil
}

// installationRole returns the role keyID holds on the installation, read
// in tx, empty for none: the installation's scope is the empty tenant id.
// The caller holds the key's row lock, which every grant change of the key
// takes, so the role cannot change before the transaction ends.
func installationRole(ctx context.Context, tx pgx.Tx, keyID string) (master.Role, error) {
	var role string
	err := tx.QueryRow(ctx, `SELECT role FROM grants WHERE key_id = $1 AND scope = $2`, keyID, master.Installation.TenantID).Scan(&role)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	return master.Role(role), nil
}

// lockGrantTargets returns NotFound unless the key, and the tenant of a
// tenant scope, exist, reports whether the key is revoked, and locks them
// against the changes that would undo that before the transaction ends. The
// key is locked FOR NO KEY UPDATE, so grant changes and a revocation on it are
// ordered; the tenant is locked for key share, so RemoveTenant waits for the
// grant being made.
func lockGrantTargets(ctx context.Context, tx pgx.Tx, keyID string, scope master.Scope) (revoked bool, err error) {
	var at *time.Time
	if err := tx.QueryRow(ctx, `SELECT revoked FROM api_keys WHERE id = $1 FOR NO KEY UPDATE`, keyID).Scan(&at); err != nil {
		return false, notFoundIn(err, "no such key")
	}
	if !scope.IsInstallation() {
		var one int
		if err := tx.QueryRow(ctx, `SELECT 1 FROM tenants WHERE id = $1 `+lockKeyShare, scope.TenantID).Scan(&one); err != nil {
			return false, notFoundIn(err, "no such tenant")
		}
	}
	return at != nil, nil
}

// grantColumns are a grant's columns with its tenant's name, joined.
const grantColumns = `g.key_id, g.scope, g.role, coalesce(t.name, '')`

// grantFrom joins each grant to its tenant, for the name.
const grantFrom = ` FROM grants g LEFT JOIN tenants t ON t.id = g.tenant_id`

// scanGrant reads one row of grantColumns.
func scanGrant(row scanner) (master.Grant, error) {
	var g master.Grant
	var role string
	err := row.Scan(&g.KeyID, &g.Scope.TenantID, &role, &g.TenantName)
	g.Role = master.Role(role)
	return g, err
}

// GrantsByKey implements master.Store: one read of the primary key's index,
// whose order (key, scope) puts the installation's empty scope first.
func (m *Master) GrantsByKey(ctx context.Context, keyID string, page master.Page) ([]master.Grant, string, error) {
	if err := master.ValidateID("key id", keyID); err != nil {
		return nil, "", err
	}
	if err := page.Validate(); err != nil {
		return nil, "", err
	}
	if err := m.open(); err != nil {
		return nil, "", err
	}
	var rows pgx.Rows
	var err error
	if page.Cursor == "" {
		// The first page has its own statement: the installation's scope
		// is the empty string, so no "scope > $2" includes it.
		rows, err = m.pool.Query(ctx, `SELECT `+grantColumns+grantFrom+` WHERE g.key_id = $1 ORDER BY g.key_id, g.scope LIMIT $2`, keyID, page.Limit+1)
	} else {
		f, derr := sqlkit.DecodeCursor(page.Cursor, listGrantsByKey, keyID, 1)
		if derr != nil {
			return nil, "", derr
		}
		rows, err = m.pool.Query(ctx, `SELECT `+grantColumns+grantFrom+` WHERE g.key_id = $1 AND g.scope > $2 ORDER BY g.key_id, g.scope LIMIT $3`, keyID, f[0], page.Limit+1)
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
	if err := m.open(); err != nil {
		return nil, "", err
	}
	// Key ids are never empty, so "after the empty id" is the first page.
	rows, err := m.pool.Query(ctx, `SELECT `+grantColumns+grantFrom+` WHERE g.scope = $1 AND g.key_id > $2 ORDER BY g.scope, g.key_id LIMIT $3`,
		scope.TenantID, after, page.Limit+1)
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
// that is not a valid tenant name is asked as the empty name, which no tenant
// has, so the statement runs all the same and finds no tenant.
func (m *Master) RolesFor(ctx context.Context, keyID, tenantName string) (master.Roles, error) {
	if err := master.ValidateID("key id", keyID); err != nil {
		return master.Roles{}, err
	}
	if master.ValidateTenantName(tenantName) != nil {
		tenantName = ""
	}
	if err := m.open(); err != nil {
		return master.Roles{}, err
	}
	var tenantID, inst, ten *string
	if err := m.pool.QueryRow(ctx, rolesForSQL, keyID, tenantName).Scan(&tenantID, &inst, &ten); err != nil {
		return master.Roles{}, m.fail(ctx, err, "the key's roles cannot be read")
	}
	r := master.Roles{TenantFound: tenantID != nil}
	if tenantID != nil {
		r.TenantID = *tenantID
	}
	if inst != nil {
		r.Installation = master.Role(*inst)
	}
	if ten != nil {
		r.Tenant = master.Role(*ten)
	}
	return r, nil
}

// rolesForSQL is RolesFor's statement, which always returns exactly one row:
// the tenant's id, NULL when no tenant has the name (the LEFT JOIN of a
// one-row table keeps the row), then the key's role on the installation and on
// that tenant, each NULL when there is no grant. The second grant lookup runs
// whether or not a tenant was found: without one it looks for the scope '*',
// which no id can be, so a missing tenant costs the same lookups as a tenant
// without a grant. Each is a point lookup: the tenants' unique index on name,
// and the grants' primary key at (key, the empty scope) and at (key, the
// tenant's id).
const rolesForSQL = `SELECT t.id,
	(SELECT role FROM grants WHERE key_id = $1 AND scope = ''),
	(SELECT role FROM grants WHERE key_id = $1 AND scope = coalesce(t.id, '*'))
	FROM (SELECT 1) AS one LEFT JOIN tenants AS t ON t.name = $2`

// RemoveGrant implements master.Store. The key's row is locked as SetGrant
// does, so the role it reports is the one the grant had when it was removed;
// one DELETE ... RETURNING then removes the grant and gives that role. A
// key or a tenant that does not exist is NotFound; a grant that does not
// exist, on a key and a tenant that do, is success.
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
	err := m.write(ctx, "the grant cannot be removed", func(tx pgx.Tx) error {
		if err := lockKeys(ctx, tx, by, keyID); err != nil {
			return err
		}
		if err := checkActor(ctx, tx, by, tenantLock{id: scope.TenantID, clause: lockKeyShare}); err != nil {
			return err
		}
		if _, err := lockGrantTargets(ctx, tx, keyID, scope); err != nil {
			return err
		}
		var old string
		err := tx.QueryRow(ctx, `DELETE FROM grants WHERE key_id = $1 AND scope = $2 RETURNING role`, keyID, scope.TenantID).Scan(&old)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		previous = master.Role(old)
		// Judged after the delete, inside its transaction: a refusal rolls
		// the delete back.
		inst, err := installationRole(ctx, tx, keyID)
		if err != nil {
			return err
		}
		if err := master.CheckGrantChange(by, keyID, scope, previous, "", inst); err != nil {
			return err
		}
		return insertAudit(ctx, tx, m.red, audit.WithRoleChange(previous, ""))
	})
	if err != nil {
		return "", err
	}
	return previous, nil
}
