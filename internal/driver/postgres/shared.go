package postgres

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/amrshadid/nopacs/internal/driver/sqlkit"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
)

// The master and every tenant keep settings and an audit log in tables of
// the same shape (migrations/master and migrations/tenant), with the same
// value bounds and the same entry type. This file is the one implementation
// of both, so the two cannot drift: what one validates, refuses, pages or
// classifies, the other does the same way.

// lists names one store's settings and audit lists in their cursors, so a
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
// since the list takes no arguments. The master's stays empty.
func (l lists) settingsBinding() string { return l.owner }

// auditBindingFor is what an audit cursor is bound to: the whole filter, and
// for a tenant its id too. The master's is the filter's alone.
func (l lists) auditBindingFor(f master.AuditFilter) string {
	if l.owner == "" {
		return auditBinding(f)
	}
	return sqlkit.Binding(l.owner, auditBinding(f))
}

// getSetting implements Setting.
func getSetting(ctx context.Context, b *base, l lists, name string) (string, error) {
	if err := master.ValidateSettingName(name); err != nil {
		return "", err
	}
	if err := b.open(); err != nil {
		return "", err
	}
	var value string
	err := b.pool.QueryRow(ctx, `SELECT value FROM settings WHERE name = $1`, name).Scan(&value)
	return value, b.notFound(ctx, err, l.missing, "the setting cannot be read")
}

// setSetting implements SetSetting. last, when not nil, runs in the
// transaction after the change and its entry are written, so after the
// setting's row is locked, and before the commit; an error from it undoes
// both: the master checks the change's actor there, and a tenant's store runs
// its Guard there, as late as either can be. A setting's change meets no
// NotFound or Conflict that a refused caller could learn from, so checking
// last costs nothing in what it tells.
func setSetting(ctx context.Context, b *base, name, value string, audit master.AuditEntry, last func(context.Context, pgx.Tx) error) error {
	if err := master.ValidateSettingName(name); err != nil {
		return err
	}
	if err := master.ValidateSettingValue(value); err != nil {
		return err
	}
	if err := audit.Validate(); err != nil {
		return err
	}
	return b.write(ctx, "the setting cannot be set", func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO settings (name, value) VALUES ($1, $2)
			ON CONFLICT (name) DO UPDATE SET value = EXCLUDED.value`, name, value); err != nil {
			return err
		}
		if err := insertAudit(ctx, tx, b.red, audit); err != nil {
			return err
		}
		return runLast(ctx, tx, last)
	})
}

// runLast runs a change's last check, when there is one.
func runLast(ctx context.Context, tx pgx.Tx, last func(context.Context, pgx.Tx) error) error {
	if last == nil {
		return nil
	}
	return last(ctx, tx)
}

// unsetSetting implements UnsetSetting. last is as setSetting's.
func unsetSetting(ctx context.Context, b *base, name string, audit master.AuditEntry, last func(context.Context, pgx.Tx) error) (bool, error) {
	if err := master.ValidateSettingName(name); err != nil {
		return false, err
	}
	if err := audit.Validate(); err != nil {
		return false, err
	}
	var removed bool
	err := b.write(ctx, "the setting cannot be unset", func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `DELETE FROM settings WHERE name = $1`, name)
		if err != nil {
			return err
		}
		removed = tag.RowsAffected() == 1
		if err := insertAudit(ctx, tx, b.red, audit); err != nil {
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
func listSettingsPage(ctx context.Context, b *base, l lists, page master.Page) ([]master.Setting, string, error) {
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
	if err := b.open(); err != nil {
		return nil, "", err
	}
	rows, err := b.pool.Query(ctx, `SELECT name, value FROM settings WHERE name > $1 ORDER BY name LIMIT $2`, after, page.Limit+1)
	out, err := collect(rows, err, func(row scanner) (master.Setting, error) {
		var st master.Setting
		err := row.Scan(&st.Name, &st.Value)
		return st, err
	})
	if err != nil {
		return nil, "", b.fail(ctx, err, "the settings cannot be listed")
	}
	out, last, more := sqlkit.Cut(out, page.Limit)
	if !more {
		return out, "", nil
	}
	return out, sqlkit.EncodeCursor(l.settings, l.settingsBinding(), last.Name), nil
}

// appendAudit implements AppendAudit. The batch is one INSERT, all or
// nothing, in a transaction of its own like every other write: a bare INSERT
// whose context ends while the server answers could return an error for rows
// that were stored, and a call that fails must have stored nothing.
func appendAudit(ctx context.Context, b *base, entries []master.AuditEntry) error {
	if len(entries) > master.MaxAuditBatch {
		return fault.New(fault.Invalid, "at most "+strconv.Itoa(master.MaxAuditBatch)+" audit entries are appended at once")
	}
	for _, e := range entries {
		if err := e.Validate(); err != nil {
			return err
		}
	}
	if len(entries) == 0 {
		return b.open()
	}
	return b.write(ctx, "the audit entries cannot be written", func(tx pgx.Tx) error {
		return execAudit(ctx, tx, b.red, entries)
	})
}

// listAuditPage implements ListAudit. The cursor is bound to the whole
// filter.
func listAuditPage(ctx context.Context, b *base, l lists, filter master.AuditFilter, page master.Page) ([]master.AuditEntry, string, error) {
	if err := filter.Validate(); err != nil {
		return nil, "", err
	}
	if err := page.Validate(); err != nil {
		return nil, "", err
	}
	binding := l.auditBindingFor(filter)
	var after *auditPosition
	if page.Cursor != "" {
		f, err := sqlkit.DecodeCursor(page.Cursor, l.audit, binding, 2)
		if err != nil {
			return nil, "", err
		}
		at, err := sqlkit.DecodeCursorInt(f[0])
		if err != nil {
			return nil, "", err
		}
		after = &auditPosition{at: microsTime(at), id: f[1]}
	}
	query, args := auditQuery(filter, after, page.Limit+1)

	if err := b.open(); err != nil {
		return nil, "", err
	}
	rows, err := b.pool.Query(ctx, query, args...)
	out, err := collect(rows, err, scanAudit)
	if err != nil {
		return nil, "", b.fail(ctx, err, "the audit log cannot be listed")
	}
	out, last, more := sqlkit.Cut(out, page.Limit)
	if !more {
		return out, "", nil
	}
	return out, sqlkit.EncodeCursor(l.audit, binding, strconv.FormatInt(last.Time.UnixMicro(), 10), last.ID), nil
}

const auditColumns = `id, at, key_id, local_user, remote_addr, method, tenant_name, object, outcome, reason, details, request_id`

// insertAudit writes one audit entry inside the transaction of the change it
// records.
func insertAudit(ctx context.Context, tx pgx.Tx, red *redactor, e master.AuditEntry) error {
	return execAudit(ctx, tx, red, []master.AuditEntry{e})
}

// execAudit writes entries with one statement: a VALUES list for the one
// entry every request ends with, and unnest over twelve arrays for a batch,
// so a batch of 10,000 is still twelve parameters and one round trip. An id
// already taken is master.ErrDuplicateAuditID.
func execAudit(ctx context.Context, tx pgx.Tx, red *redactor, entries []master.AuditEntry) error {
	var err error
	if len(entries) == 1 {
		e := entries[0]
		var details string
		if details, err = sqlkit.EncodeDetails(e.Details); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO audit (`+auditColumns+`) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
			e.ID, micros(e.Time), e.KeyID, e.LocalUser, e.RemoteAddress, e.Method, e.TenantName, e.Object,
			string(e.Outcome), e.Reason, details, e.RequestID)
	} else {
		n := len(entries)
		col := func() []string { return make([]string, 0, n) }
		ids, keys, users, addrs, methods, tenants, objects, outcomes, reasons, dets, reqs := col(), col(), col(), col(), col(), col(), col(), col(), col(), col(), col()
		times := make([]time.Time, 0, n)
		for _, e := range entries {
			var details string
			if details, err = sqlkit.EncodeDetails(e.Details); err != nil {
				return err
			}
			ids, times, keys = append(ids, e.ID), append(times, micros(e.Time)), append(keys, e.KeyID)
			users, addrs, methods = append(users, e.LocalUser), append(addrs, e.RemoteAddress), append(methods, e.Method)
			tenants, objects, outcomes = append(tenants, e.TenantName), append(objects, e.Object), append(outcomes, string(e.Outcome))
			reasons, dets, reqs = append(reasons, e.Reason), append(dets, details), append(reqs, e.RequestID)
		}
		_, err = tx.Exec(ctx, `INSERT INTO audit (`+auditColumns+`)
			SELECT id, at, key_id, local_user, remote_addr, method, tenant_name, object, outcome, reason, details::jsonb, request_id
			FROM unnest($1::text[], $2::timestamptz[], $3::text[], $4::text[], $5::text[], $6::text[], $7::text[], $8::text[], $9::text[], $10::text[], $11::text[], $12::text[])
			AS t(id, at, key_id, local_user, remote_addr, method, tenant_name, object, outcome, reason, details, request_id)`,
			ids, times, keys, users, addrs, methods, tenants, objects, outcomes, reasons, dets, reqs)
	}
	if constraintViolation(err, "audit_pkey") {
		// The audit table's only unique key is the id: a duplicate id is
		// master.ErrDuplicateAuditID, never a Conflict.
		return fault.Wrap(fault.Internal, errors.Join(master.ErrDuplicateAuditID, red.scrub(err)), "an audit entry with this id exists")
	}
	return err
}

// scanAudit reads one row of auditColumns.
func scanAudit(row scanner) (master.AuditEntry, error) {
	var e master.AuditEntry
	var at time.Time
	var outcome, details string
	if err := row.Scan(&e.ID, &at, &e.KeyID, &e.LocalUser, &e.RemoteAddress, &e.Method,
		&e.TenantName, &e.Object, &outcome, &e.Reason, &details, &e.RequestID); err != nil {
		return e, err
	}
	e.Time = at.UTC()
	e.Outcome = master.Outcome(outcome)
	var err error
	e.Details, err = sqlkit.DecodeDetails(details)
	return e, err
}

// auditBinding is what a ListAudit cursor is bound to: every field of the
// filter, so a cursor made with one filter is refused with another.
func auditBinding(f master.AuditFilter) string {
	t := func(x time.Time) string {
		if x.IsZero() {
			return ""
		}
		return strconv.FormatInt(x.UnixMicro(), 10)
	}
	return sqlkit.Binding(f.TenantName, f.KeyID, string(f.Outcome), t(f.Since), t(f.Until))
}

// auditPosition is the (at, id) of the last row of the previous page.
type auditPosition struct {
	at time.Time
	id string
}

// auditQuery returns the statement and arguments of one page of ListAudit.
// The statement is put together from fixed fragments according to which
// filters are set, so that each combination reads the index named for it in
// the schema instead of testing "parameter is NULL OR column = parameter",
// which no index answers. No value is ever written into the text; every one
// is a parameter, and pgx's statement cache keeps one prepared statement per
// combination.
func auditQuery(filter master.AuditFilter, after *auditPosition, limit int) (string, []any) {
	var where []string
	var args []any
	arg := func(v any) string {
		args = append(args, v)
		return "$" + strconv.Itoa(len(args))
	}
	if filter.TenantName != "" {
		where = append(where, "tenant_name = "+arg(filter.TenantName))
	}
	if filter.KeyID != "" {
		where = append(where, "key_id = "+arg(filter.KeyID))
	}
	if filter.Outcome != "" {
		where = append(where, "outcome = "+arg(string(filter.Outcome)))
	}
	if !filter.Since.IsZero() {
		where = append(where, "at >= "+arg(micros(filter.Since)))
	}
	if !filter.Until.IsZero() {
		where = append(where, "at < "+arg(micros(filter.Until)))
	}
	if after != nil {
		// Newest first, so the next page is what sorts before the last row.
		where = append(where, "(at, id) < ("+arg(after.at)+", "+arg(after.id)+")")
	}
	query := "SELECT " + auditColumns + " FROM audit"
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ") // #nosec G202 -- every fragment is a constant above; values are parameters.
	}
	// The id breaks ties between entries of the same time, so the order is
	// total and a cursor at (at, id) is unambiguous.
	query += " ORDER BY at DESC, id DESC LIMIT " + arg(limit)
	return query, args
}
