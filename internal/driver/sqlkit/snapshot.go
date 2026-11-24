package sqlkit

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/snapshot"
)

// A snapshot of a database (package snapshot): every table but the
// catalog's and the drivers' own bookkeeping, read in one read transaction
// as typed rows, and the load that writes one into an empty database of
// either driver. The tables and their columns are read from the database
// itself, so a table a later migration adds is in the next backup with no
// change here.

// SnapshotDialect is what a snapshot needs to know of one database type.
type SnapshotDialect struct {
	// Tables returns one column: the names of the database's tables, the
	// engine's own left out.
	Tables string
	// Columns returns the columns of the table named by its one parameter,
	// in their order: the name and the declared type.
	Columns string
	// References returns two columns for each foreign key: the table that
	// holds it and the table it refers to.
	References string
	// Kind returns the kind of a declared type, false for a type the
	// snapshot does not know.
	Kind func(declared string) (snapshot.Kind, bool)
	// Select returns the expression that reads a column of a declared type.
	Select func(column, declared string) string
	// Param returns the placeholder of the i-th value, from 1, written to a
	// column of a declared type.
	Param func(i int, declared string) string
	// Overriding is the clause an INSERT takes to write ids the database
	// would otherwise make itself, or "".
	Overriding string
	// Skip are tables of the driver's own bookkeeping, never in a snapshot:
	// what Migrate writes and checks.
	Skip []string
	// BeginRead begins the read transaction of one snapshot: every statement
	// in it reads one moment of the database.
	BeginRead func(ctx context.Context) (*sql.Tx, error)
	// BeginWrite begins the load's one write transaction.
	BeginWrite func(ctx context.Context) (*sql.Tx, error)
	// ReadSequences returns the next-id counter of each table whose ids the
	// database makes, by table.
	ReadSequences func(ctx context.Context, tx *sql.Tx) (map[string]int64, error)
	// RaiseSequences raises the counter of each table to at least its
	// value and above every id the table holds.
	RaiseSequences func(ctx context.Context, tx *sql.Tx, seqs map[string]int64) error
}

// migrationsTable is the migration runner's table, which every driver
// keeps and no snapshot holds: the schema version is the snapshot's own.
const migrationsTable = "schema_migrations"

// quoteIdent quotes a table's or a column's name as both databases read it.
func quoteIdent(name string) string { return `"` + strings.ReplaceAll(name, `"`, `""`) + `"` }

// column is a column as the database declares it.
type column struct {
	name, declared string
	kind           snapshot.Kind
}

// schemaVersion reads the version the migration runner recorded, in tx.
func schemaVersion(ctx context.Context, tx *sql.Tx) (int, error) {
	var v int
	if err := tx.QueryRowContext(ctx, `SELECT coalesce(max(version), 0) FROM `+migrationsTable).Scan(&v); err != nil {
		return 0, err
	}
	return v, nil
}

// snapshotTables returns the tables a snapshot holds, in an order in which
// they load: each after every table it refers to, by name otherwise.
func snapshotTables(ctx context.Context, tx *sql.Tx, d SnapshotDialect) ([]string, error) {
	var names []string
	if err := queryStrings(ctx, tx, d.Tables, func(s ...string) { names = append(names, s[0]) }, 1); err != nil {
		return nil, err
	}
	keep := map[string]bool{}
	for _, n := range names {
		if n == migrationsTable || slices.Contains(d.Skip, n) || snapshot.IsCatalog(n) {
			continue
		}
		keep[n] = true
	}
	refs := map[string][]string{}
	if err := queryStrings(ctx, tx, d.References, func(s ...string) {
		if s[0] != s[1] && keep[s[0]] && keep[s[1]] {
			refs[s[0]] = append(refs[s[0]], s[1])
		}
	}, 2); err != nil {
		return nil, err
	}
	return loadOrder(keep, refs)
}

// loadOrder orders the tables so each comes after those it refers to,
// taking the first by name of those ready each time.
func loadOrder(tables map[string]bool, refs map[string][]string) ([]string, error) {
	done := map[string]bool{}
	var out []string
	for len(out) < len(tables) {
		var ready []string
		for t := range tables {
			if done[t] {
				continue
			}
			ok := true
			for _, r := range refs[t] {
				if !done[r] {
					ok = false
				}
			}
			if ok {
				ready = append(ready, t)
			}
		}
		if len(ready) == 0 {
			return nil, fault.New(fault.Internal, "snapshot: the tables' foreign keys refer to each other in a circle")
		}
		slices.Sort(ready)
		done[ready[0]] = true
		out = append(out, ready[0])
	}
	return out, nil
}

// queryStrings runs q and calls fn with each row's n text columns.
func queryStrings(ctx context.Context, tx *sql.Tx, q string, fn func(...string), n int, args ...any) error {
	rows, err := tx.QueryContext(ctx, q, args...)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	vals := make([]string, n)
	ptrs := make([]any, n)
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return err
		}
		fn(vals...)
	}
	return rows.Err()
}

// tableColumns reads the columns of table.
func tableColumns(ctx context.Context, tx *sql.Tx, d SnapshotDialect, table string) ([]column, error) {
	var cols []column
	var bad string
	err := queryStrings(ctx, tx, d.Columns, func(s ...string) {
		k, ok := d.Kind(s[1])
		if !ok && bad == "" {
			bad = s[0] + " (" + s[1] + ")"
		}
		cols = append(cols, column{name: s[0], declared: s[1], kind: k})
	}, 2, table)
	if err != nil {
		return nil, err
	}
	if bad != "" {
		return nil, fault.New(fault.Internal, "snapshot: the column "+table+"."+bad+" is of a type a snapshot does not know")
	}
	if len(cols) == 0 {
		return nil, fault.New(fault.Internal, "snapshot: the table "+table+" has no columns")
	}
	return cols, nil
}

// Snapshot reads db whole into sink (snapshot.Database's Snapshot): the
// tables snapshotTables lists, each with its rows, then the sequences, all
// in one read transaction; with tenant, the point is filled from the
// tenant's own tables.
func Snapshot(ctx context.Context, d SnapshotDialect, tenant bool, sink snapshot.Sink) (snapshot.Point, error) {
	// The time is read before the transaction begins, so what it holds is
	// never older than the time says.
	point := snapshot.Point{Time: time.Now().UTC().Truncate(time.Microsecond)}
	tx, err := d.BeginRead(ctx)
	if err != nil {
		return point, fault.Wrap(fault.Unavailable, err, "the database cannot be read")
	}
	defer func() { _ = tx.Rollback() }() // Read only: nothing to keep.
	if point.Version, err = schemaVersion(ctx, tx); err != nil {
		return point, fault.Wrap(fault.Unavailable, err, "the schema version cannot be read")
	}
	tables, err := snapshotTables(ctx, tx, d)
	if err != nil {
		return point, wrapRead(err, "the list of tables")
	}
	for _, t := range tables {
		if err := snapshotTable(ctx, tx, d, t, sink); err != nil {
			return point, err
		}
	}
	seqs, err := d.ReadSequences(ctx, tx)
	if err != nil {
		return point, wrapRead(err, "the sequences")
	}
	if err := sink.Sequences(seqs); err != nil {
		return point, err
	}
	if tenant {
		if err := tenantPoint(ctx, tx, &point); err != nil {
			return point, wrapRead(err, "the recovery point")
		}
	}
	return point, nil
}

// wrapRead gives a failure to read part of a snapshot its kind, keeping
// one that has a kind already.
func wrapRead(err error, what string) error {
	if fault.KindOf(err) != fault.Internal || strings.HasPrefix(fault.Message(err), "snapshot:") {
		return err
	}
	return fault.Wrap(fault.Unavailable, err, "snapshot: "+what+" cannot be read")
}

// snapshotTable reads one table into sink.
func snapshotTable(ctx context.Context, tx *sql.Tx, d SnapshotDialect, name string, sink snapshot.Sink) error {
	cols, err := tableColumns(ctx, tx, d, name)
	if err != nil {
		return wrapRead(err, "the columns of "+name)
	}
	t := snapshot.Table{Name: name}
	sel := make([]string, len(cols))
	for i, c := range cols {
		t.Columns = append(t.Columns, snapshot.Column{Name: c.name, Kind: c.kind})
		sel[i] = d.Select(c.name, c.declared)
	}
	if err := sink.Table(t); err != nil {
		return err
	}
	//nolint:gosec // G202: the table's and columns' names are read from the database's own catalog and quoted; no value is in the text.
	rows, err := tx.QueryContext(ctx, `SELECT `+strings.Join(sel, ", ")+` FROM `+quoteIdent(name))
	if err != nil {
		return wrapRead(err, "the table "+name)
	}
	defer func() { _ = rows.Close() }()
	raw := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range raw {
		ptrs[i] = &raw[i]
	}
	values := make([]any, len(cols))
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return wrapRead(err, "a row of "+name)
		}
		for i, c := range cols {
			v, err := normalize(raw[i], c.kind)
			if err != nil {
				return fault.New(fault.Internal, "snapshot: "+name+"."+c.name+": "+err.Error())
			}
			values[i] = v
		}
		if err := sink.Row(values); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return wrapRead(err, "the table "+name)
	}
	return nil
}

// normalize returns a value a driver scanned as the one Go type its kind
// has (package snapshot).
func normalize(v any, k snapshot.Kind) (any, error) {
	if v == nil {
		return nil, nil
	}
	switch k {
	case snapshot.KindInt:
		switch x := v.(type) {
		case int64:
			return x, nil
		case int32:
			return int64(x), nil
		case int:
			return int64(x), nil
		case bool:
			return snapshot.Convert(x, snapshot.KindBool, snapshot.KindInt)
		}
	case snapshot.KindText:
		switch x := v.(type) {
		case string:
			return x, nil
		case []byte:
			return string(x), nil
		}
	case snapshot.KindBytes:
		switch x := v.(type) {
		case []byte:
			return slices.Clone(x), nil
		case string:
			return []byte(x), nil
		}
	case snapshot.KindBool:
		switch x := v.(type) {
		case bool:
			return x, nil
		case int64:
			return snapshot.Convert(x, snapshot.KindInt, snapshot.KindBool)
		}
	case snapshot.KindTime:
		if x, ok := v.(time.Time); ok {
			return x.UTC().Truncate(time.Microsecond), nil
		}
	case snapshot.KindFloat:
		switch x := v.(type) {
		case float64:
			return x, nil
		case int64:
			return float64(x), nil
		}
	}
	return nil, fmt.Errorf("a value of Go type %T in a %s column", v, k)
}

// tenantPoint fills the recovery point from a tenant's tables, in the
// snapshot's transaction.
func tenantPoint(ctx context.Context, tx *sql.Tx, p *snapshot.Point) error {
	one := func(q string, dst *string) error {
		err := tx.QueryRowContext(ctx, q).Scan(dst)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}
	if err := one(`SELECT id FROM event_history`, &p.EventHistory); err != nil {
		return err
	}
	if err := one(`SELECT id FROM corrections ORDER BY id DESC LIMIT 1`, &p.LastCorrection); err != nil {
		return err
	}
	if err := one(`SELECT id FROM receptions ORDER BY started DESC, id DESC LIMIT 1`, &p.LastReception); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT storage, generation FROM storage_owners ORDER BY storage`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var o snapshot.Owner
		if err := rows.Scan(&o.Storage, &o.Generation); err != nil {
			return err
		}
		p.Owners = append(p.Owners, o)
	}
	return rows.Err()
}
