package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/amrshadid/nopacs/internal/driver/sqlkit"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
)

// AppendAudit implements master.Store.
func (m *Master) AppendAudit(ctx context.Context, entries ...master.AuditEntry) error {
	return appendAudit(ctx, m, entries)
}

// insertAudit writes one audit entry inside the transaction of the change it
// records.
func insertAudit(ctx context.Context, tx *sql.Tx, s *statements, e master.AuditEntry) error {
	return execAudit(ctx, tx.StmtContext(ctx, s.insertAudit), e)
}

// execAudit writes one audit entry with the transaction's insert statement.
// The audit table's only unique key is the id, so a unique violation here is
// a duplicate id: master.ErrDuplicateAuditID, never a Conflict.
func execAudit(ctx context.Context, stmt *sql.Stmt, e master.AuditEntry) error {
	args, err := auditArgs(e)
	if err != nil {
		return err
	}
	_, err = stmt.ExecContext(ctx, args...)
	return duplicateAudit(err)
}

// auditArgs returns e's values in the order of auditColumns.
func auditArgs(e master.AuditEntry) ([]any, error) {
	details, err := sqlkit.EncodeDetails(e.Details)
	if err != nil {
		return nil, err
	}
	return []any{e.ID, micros(e.Time), e.KeyID, e.LocalUser, e.RemoteAddress,
		e.Method, e.TenantName, e.Object, string(e.Outcome), e.Reason, details, e.RequestID}, nil
}

// duplicateAudit turns a unique violation of an audit insert into
// master.ErrDuplicateAuditID (Internal) and passes any other error through.
func duplicateAudit(err error) error {
	if isUniqueViolation(err) {
		return fault.Wrap(fault.Internal, errors.Join(master.ErrDuplicateAuditID, err), "an audit entry with this id exists")
	}
	return err
}

const auditColumns = `id, at, key_id, local_user, remote_addr, method, tenant_name, object, outcome, reason, details, request_id`

// scanAudit reads one row of auditColumns.
func scanAudit(row interface{ Scan(...any) error }) (master.AuditEntry, error) {
	var e master.AuditEntry
	var at int64
	var outcome, details string
	if err := row.Scan(&e.ID, &at, &e.KeyID, &e.LocalUser, &e.RemoteAddress, &e.Method,
		&e.TenantName, &e.Object, &outcome, &e.Reason, &details, &e.RequestID); err != nil {
		return e, err
	}
	e.Time = fromMicros(at)
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
		return strconv.FormatInt(micros(x), 10)
	}
	return sqlkit.Binding(f.TenantName, f.KeyID, string(f.Outcome), t(f.Since), t(f.Until))
}

// auditQuery returns the statement and arguments of one page of ListAudit.
// The statement is put together from fixed fragments according to which
// filters are set, so that each combination reads the index named for it in
// migrations/master/0001_baseline.sql instead of testing "parameter is empty OR column =
// parameter", which SQLite cannot answer from an index. No value is ever
// written into the text; every one is a parameter. The cursor, when there is
// one, is the (at, id) of the last row returned.
func auditQuery(filter master.AuditFilter, after []any, limit int) (string, []any) {
	var where []string
	var args []any
	if filter.TenantName != "" {
		where = append(where, "tenant_name = ?")
		args = append(args, filter.TenantName)
	}
	if filter.KeyID != "" {
		where = append(where, "key_id = ?")
		args = append(args, filter.KeyID)
	}
	if filter.Outcome != "" {
		where = append(where, "outcome = ?")
		args = append(args, string(filter.Outcome))
	}
	if !filter.Since.IsZero() {
		where = append(where, "at >= ?")
		args = append(args, micros(filter.Since))
	}
	if !filter.Until.IsZero() {
		where = append(where, "at < ?")
		args = append(args, micros(filter.Until))
	}
	if after != nil {
		// Newest first, so the next page is what sorts before the last row.
		where = append(where, "(at, id) < (?, ?)")
		args = append(args, after...)
	}
	query := "SELECT " + auditColumns + " FROM audit"
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ") // #nosec G202 -- every fragment is a constant above; values are parameters.
	}
	// The id breaks ties between entries of the same time, so the order is
	// total and a cursor at (at, id) is unambiguous.
	query += " ORDER BY at DESC, id DESC LIMIT ?"
	return query, append(args, limit)
}

// ListAudit implements master.Store. The cursor is bound to the whole filter.
func (m *Master) ListAudit(ctx context.Context, filter master.AuditFilter, page master.Page) ([]master.AuditEntry, string, error) {
	return listAuditPage(ctx, m, masterLists, filter, page)
}
