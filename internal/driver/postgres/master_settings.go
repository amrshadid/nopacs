package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/amrshadid/nopacs/internal/master"
)

// Setting implements master.Store.
func (m *Master) Setting(ctx context.Context, name string) (string, error) {
	return getSetting(ctx, &m.base, masterLists, name)
}

// SetSetting implements master.Store.
func (m *Master) SetSetting(ctx context.Context, name, value string, audit master.AuditEntry, by master.Actor) error {
	if err := by.Validate(); err != nil {
		return err
	}
	return setSetting(ctx, &m.base, name, value, audit, actorCheck(by))
}

// UnsetSetting implements master.Store.
func (m *Master) UnsetSetting(ctx context.Context, name string, audit master.AuditEntry, by master.Actor) (bool, error) {
	if err := by.Validate(); err != nil {
		return false, err
	}
	return unsetSetting(ctx, &m.base, name, audit, actorCheck(by))
}

// ListSettings implements master.Store.
func (m *Master) ListSettings(ctx context.Context, page master.Page) ([]master.Setting, string, error) {
	return listSettingsPage(ctx, &m.base, masterLists, page)
}

// actorSQL is the check of a change's actor (master.Actor): the key's revoked
// time and expiry, and its role on the actor's scope, NULL when it holds no
// grant there; no row when the key does not exist. Two point lookups: the
// keys' primary key and the grants' primary key at (key, scope).
const actorSQL = `SELECT k.revoked, k.expires,
	(SELECT role FROM grants WHERE key_id = $1 AND scope = $2)
	FROM api_keys AS k WHERE k.id = $1`

// CheckActor implements master.Store with actorSQL, locking nothing. An
// actor that is not a key reads nothing.
func (m *Master) CheckActor(ctx context.Context, by master.Actor) error {
	if err := by.Validate(); err != nil {
		return err
	}
	if err := m.open(); err != nil {
		return err
	}
	if !by.IsKey() {
		return nil
	}
	found, revoked, expires, role, err := readActor(ctx, m.pool, actorSQL, by)
	if err != nil {
		return m.fail(ctx, err, "the acting key cannot be read")
	}
	return by.Judge(found, revoked, expires, role)
}

// actorCheck returns checkActor for the shared settings operations to run
// last in their transaction, before the commit. A setting's change locks no
// tenant.
func actorCheck(by master.Actor) func(context.Context, pgx.Tx) error {
	return func(ctx context.Context, tx pgx.Tx) error { return checkActor(ctx, tx, by, tenantLock{}) }
}

// checkActor checks by inside a change's transaction. A change that can meet
// NotFound or Conflict runs it before it reads or writes anything else, so a
// refused actor learns nothing of what it named; a setting's change, which
// can meet neither, runs it last.
//
// It locks first and reads after, in a statement of its own. Under read
// committed each statement sees what was committed when it began, and a
// locking statement that waited re-reads only the row it locked, and only if
// that row changed: SetGrant and RemoveGrant lock the key's row without
// changing it, so a grant read in the statement that locked the key would be
// the grant as it was before the change the lock waited for. Read after the
// locks are held, the key and its grant are what was committed before them,
// and the locks keep them so until the commit:
//
//   - the key's row is held for share, which RevokeKey, SetGrant and
//     RemoveGrant (each locking it FOR NO KEY UPDATE first) wait for;
//   - when the role is on a tenant, the tenant's row is held for key share,
//     or for the stronger lock the change itself will take on that row
//     (lock), which RemoveTenant, the one change that removes grants
//     without locking their key, waits for. A tenant removed while this
//     waited has no row, and so no grants.
//
// The key's expiry is judged after the read, at the time the actor's clock
// gives then (master.Actor.Judge), so a change that waited for a lock past
// the expiry is refused.
func checkActor(ctx context.Context, tx pgx.Tx, by master.Actor, lock tenantLock) error {
	if !by.IsKey() {
		return nil
	}
	var one int
	err := tx.QueryRow(ctx, `SELECT 1 FROM api_keys WHERE id = $1 FOR SHARE`, by.KeyID()).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return by.Judge(false, time.Time{}, time.Time{}, "")
	}
	if err != nil {
		return err
	}
	tenantGone := false
	if scope := by.Scope(); !scope.IsInstallation() {
		clause := lockKeyShare
		if scope.TenantID == lock.id {
			clause = lock.clause
		}
		err := tx.QueryRow(ctx, `SELECT 1 FROM tenants WHERE id = $1 `+clause, scope.TenantID).Scan(&one)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			tenantGone = true
		case err != nil:
			return err
		}
	}
	found, revoked, expires, role, err := readActor(ctx, tx, actorSQL, by)
	if err != nil {
		return err
	}
	if tenantGone {
		role = ""
	}
	return by.Judge(found, revoked, expires, role)
}

// rowQuerier is what a pool and a transaction share for a one-row query.
type rowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// readActor runs query, actorSQL with or without its lock, for by's key.
func readActor(ctx context.Context, q rowQuerier, query string, by master.Actor) (found bool, revoked, expires time.Time, role master.Role, err error) {
	var rev, exp *time.Time
	var r *string
	err = q.QueryRow(ctx, query, by.KeyID(), by.Scope().TenantID).Scan(&rev, &exp, &r)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, time.Time{}, time.Time{}, "", nil
	}
	if err != nil {
		return false, time.Time{}, time.Time{}, "", err
	}
	if r != nil {
		role = master.Role(*r)
	}
	return true, fromNullMicros(rev), fromNullMicros(exp), role, nil
}

// masterLists names the master's settings and audit lists in their cursors,
// as they always were; a tenant's have names of their own (tenantLists).
var masterLists = lists{settings: listSettings, audit: listAudit, missing: "no such setting"}
