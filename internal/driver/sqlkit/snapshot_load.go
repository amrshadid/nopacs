package sqlkit

import (
	"context"
	"database/sql"
	"slices"
	"strconv"
	"strings"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/snapshot"
)

// LoadSnapshot writes src into the database (snapshot.Database's Load), in
// one write transaction: the rows the migrations wrote into the tables a
// snapshot holds are removed first, then each table of src is written,
// then every sequence is raised past the ids src holds. The database must
// be at version. A table src names that the database lacks, or whose
// columns are not the database's, is Invalid, and nothing is written.
func LoadSnapshot(ctx context.Context, d SnapshotDialect, version int, src snapshot.Source) error {
	tx, err := d.BeginWrite(ctx)
	if err != nil {
		return fault.Wrap(fault.Unavailable, err, "the database cannot be written")
	}
	defer func() { _ = tx.Rollback() }() // A no-op after Commit.
	got, err := schemaVersion(ctx, tx)
	if err != nil {
		return fault.Wrap(fault.Unavailable, err, "the schema version cannot be read")
	}
	if got != version {
		return fault.New(fault.Internal, "snapshot: the database is at schema version "+strconv.Itoa(got)+", not "+strconv.Itoa(version))
	}
	tables, err := snapshotTables(ctx, tx, d)
	if err != nil {
		return wrapRead(err, "the list of tables")
	}
	// Removed in the reverse of the load order, so no row is removed while
	// a row that refers to it is left.
	for i := len(tables) - 1; i >= 0; i-- {
		//nolint:gosec // G202: the table's name is read from the database's own catalog and quoted.
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+quoteIdent(tables[i])); err != nil {
			return fault.Wrap(fault.Unavailable, err, "snapshot: the table "+tables[i]+" cannot be emptied")
		}
	}
	seen := map[string]bool{}
	for {
		t, ok, err := src.NextTable()
		if err != nil {
			return err
		}
		if !ok {
			break
		}
		if !slices.Contains(tables, t.Name) || seen[t.Name] {
			return fault.New(fault.Invalid, "snapshot: the table "+t.Name+" is not one this database loads, or comes twice")
		}
		seen[t.Name] = true
		if err := loadTable(ctx, tx, d, t, src); err != nil {
			return err
		}
	}
	seqs, err := src.Sequences()
	if err != nil {
		return err
	}
	if err := d.RaiseSequences(ctx, tx, seqs); err != nil {
		return fault.Wrap(fault.Unavailable, err, "snapshot: the sequences cannot be written")
	}
	if err := tx.Commit(); err != nil {
		return fault.Wrap(fault.Unavailable, err, "the snapshot cannot be committed")
	}
	return nil
}

// loadTable writes the rows of t from src, converting each value to the
// kind of the database's own column.
func loadTable(ctx context.Context, tx *sql.Tx, d SnapshotDialect, t snapshot.Table, src snapshot.Source) error {
	cols, err := tableColumns(ctx, tx, d, t.Name)
	if err != nil {
		return wrapRead(err, "the columns of "+t.Name)
	}
	if len(cols) != len(t.Columns) {
		return fault.New(fault.Invalid, "snapshot: the table "+t.Name+" has other columns here than in the snapshot")
	}
	byName := make(map[string]column, len(cols))
	for _, c := range cols {
		byName[c.name] = c
	}
	target := make([]column, len(t.Columns))
	names := make([]string, len(t.Columns))
	params := make([]string, len(t.Columns))
	for i, c := range t.Columns {
		tc, ok := byName[c.Name]
		if !ok || !c.Kind.Valid() {
			return fault.New(fault.Invalid, "snapshot: the table "+t.Name+" has other columns here than in the snapshot")
		}
		target[i] = tc
		names[i] = quoteIdent(c.Name)
		params[i] = d.Param(i+1, tc.declared)
	}
	// The table and every column are ones the database's own catalog lists
	// (checked above), quoted; the values go as parameters.
	//nolint:gosec // G202: names only, checked against the database's catalog and quoted.
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO `+quoteIdent(t.Name)+` (`+strings.Join(names, ", ")+`)`+d.Overriding+
		` VALUES (`+strings.Join(params, ", ")+`)`)
	if err != nil {
		return fault.Wrap(fault.Unavailable, err, "snapshot: the rows of "+t.Name+" cannot be written")
	}
	defer func() { _ = stmt.Close() }()
	values := make([]any, len(t.Columns))
	args := make([]any, len(t.Columns))
	for {
		ok, err := src.NextRow(values)
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
		for i, c := range t.Columns {
			if args[i], err = snapshot.Convert(values[i], c.Kind, target[i].kind); err != nil {
				return fault.Wrap(fault.Invalid, err, "snapshot: "+t.Name+"."+c.Name+" cannot be written")
			}
		}
		if _, err := stmt.ExecContext(ctx, args...); err != nil {
			return fault.Wrap(fault.Invalid, err, "snapshot: a row of "+t.Name+" cannot be written")
		}
	}
}
