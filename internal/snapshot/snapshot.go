// Package snapshot is the port through which a backup reads a database
// whole and a restore writes it back: the rows of every table but the
// catalog's, as typed values the same for every database type, read in one
// read transaction, and the recovery point that says which moment they
// stand for.
//
// The catalog (subjects, sessions, scans, instances and what is derived
// from them: session_modalities, catalog_totals, storage_totals,
// reception_scans) is never in a snapshot: a restore rebuilds it from the
// storages, so a catalog can never be older than the bytes it points at.
//
// # Values
//
// A column has one Kind, and each value of it is one Go type, or nil for
// NULL:
//
//	KindInt    int64
//	KindText   string
//	KindBytes  []byte
//	KindBool   bool
//	KindTime   time.Time, in UTC, to the microsecond
//	KindFloat  float64
//
// A database keeps some of these in another form (SQLite keeps a time as
// an integer of microseconds and a bool as 0 or 1); a snapshot gives each
// value in the form its own column has, and a load converts it to the form
// the target's column has (Convert), so a snapshot of one database type
// loads into the other when their tables agree by name and columns.
//
// # What this package must never do
//
// It must never import a database driver, nor read a clock, a file or the
// network: it states operations, and the drivers implement them.
package snapshot

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/amrshadid/nopacs/internal/fault"
)

// Kind is the type of a column's values.
type Kind string

// The kinds of column.
const (
	KindInt   Kind = "int"
	KindText  Kind = "text"
	KindBytes Kind = "bytes"
	KindBool  Kind = "bool"
	KindTime  Kind = "time"
	KindFloat Kind = "float"
)

// Valid reports whether k is one of the six kinds.
func (k Kind) Valid() bool {
	switch k {
	case KindInt, KindText, KindBytes, KindBool, KindTime, KindFloat:
		return true
	}
	return false
}

// Catalog lists the tables a snapshot leaves out: the catalog, which a
// restore rebuilds from the storages.
var Catalog = []string{"subjects", "sessions", "scans", "instances", "session_modalities", "catalog_totals",
	"storage_totals", "reception_scans"}

// IsCatalog reports whether the table named name is one of Catalog.
func IsCatalog(name string) bool {
	for _, c := range Catalog {
		if c == name {
			return true
		}
	}
	return false
}

// Column is one column of a table.
type Column struct {
	// Name is the column's name.
	Name string
	// Kind is the type of its values.
	Kind Kind
}

// Table is a table's name and its columns, in their order.
type Table struct {
	// Name is the table's name.
	Name string
	// Columns are its columns.
	Columns []Column
}

// Owner is the owner generation a tenant's database holds for one storage.
type Owner struct {
	// Storage is the storage's name.
	Storage string
	// Generation is the generation of its owner record this database wrote.
	Generation int64
}

// Point is a snapshot's recovery point: the moment its rows stand for.
type Point struct {
	// Time is when its read transaction began, in UTC.
	Time time.Time
	// Version is the database's schema version.
	Version int
	// EventHistory is a tenant's event feed's history id; "" for the
	// master.
	EventHistory string
	// LastCorrection is the operation id of the newest correction a
	// tenant's database holds; "" for none and for the master.
	LastCorrection string
	// LastReception is the id of the newest reception a tenant's database
	// holds, by its start; "" for none and for the master.
	LastReception string
	// Owners are a tenant's owner generations, by storage name; none for the
	// master.
	Owners []Owner
}

// Sink takes a snapshot as it is read: each table, then its rows, then the
// sequences. A Sink's error ends the snapshot.
type Sink interface {
	// Table begins a table; the rows that follow are of it.
	Table(t Table) error
	// Row takes one row of the current table, a value per column in the
	// table's order. The slice is reused for the next row.
	Row(values []any) error
	// Sequences takes, last, the next-id counter of each table whose ids the
	// database makes, by table name: an id at or below it was given once,
	// so a restore never gives it again.
	Sequences(seqs map[string]int64) error
}

// Source gives a snapshot back to a load, in the order a Sink took it.
type Source interface {
	// NextTable returns the next table, or false when no table is left.
	NextTable() (Table, bool, error)
	// NextRow fills values, one per column of the current table, or returns
	// false when its rows are done.
	NextRow(values []any) (bool, error)
	// Sequences returns the counters a Sink took, once every table is read.
	Sequences() (map[string]int64, error)
}

// Database is a database a backup reads and a restore writes: the master
// or a tenant's. A driver implements it beside its store.
type Database interface {
	// Snapshot reads every table but the catalog's in one read
	// transaction, in an order that loads (a table before any that refers
	// to it), into sink, and returns the recovery point.
	Snapshot(ctx context.Context, sink Sink) (Point, error)
	// MigrateTo brings an empty database's schema to version and no
	// further; a database already past it is refused.
	MigrateTo(ctx context.Context, version int) error
	// Load writes src into the database, which must be at version and hold
	// no rows of its own but those its migrations wrote: those are replaced.
	// Everything is written in one transaction, or nothing. A table src names
	// that the database does not have, or whose columns differ, is refused.
	Load(ctx context.Context, version int, src Source) error
}

// Convert returns v, a value of a column of kind from, as a value of a
// column of kind to: an int as a bool (0 or 1) or a time (microseconds
// since 1970), and back; a float from an int. Anything else is the same
// kind or an Invalid error.
func Convert(v any, from, to Kind) (any, error) {
	if v == nil || from == to {
		return v, nil
	}
	switch to {
	case KindInt:
		switch x := v.(type) {
		case bool:
			if x {
				return int64(1), nil
			}
			return int64(0), nil
		case time.Time:
			return x.UnixMicro(), nil
		}
	case KindBool:
		if x, ok := v.(int64); ok && (x == 0 || x == 1) {
			return x == 1, nil
		}
	case KindTime:
		if x, ok := v.(int64); ok {
			return time.UnixMicro(x).UTC(), nil
		}
	case KindFloat:
		if x, ok := v.(int64); ok && math.Abs(float64(x)) < 1<<53 {
			return float64(x), nil
		}
	}
	return nil, fault.New(fault.Invalid, fmt.Sprintf("snapshot: a value of a %s column cannot be written to a %s column", from, to))
}
