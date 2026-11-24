package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/amrshadid/nopacs/internal/master"
)

// Setting implements master.Store.
func (m *Master) Setting(ctx context.Context, name string) (string, error) {
	return getSetting(ctx, m, masterLists, name)
}

// SetSetting implements master.Store.
func (m *Master) SetSetting(ctx context.Context, name, value string, audit master.AuditEntry, by master.Actor) error {
	if err := by.Validate(); err != nil {
		return err
	}
	return setSetting(ctx, m, name, value, audit, m.actorCheck(by))
}

// UnsetSetting implements master.Store.
func (m *Master) UnsetSetting(ctx context.Context, name string, audit master.AuditEntry, by master.Actor) (bool, error) {
	if err := by.Validate(); err != nil {
		return false, err
	}
	return unsetSetting(ctx, m, name, audit, m.actorCheck(by))
}

// actorCheck returns the check of by that the shared settings operations
// run last in their write transaction, with the master's own statement.
func (m *Master) actorCheck(by master.Actor) func(ctx context.Context, tx *sql.Tx) error {
	return func(ctx context.Context, tx *sql.Tx) error {
		s, err := m.ready(ctx)
		if err != nil {
			return err
		}
		return checkActor(ctx, tx, s, by)
	}
}

// CheckActor implements master.Store with actorSQL on the read pool. An
// actor that is not a key reads nothing.
func (m *Master) CheckActor(ctx context.Context, by master.Actor) error {
	if err := by.Validate(); err != nil {
		return err
	}
	if !by.IsKey() {
		return m.open()
	}
	s, err := m.ready(ctx)
	if err != nil {
		return err
	}
	return m.fail(ctx, actorMay(ctx, s.actorRead, by), "the acting key cannot be read")
}

// checkActor checks by inside a write transaction. A change that can meet
// NotFound or Conflict runs it before it reads or writes anything else, so a
// refused actor learns nothing of what it named; a setting's change, which
// can meet neither, runs it last. The transaction holds the database's write
// lock from its BEGIN IMMEDIATE, so no other writer can undo what was read
// before the commit.
func checkActor(ctx context.Context, tx *sql.Tx, s *statements, by master.Actor) error {
	if !by.IsKey() {
		return nil
	}
	return actorMay(ctx, tx.StmtContext(ctx, s.actorWrite), by)
}

// actorMay reads by's key and role with stmt (actorSQL) and returns
// master.Actor.Judge's answer: nil, a refusal, or the read's own failure.
func actorMay(ctx context.Context, stmt *sql.Stmt, by master.Actor) error {
	if !by.IsKey() {
		return nil
	}
	var revoked, expires sql.NullInt64
	var role sql.NullString
	err := stmt.QueryRowContext(ctx, by.KeyID(), by.Scope().TenantID).Scan(&revoked, &expires, &role)
	if errors.Is(err, sql.ErrNoRows) {
		return by.Judge(false, time.Time{}, time.Time{}, "")
	}
	if err != nil {
		return err
	}
	return by.Judge(true, fromNullMicros(revoked), fromNullMicros(expires), master.Role(role.String))
}

// ListSettings implements master.Store.
func (m *Master) ListSettings(ctx context.Context, page master.Page) ([]master.Setting, string, error) {
	return listSettingsPage(ctx, m, masterLists, page)
}

// masterLists names the master's settings and audit lists for the code it
// shares with a tenant's database (shared.go).
var masterLists = lists{settings: listSettings, audit: listAudit, missing: "no such setting"}
