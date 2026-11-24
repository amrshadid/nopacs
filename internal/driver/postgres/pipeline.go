package postgres

import (
	"context"
	"errors"
	"math/rand/v2"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/amrshadid/nopacs/internal/driver/sqlkit"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// The pipeline's operations on a tenant's database (tenant.Pipeline), in
// pipeline_*.go, on the tables of migrations/tenant/0001_baseline.sql. The
// package documentation lists the row locks each change takes, in which
// order, and why; every change goes through Tenant.pipe, which retries a
// transaction Postgres ended as a deadlock or a serialization failure, so
// fn may run more than once and only assigns its results.

var _ tenant.Pipeline = (*Tenant)(nil)

// The names of the pipeline's lists in their cursors, apart from each other
// and from the settings and audit lists. They are the SQLite driver's, so a
// cursor means the same list in both.
const (
	listStorages      = "tenant-storages"
	listReceivers     = "tenant-receivers"
	listReceptions    = "tenant-receptions"
	listReceivedFiles = "tenant-received-files"
	listQuarantine    = "tenant-quarantine"
	listDiscarded     = "tenant-discarded-files"
	listWithoutWork   = "tenant-receptions-without-work"
	listScanInstances = "tenant-scan-instances"
)

// SQLSTATEs a change is tried again for (appendix A of the Postgres manual).
const (
	sqlstateDeadlock      = "40P01"
	sqlstateSerialization = "40001"
)

// maxTxAttempts bounds how many times pipe runs one change. A deadlock
// should not happen at all, since every change takes its row locks in the
// one order the package documentation states; the bound is what keeps one
// that does (from a lock a later change takes out of that order, or from the
// hook a step runs) from reaching the caller, and keeps a change that keeps
// meeting one from running forever.
const maxTxAttempts = 6

// retryable reports whether a change that failed with err, before any of its
// statements committed, may be run again in a new transaction: Postgres
// ended it as a deadlock or a serialization failure, which say nothing about
// the request, or a unique key met a row that a transaction committed after
// the change read (every insert the pipeline makes is written to meet that
// with ON CONFLICT DO NOTHING and a read; a unique violation that still
// comes is a race no statement foresaw, and running the change again reads
// the row that won). An answer the change decided (an error with a kind:
// a Conflict, a precondition, a duplicate audit id) is never run again.
func retryable(err error) bool {
	var fe *fault.Error
	if err == nil || errors.As(err, &fe) {
		return false
	}
	var pe *pgconn.PgError
	if !errors.As(err, &pe) {
		return false
	}
	switch pe.Code {
	case sqlstateDeadlock, sqlstateSerialization, sqlstateUniqueViolation:
		return true
	}
	return false
}

// pipe runs fn in one write transaction (base.write, read committed), and
// runs it again in a new one, up to maxTxAttempts in all, when it failed in a
// way retryable says nothing about the request. A unique violation still
// there after the last attempt is Unavailable, with a message to try again:
// it is a race, never the caller's mistake, and its constraint's name is not
// a message for a person.
func (t *Tenant) pipe(ctx context.Context, message string, fn func(tx pgx.Tx) error) error {
	for attempt := 1; ; attempt++ {
		var raw error
		err := t.write(ctx, message, func(tx pgx.Tx) error {
			raw = fn(tx)
			if raw == nil && t.seams.failAttempt != nil {
				raw = t.seams.failAttempt(attempt)
			}
			return raw
		})
		if err == nil || !retryable(raw) || ctx.Err() != nil {
			return err
		}
		t.retried.count(raw)
		if attempt == maxTxAttempts {
			if isUniqueViolation(raw) {
				return fault.Wrap(fault.Unavailable, t.red.scrub(raw), message+": the rows it needs kept changing; try again")
			}
			return err
		}
		// A short wait that grows, with a random part, so two changes that
		// met each other do not meet again at once.
		wait := time.Duration(attempt)*time.Millisecond + time.Duration(rand.Int64N(int64(2*time.Millisecond))) //nolint:gosec // G404: spreading retries needs no secret randomness.
		select {
		case <-ctx.Done():
			return t.fail(ctx, ctx.Err(), message)
		case <-time.After(wait):
		}
	}
}

// retryCounts are the changes pipe ran again, by the SQLSTATE that ended
// them.
type retryCounts struct {
	deadlocks, serialization, unique atomic.Int64
}

// count records one change run again after err.
func (c *retryCounts) count(err error) {
	var pe *pgconn.PgError
	if !errors.As(err, &pe) {
		return
	}
	switch pe.Code {
	case sqlstateDeadlock:
		c.deadlocks.Add(1)
	case sqlstateSerialization:
		c.serialization.Add(1)
	case sqlstateUniqueViolation:
		c.unique.Add(1)
	}
}

// configLockKey is the advisory lock that serializes a tenant's changes of
// configuration. An advisory lock belongs to the whole database, so the key
// is made from the schema's name too, as the migration lock's is: two
// tenants in two schemas of one database never wait for each other.
const configLockKey = `hashtextextended('nopacs/tenant-config/' || current_schema(), 0)`

// lockConfig takes the tenant's configuration lock for the rest of the
// transaction (pg_advisory_xact_lock, released by the commit or the
// rollback). Every change of a storage or a receiver takes it first, so the
// rules across rows that each checks (a name, an overlap of storages, a port
// and address, a storage a receiver names, a receiver's name still held by a
// record) are checked with no other such change in flight. An advisory lock
// rather than a row: no table has a row that stands for "the configuration",
// and a lock that needs none cannot be forgotten by a migration.
func lockConfig(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(`+configLockKey+`)`)
	return err
}

// audited ends a change a person or a key asked for: its audit entry is
// written in its transaction, then its Guard runs, last, as Store says.
func (t *Tenant) audited(ctx context.Context, tx pgx.Tx, audit master.AuditEntry, guard tenant.Guard) error {
	if err := insertAudit(ctx, tx, t.red, audit); err != nil {
		return err
	}
	return runLast(ctx, tx, guarded(guard))
}

// exists reports whether query, run in tx with args, returns a row.
func exists(ctx context.Context, tx pgx.Tx, query string, args ...any) (bool, error) {
	var one int
	err := tx.QueryRow(ctx, query, args...).Scan(&one)
	if isNoRow(err) {
		return false, nil
	}
	return err == nil, err
}

// inUse reports whether query, an "is there one" lookup of a value that
// many rows may share (a storage, a receiver's name), returns a row, run in
// tx with args. It is run as an unnamed statement (pgx's
// QueryExecModeExec), which Postgres plans for the value it is given every
// time. A statement pgx keeps prepared gets, after a few runs, a generic
// plan made for any value; for these that is a scan of the table that stops
// at its first match, cheap for a value many rows hold and a read of the
// whole table for one no row holds, which is the answer these checks
// usually get. The plan test shows the plan for such a value.
func inUse(ctx context.Context, tx pgx.Tx, query string, args ...any) (bool, error) {
	return exists(ctx, tx, query, append([]any{pgx.QueryExecModeExec}, args...)...)
}

// readRows runs query on the pool and scans every row.
func readRows[T any](ctx context.Context, t *Tenant, message, query string, args []any, scan func(scanner) (T, error)) ([]T, error) {
	if err := t.open(); err != nil {
		return nil, err
	}
	rows, err := t.pool.Query(ctx, query, args...)
	out, err := collect(rows, err, scan)
	if err != nil {
		return nil, t.fail(ctx, err, message)
	}
	return out, nil
}

// readOne runs query on the pool and scans its one row, or returns NotFound
// with what.
func readOne[T any](ctx context.Context, t *Tenant, what, message, query string, args []any, scan func(scanner) (T, error)) (T, error) {
	var zero T
	if err := t.open(); err != nil {
		return zero, err
	}
	v, err := scan(t.pool.QueryRow(ctx, query, args...))
	if err != nil {
		return zero, t.notFound(ctx, err, what, message)
	}
	return v, nil
}

// binding is what a pipeline list's cursor is bound to: the tenant and the
// list's arguments.
func (t *Tenant) binding(parts ...string) string {
	return sqlkit.Binding(append([]string{t.lists.owner}, parts...)...)
}

// pageAfterName decodes a list's cursor that holds one name, or returns ""
// for the first page.
func (t *Tenant) pageAfterName(page master.Page, list string) (string, error) {
	if err := page.Validate(); err != nil {
		return "", err
	}
	if page.Cursor == "" {
		return "", nil
	}
	f, err := sqlkit.DecodeCursor(page.Cursor, list, t.binding(), 1)
	if err != nil {
		return "", err
	}
	return f[0], nil
}

// nullSum returns NULL for a zero checksum, its bytes otherwise.
func nullSum(s tenant.SHA256) any {
	if s.IsZero() {
		return nil
	}
	return sumBytes(s)
}

// sumBytes returns a checksum's bytes in a slice of their own.
func sumBytes(s tenant.SHA256) []byte {
	b := make([]byte, len(s))
	copy(b, s[:])
	return b
}

// rowsChanged returns how many rows a statement changed, or its error.
func rowsChanged(tag pgconn.CommandTag, err error) (int64, error) {
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
