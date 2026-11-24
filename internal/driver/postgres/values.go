package postgres

import (
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
)

// micros returns t cut to a whole microsecond, in UTC. Postgres keeps
// timestamptz to the microsecond; what is finer is dropped here, the same
// way, so what is written is exactly what is read back (master package
// documentation). master.ValidateTime keeps t after 1970, where dropping is
// the same as rounding down.
func micros(t time.Time) time.Time {
	return microsTime(t.UnixMicro())
}

// microsTime is the time v microseconds after 1970, in UTC.
func microsTime(v int64) time.Time {
	return time.UnixMicro(v).UTC()
}

// nullMicros is micros, with NULL for the zero time ("never").
func nullMicros(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return micros(t)
}

// fromNullMicros reads a nullable timestamptz, with the zero time for NULL,
// in UTC.
func fromNullMicros(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return t.UTC()
}

// setMicros is fromNullMicros for a time whose zero means "never" (a key's
// expiry, revocation, suspension): a value that is not NULL never reads as
// the zero time (master.SetTime).
func setMicros(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return master.SetTime(t.UTC())
}

// notFoundIn turns pgx.ErrNoRows into NotFound with what and passes any
// other error through for write to classify.
func notFoundIn(err error, what string) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return fault.New(fault.NotFound, what)
	}
	return err
}

// oneRow returns NotFound with what unless the statement changed a row.
func oneRow(tag pgconn.CommandTag, err error, what string) error {
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fault.New(fault.NotFound, what)
	}
	return nil
}

// collect reads every row of a query with scan, closing the rows.
func collect[T any](rows pgx.Rows, err error, scan func(scanner) (T, error)) ([]T, error) {
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (T, error) { return scan(row) })
}
