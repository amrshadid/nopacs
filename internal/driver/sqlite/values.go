package sqlite

import (
	"database/sql"
	"time"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
)

// micros returns t as microseconds since 1970 in UTC, the form every time is
// stored in. What is finer than a microsecond is dropped (master package
// documentation); master.ValidateTime keeps t after 1970, where dropping is
// the same as rounding down.
func micros(t time.Time) int64 {
	return t.UnixMicro()
}

// nullMicros is micros, with NULL for the zero time ("never").
func nullMicros(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UnixMicro()
}

// fromMicros is the inverse of micros, in UTC.
func fromMicros(v int64) time.Time {
	return time.UnixMicro(v).UTC()
}

// fromNullMicros is fromMicros, with the zero time for NULL.
func fromNullMicros(v sql.NullInt64) time.Time {
	if !v.Valid {
		return time.Time{}
	}
	return fromMicros(v.Int64)
}

// setMicros is fromNullMicros for a time whose zero means "never" (a key's
// expiry, revocation, suspension): a value that is not NULL never reads as
// the zero time (master.SetTime).
func setMicros(v sql.NullInt64) time.Time {
	if !v.Valid {
		return time.Time{}
	}
	return master.SetTime(fromMicros(v.Int64))
}

// oneRow returns NotFound with what unless the statement changed a row.
func oneRow(res sql.Result, err error, what string) error {
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fault.New(fault.NotFound, what)
	}
	return nil
}

// collect reads every row of a query with scan, closing the rows.
func collect[T any](rows *sql.Rows, err error, scan func(interface{ Scan(...any) error }) (T, error)) ([]T, error) {
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }() // Errors are reported by rows.Err.
	var out []T
	for rows.Next() {
		v, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
