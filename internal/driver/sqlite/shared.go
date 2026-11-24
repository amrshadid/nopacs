package sqlite

import (
	"context"
	"database/sql"
	"strconv"

	"github.com/amrshadid/nopacs/internal/driver/sqlkit"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
)

// The master and every tenant keep settings and an audit log in tables of
// the same shape (migrations/master and migrations/tenant), with the same
// value bounds and the same entry type. This file is the one implementation
// of both, so the two cannot drift: what one validates, refuses, pages or
// classifies, the other does the same way.

// sharedStatements are the statements of the settings and audit tables, which
// the master's statements and a tenant's both embed.
type sharedStatements struct {
	// Read pool.
	settingByName, listSettings *sql.Stmt
	// Write pool, used inside write transactions through Tx.StmtContext.
	upsertSetting, deleteSetting, insertAudit *sql.Stmt
}

// stmtEntry is one statement to prepare: where it goes, on which pool, and
// its text.
type stmtEntry struct {
	dst  **sql.Stmt
	db   *sql.DB
	text string
}

// sharedEntries returns the shared statements to prepare into s.
func sharedEntries(s *sharedStatements, p *pools) []stmtEntry {
	r, w := p.read, p.write
	return []stmtEntry{
		{&s.settingByName, r, `SELECT value FROM settings WHERE name = ?`},
		{&s.listSettings, r, `SELECT name, value FROM settings WHERE name > ? ORDER BY name LIMIT ?`},
		{&s.upsertSetting, w, `INSERT INTO settings (name, value) VALUES (?, ?)
			ON CONFLICT (name) DO UPDATE SET value = excluded.value`},
		{&s.deleteSetting, w, `DELETE FROM settings WHERE name = ?`},
		{&s.insertAudit, w, `INSERT INTO audit (` + auditColumns + `) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`},
	}
}

// sharedHost is what the shared operations need of a store: its base, its
// prepared shared statements, and a write transaction that hands them over.
type sharedHost interface {
	b() *base
	shared(ctx context.Context) (*sharedStatements, error)
	writeShared(ctx context.Context, message string, fn func(tx *sql.Tx, s *sharedStatements) error) error
}

// lists names one store's settings and audit lists in its cursors, so a
// cursor from the master's list is refused by a tenant's, and the other way
// round.
type lists struct {
	settings, audit string
	// missing is the NotFound message of a setting that is not set.
	missing string
	// owner is what every cursor of this store is also bound to: empty for
	// the master, the tenant's id for a tenant, so a page token from one
	// tenant's list is refused by another tenant's.
	owner string
}

// settingsBinding is what a settings cursor is bound to: the owner alone,
// since the list takes no arguments. The master's stays empty, as before.
func (l lists) settingsBinding() string { return l.owner }

// auditBinding is what an audit cursor is bound to: the whole filter, and
// for a tenant its id too. The master's is the filter's alone, as before.
func (l lists) auditBinding(f master.AuditFilter) string {
	if l.owner == "" {
		return auditBinding(f)
	}
	return sqlkit.Binding(l.owner, auditBinding(f))
}

// getSetting implements Setting.
func getSetting(ctx context.Context, h sharedHost, l lists, name string) (string, error) {
	if err := master.ValidateSettingName(name); err != nil {
		return "", err
	}
	s, err := h.shared(ctx)
	if err != nil {
		return "", err
	}
	var value string
	err = s.settingByName.QueryRowContext(ctx, name).Scan(&value)
	return value, h.b().notFound(ctx, err, l.missing, "the setting cannot be read")
}

// setSetting implements SetSetting. last, when not nil, runs in the write
// transaction after the change and its entry are written and before the
// commit, and an error from it undoes both: the master checks the change's
// actor there, and a tenant's store runs its Guard there, as late as either
// can be. A setting's change meets no NotFound or Conflict that a refused
// caller could learn from, so checking last costs nothing in what it tells.
func setSetting(ctx context.Context, h sharedHost, name, value string, audit master.AuditEntry, last func(context.Context, *sql.Tx) error) error {
	if err := master.ValidateSettingName(name); err != nil {
		return err
	}
	if err := master.ValidateSettingValue(value); err != nil {
		return err
	}
	if err := audit.Validate(); err != nil {
		return err
	}
	return h.writeShared(ctx, "the setting cannot be set", func(tx *sql.Tx, s *sharedStatements) error {
		if _, err := tx.StmtContext(ctx, s.upsertSetting).ExecContext(ctx, name, value); err != nil {
			return err
		}
		if err := execAudit(ctx, tx.StmtContext(ctx, s.insertAudit), audit); err != nil {
			return err
		}
		return runLast(ctx, tx, last)
	})
}

// runLast runs a change's last check, when there is one.
func runLast(ctx context.Context, tx *sql.Tx, last func(context.Context, *sql.Tx) error) error {
	if last == nil {
		return nil
	}
	return last(ctx, tx)
}

// unsetSetting implements UnsetSetting. last is as setSetting's.
func unsetSetting(ctx context.Context, h sharedHost, name string, audit master.AuditEntry, last func(context.Context, *sql.Tx) error) (bool, error) {
	if err := master.ValidateSettingName(name); err != nil {
		return false, err
	}
	if err := audit.Validate(); err != nil {
		return false, err
	}
	var removed bool
	err := h.writeShared(ctx, "the setting cannot be unset", func(tx *sql.Tx, s *sharedStatements) error {
		res, err := tx.StmtContext(ctx, s.deleteSetting).ExecContext(ctx, name)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		removed = n == 1
		if err := execAudit(ctx, tx.StmtContext(ctx, s.insertAudit), audit); err != nil {
			return err
		}
		return runLast(ctx, tx, last)
	})
	if err != nil {
		return false, err
	}
	return removed, nil
}

// listSettingsPage implements ListSettings. Its cursor is bound to the store's
// owner only: the list has no arguments.
func listSettingsPage(ctx context.Context, h sharedHost, l lists, page master.Page) ([]master.Setting, string, error) {
	if err := page.Validate(); err != nil {
		return nil, "", err
	}
	after := ""
	if page.Cursor != "" {
		f, err := sqlkit.DecodeCursor(page.Cursor, l.settings, l.settingsBinding(), 1)
		if err != nil {
			return nil, "", err
		}
		after = f[0]
	}
	s, err := h.shared(ctx)
	if err != nil {
		return nil, "", err
	}
	// Names are never empty, so "after the empty name" is the first page.
	rows, err := s.listSettings.QueryContext(ctx, after, page.Limit+1)
	out, err := collect(rows, err, func(row interface{ Scan(...any) error }) (master.Setting, error) {
		var st master.Setting
		err := row.Scan(&st.Name, &st.Value)
		return st, err
	})
	if err != nil {
		return nil, "", h.b().fail(ctx, err, "the settings cannot be listed")
	}
	out, last, more := sqlkit.Cut(out, page.Limit)
	if !more {
		return out, "", nil
	}
	return out, sqlkit.EncodeCursor(l.settings, l.settingsBinding(), last.Name), nil
}

// appendAudit implements AppendAudit.
func appendAudit(ctx context.Context, h sharedHost, entries []master.AuditEntry) error {
	if len(entries) > master.MaxAuditBatch {
		return fault.New(fault.Invalid, "at most "+strconv.Itoa(master.MaxAuditBatch)+" audit entries are appended at once")
	}
	for _, e := range entries {
		if err := e.Validate(); err != nil {
			return err
		}
	}
	if len(entries) == 0 {
		return h.b().open()
	}
	return h.writeShared(ctx, "the audit entries cannot be written", func(tx *sql.Tx, s *sharedStatements) error {
		stmt := tx.StmtContext(ctx, s.insertAudit)
		for _, e := range entries {
			if err := execAudit(ctx, stmt, e); err != nil {
				return err
			}
		}
		return nil
	})
}

// listAuditPage implements ListAudit. The cursor is bound to the whole
// filter.
func listAuditPage(ctx context.Context, h sharedHost, l lists, filter master.AuditFilter, page master.Page) ([]master.AuditEntry, string, error) {
	if err := filter.Validate(); err != nil {
		return nil, "", err
	}
	if err := page.Validate(); err != nil {
		return nil, "", err
	}
	binding := l.auditBinding(filter)
	var after []any
	if page.Cursor != "" {
		f, err := sqlkit.DecodeCursor(page.Cursor, l.audit, binding, 2)
		if err != nil {
			return nil, "", err
		}
		at, err := sqlkit.DecodeCursorInt(f[0])
		if err != nil {
			return nil, "", err
		}
		after = []any{at, f[1]}
	}
	query, args := auditQuery(filter, after, page.Limit+1)
	b := h.b()
	if err := b.open(); err != nil {
		return nil, "", err
	}
	rows, err := b.p.read.QueryContext(ctx, query, args...)
	out, err := collect(rows, err, scanAudit)
	if err != nil {
		return nil, "", b.fail(ctx, err, "the audit log cannot be listed")
	}
	out, last, more := sqlkit.Cut(out, page.Limit)
	if !more {
		return out, "", nil
	}
	return out, sqlkit.EncodeCursor(l.audit, binding, strconv.FormatInt(micros(last.Time), 10), last.ID), nil
}
